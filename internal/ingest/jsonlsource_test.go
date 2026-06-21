package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/jsonl"
	"github.com/openwong2kim/wlog/internal/model"
)

// --- synthetic transcript helpers ------------------------------------------
//
// These mirror the confirmed real Claude Code transcript schema (see
// internal/jsonl/parser_test.go). The jsonl parser materializes a Session for
// any line carrying a sessionId and an APIRequest for an assistant line bearing
// a message.usage block, so a single assistant line is the minimal unit that
// produces both a session row and a child api_request row.

// jlsAssistant builds one assistant transcript line (with usage) for session
// sid at timestamp ts. ts must be a distinct RFC3339 instant per line so the
// derived dedup_key differs and rows are not collapsed by the store.
func jlsAssistant(sid, ts, mdl string, in, out int64) string {
	return `{"type":"assistant","uuid":"u-` + ts + `","timestamp":"` + ts +
		`","sessionId":"` + sid + `","cwd":"D:\\proj\\myrepo","gitBranch":"main",` +
		`"version":"2.1.143","message":{"role":"assistant","model":"` + mdl +
		`","id":"msg_x","usage":{"input_tokens":` + strconv.FormatInt(in, 10) +
		`,"output_tokens":` + strconv.FormatInt(out, 10) +
		`,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},` +
		`"content":[{"type":"text","text":"ok"}]}}`
}

// jlsWriteFile writes content to a fresh file at path (creating parent dirs).
func jlsWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// jlsAppendFile appends content to an existing file at path.
func jlsAppendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open-append %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// jlsNewSource constructs a JSONLSource feeding a freshly started Pipeline that
// writes into w. The pipeline worker is shut down via t.Cleanup. poll is large
// (we drive scan() directly), so the background ticker never fires during a test.
func jlsNewSource(t *testing.T, dir string, w model.Writer, queue int) (*JSONLSource, *Pipeline) {
	t.Helper()
	p, _ := newTestPipeline(t, w, queue)
	p.Start(context.Background())
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	s := NewJSONLSource(dir, p, jsonl.Options{}, time.Hour)
	return s, p
}

// jlsCollectAPIRequests flattens every api_request across all written batches.
func jlsCollectAPIRequests(w *fakeWriter) []model.APIRequest {
	var out []model.APIRequest
	for _, b := range w.all() {
		out = append(out, b.APIRequests...)
	}
	return out
}

// jlsSessionIDs returns the set (as a map) of session ids seen across all
// written batches.
func jlsSessionIDs(w *fakeWriter) map[string]int {
	out := map[string]int{}
	for _, b := range w.all() {
		for _, s := range b.Sessions {
			out[s.ID]++
		}
	}
	return out
}

// --- 1. historical scan -----------------------------------------------------

// TestJSONLSourceScanIngestsExistingFiles verifies the initial pass ingests
// pre-existing *.jsonl transcripts: the pipeline persists both the session row
// and the api_request rows.
func TestJSONLSourceScanIngestsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	jlsWriteFile(t, filepath.Join(dir, "sess-a.jsonl"),
		jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 6, 29)+"\n"+
			jlsAssistant("sess-a", "2026-05-18T13:00:01.000Z", "claude-opus-4-7", 1, 2)+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	ctx := context.Background()

	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 2 })

	reqs := jlsCollectAPIRequests(w)
	if len(reqs) != 2 {
		t.Fatalf("api_requests = %d, want 2", len(reqs))
	}
	for _, r := range reqs {
		if r.SessionID != "sess-a" {
			t.Errorf("api_request SessionID = %q, want sess-a", r.SessionID)
		}
	}
	if ids := jlsSessionIDs(w); ids["sess-a"] == 0 {
		t.Errorf("expected a session row for sess-a, got %v", ids)
	}
}

// --- 2. incremental watch ---------------------------------------------------

// TestJSONLSourceWatchIngestsAppendedLinesOnly verifies a second scan ingests
// only newly appended complete lines (the file offset advances; earlier lines
// are not re-submitted).
func TestJSONLSourceWatchIngestsAppendedLinesOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")
	jlsWriteFile(t, path, jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	ctx := context.Background()

	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 1 })
	offAfterFirst := s.offsets[path]
	if offAfterFirst <= 0 {
		t.Fatalf("offset did not advance after first scan: %d", offAfterFirst)
	}

	// Append one more complete line; the next scan must ingest only it.
	jlsAppendFile(t, path, jlsAssistant("sess-a", "2026-05-18T13:00:02.000Z", "m", 5, 5)+"\n")
	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 2 })

	reqs := jlsCollectAPIRequests(w)
	if len(reqs) != 2 {
		t.Fatalf("api_requests = %d, want 2 (no re-submission of the first line)", len(reqs))
	}
	if s.offsets[path] <= offAfterFirst {
		t.Fatalf("offset did not advance after append: was %d now %d", offAfterFirst, s.offsets[path])
	}
	if int64(len(mustReadFile(t, path))) != s.offsets[path] {
		t.Fatalf("offset %d should equal file size %d (file ends with newline)",
			s.offsets[path], len(mustReadFile(t, path)))
	}
}

// --- 3. partial trailing line ----------------------------------------------

// TestJSONLSourcePartialLineDeferredUntilNewline verifies a final line without
// a trailing newline (append in progress) is NOT ingested until the newline
// lands on a later scan (the upToLastNewline boundary).
func TestJSONLSourcePartialLineDeferredUntilNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")
	// First line complete; second line written WITHOUT a terminating newline.
	complete := jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "m", 1, 1) + "\n"
	partial := jlsAssistant("sess-a", "2026-05-18T13:00:02.000Z", "m", 9, 9) // no "\n"
	jlsWriteFile(t, path, complete+partial)

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	ctx := context.Background()

	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 1 })
	if got := len(jlsCollectAPIRequests(w)); got != 1 {
		t.Fatalf("api_requests = %d, want 1 (partial line must be deferred)", got)
	}
	// Offset must stop at the end of the first (complete) line, not into the partial.
	if want := int64(len(complete)); s.offsets[path] != want {
		t.Fatalf("offset = %d, want %d (end of first complete line)", s.offsets[path], want)
	}

	// Complete the partial line by appending the missing newline.
	jlsAppendFile(t, path, "\n")
	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 2 })
	if got := len(jlsCollectAPIRequests(w)); got != 2 {
		t.Fatalf("api_requests = %d, want 2 after newline completes the line", got)
	}
}

// --- 3b. tool_use / tool_result pairing across a poll boundary (P2) ---------

// jlsAssistantToolUse builds an assistant line carrying a Bash tool_use block
// (and usage, so it also yields an api_request). The tool_result that completes
// it arrives on a separate user line (jlsUserToolResult), possibly on a later
// poll.
func jlsAssistantToolUse(sid, ts, toolUseID, command string) string {
	content := `[{"type":"tool_use","id":"` + toolUseID + `","name":"Bash","input":{"command":"` + command + `"}}]`
	return `{"type":"assistant","uuid":"u-` + ts + `","timestamp":"` + ts +
		`","sessionId":"` + sid + `","cwd":"D:\\proj\\myrepo","gitBranch":"main",` +
		`"version":"2.1.143","message":{"role":"assistant","model":"claude-opus-4-7",` +
		`"id":"msg_x","usage":{"input_tokens":1,"output_tokens":1,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":` + content + `}}`
}

// jlsUserToolResult builds a user line carrying the tool_result that completes a
// prior tool_use (matched by toolUseID).
func jlsUserToolResult(sid, ts, toolUseID string) string {
	return `{"type":"user","uuid":"ur-` + ts + `","timestamp":"` + ts +
		`","sessionId":"` + sid + `","cwd":"D:\\proj\\myrepo","gitBranch":"main",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolUseID +
		`","is_error":false,"content":"PASS"}]}}`
}

// jlsCollectToolResults flattens every tool_result across all written batches.
func jlsCollectToolResults(w *fakeWriter) []model.ToolResult {
	var out []model.ToolResult
	for _, b := range w.all() {
		out = append(out, b.ToolResults...)
	}
	return out
}

// TestJSONLSourcePendingToolPairedAcrossPolls is the P2 pending-tool regression:
// an assistant tool_use lands on poll 1 and its matching user tool_result only on
// poll 2. The source must NOT advance its offset past the open tool_use, so the
// next poll re-reads the tool_use TOGETHER with the result and the emitted
// ToolResult carries the tool name + bash command (it would otherwise be parsed
// in isolation and lose both). Re-parsing the rewound region is idempotent.
func TestJSONLSourcePendingToolPairedAcrossPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")
	// Poll 1: only the assistant tool_use is present (no result yet).
	jlsWriteFile(t, path, jlsAssistantToolUse("sess-a", "2026-05-18T13:00:00.000Z", "toolu_1", "go test ./...")+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	ctx := context.Background()

	s.scan(ctx)
	// The unpaired tool_use is the FIRST unconsumed line, so the whole region is
	// deferred (like a partial trailing line): nothing is submitted yet and the
	// offset stays at 0. Deferring (rather than emitting the lone tool_use) is what
	// lets the result re-join it on the next poll.
	time.Sleep(50 * time.Millisecond)
	if got := len(jlsCollectToolResults(w)); got != 0 {
		t.Fatalf("ToolResults = %d, want 0 before the result line lands", got)
	}
	if got := len(jlsCollectAPIRequests(w)); got != 0 {
		t.Fatalf("api_requests = %d, want 0 (region deferred behind the open tool_use)", got)
	}
	// The offset must be held BEFORE the open tool_use line (nothing safe to
	// consume), so it has not advanced to end-of-file.
	if off := s.offsets[path]; off != 0 {
		t.Fatalf("offset = %d, want 0 (held before the unpaired tool_use)", off)
	}

	// Poll 2: the matching tool_result is appended.
	jlsAppendFile(t, path, jlsUserToolResult("sess-a", "2026-05-18T13:00:01.000Z", "toolu_1")+"\n")
	s.scan(ctx)
	waitFor(t, 2*time.Second, func() bool { return len(jlsCollectToolResults(w)) == 1 })

	trs := jlsCollectToolResults(w)
	if len(trs) != 1 {
		t.Fatalf("ToolResults = %d, want 1 after the result line lands", len(trs))
	}
	tr := trs[0]
	if tr.ToolName != "Bash" {
		t.Errorf("ToolResult.ToolName = %q, want Bash (recovered from the re-read tool_use)", tr.ToolName)
	}
	if tr.BashCommand != "go test ./..." {
		t.Errorf("ToolResult.BashCommand = %q, want %q (paired across the poll boundary)", tr.BashCommand, "go test ./...")
	}
	if !tr.Success {
		t.Errorf("ToolResult.Success = false, want true (is_error=false)")
	}
	// The assistant api_request (deferred on poll 1) now surfaces too, exactly once.
	if got := len(jlsCollectAPIRequests(w)); got != 1 {
		t.Errorf("api_requests = %d, want 1 (the deferred assistant request, no duplicate)", got)
	}
	// Offset now advances to end-of-file (the pair is complete; nothing pending).
	if want := int64(len(mustReadFile(t, path))); s.offsets[path] != want {
		t.Errorf("offset after pairing = %d, want %d (end of file)", s.offsets[path], want)
	}
}

// TestJSONLSourceHoldbackBoundedForAbandonedToolUse verifies the holdback does
// not stall ingestion forever when a tool_use never receives a result (an
// interrupted turn): once more than maxPendingHoldbackBytes of newer complete
// data has piled up after the open tool_use, the source consumes through it (the
// orphan tool_use is dropped, its api_request stands) and the offset advances.
func TestJSONLSourceHoldbackBoundedForAbandonedToolUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")

	// An open tool_use FIRST, then enough later complete lines to exceed the cap so
	// the abandoned tool_use cannot hold the offset hostage.
	var sb strings.Builder
	sb.WriteString(jlsAssistantToolUse("sess-a", "2026-05-18T13:00:00.000Z", "toolu_abandoned", "sleep 999") + "\n")
	filler := jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "m", 1, 1) // reused content, distinct ts below
	// Each filler line is a few hundred bytes; write enough to exceed 1MiB.
	n := (maxPendingHoldbackBytes / len(filler)) + 8
	for i := 0; i < n; i++ {
		ts := "2026-05-18T13:10:" + twoDigit(i%60) + "." + threeDigit(i%1000) + "Z"
		sb.WriteString(jlsAssistant("sess-a", ts, "m", int64(i+2), int64(i+2)) + "\n")
	}
	jlsWriteFile(t, path, sb.String())

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 4096)
	ctx := context.Background()

	s.scan(ctx)
	// Progress must happen despite the unpaired tool_use: the offset advances to
	// end-of-file (the cap was exceeded, so the orphan is consumed through).
	waitFor(t, 3*time.Second, func() bool {
		return s.offsets[path] == int64(len(mustReadFile(t, path)))
	})
	if off, want := s.offsets[path], int64(len(mustReadFile(t, path))); off != want {
		t.Fatalf("offset = %d, want %d (abandoned tool_use must not stall ingestion)", off, want)
	}
	// Many api_requests were ingested (all the assistant lines), proving forward
	// progress; the orphan tool_use produced no ToolResult.
	if got := len(jlsCollectAPIRequests(w)); got < n {
		t.Errorf("api_requests = %d, want >= %d (forward progress past the orphan)", got, n)
	}
}

// twoDigit / threeDigit zero-pad small ints for synthetic RFC3339 timestamps.
func twoDigit(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
func threeDigit(n int) string {
	switch {
	case n < 10:
		return "00" + strconv.Itoa(n)
	case n < 100:
		return "0" + strconv.Itoa(n)
	default:
		return strconv.Itoa(n)
	}
}

// --- 4. truncate / rotate ---------------------------------------------------

// TestJSONLSourceTruncateRereadsFromStart verifies that when a file shrinks
// below the recorded offset (truncation/rotation), the source re-reads from the
// beginning rather than skipping content.
func TestJSONLSourceTruncateRereadsFromStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")
	jlsWriteFile(t, path,
		jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n"+
			jlsAssistant("sess-a", "2026-05-18T13:00:01.000Z", "m", 2, 2)+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	ctx := context.Background()

	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 2 })
	if s.offsets[path] == 0 {
		t.Fatalf("offset should be > 0 after initial ingest")
	}

	// Rotate: replace with a SHORTER single-line file (new content, smaller size).
	jlsWriteFile(t, path, jlsAssistant("sess-a", "2026-05-18T14:00:00.000Z", "m", 7, 7)+"\n")
	if int64(len(mustReadFile(t, path))) >= s.offsets[path] {
		t.Fatalf("test setup: rotated file must be smaller than the prior offset")
	}

	s.scan(ctx)
	// The single line of the rotated file must be ingested (re-read from start).
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 3 })
	if got := len(jlsCollectAPIRequests(w)); got != 3 {
		t.Fatalf("api_requests = %d, want 3 (2 original + 1 after rotate)", got)
	}
	// Offset resets to the size of the (smaller) rotated file.
	if want := int64(len(mustReadFile(t, path))); s.offsets[path] != want {
		t.Fatalf("offset after rotate = %d, want %d", s.offsets[path], want)
	}
}

// --- 5. missing directory ---------------------------------------------------

// TestJSONLSourceMissingDirSilentlySkipped verifies a scan over a non-existent
// directory returns quietly (no panic, no submission) — OTLP-only operation is
// fully supported.
func TestJSONLSourceMissingDirSilentlySkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)

	// Must not panic.
	s.scan(context.Background())
	// Give any (erroneous) async write a chance to land, then assert none did.
	time.Sleep(20 * time.Millisecond)
	if n := w.total(); n != 0 {
		t.Fatalf("expected no writes for a missing dir, got %d", n)
	}

	// A regular FILE (not a directory) passed as dir must also be skipped cleanly.
	fileAsDir := filepath.Join(t.TempDir(), "afile")
	jlsWriteFile(t, fileAsDir, "not a dir\n")
	s2 := NewJSONLSource(fileAsDir, s.pipe, jsonl.Options{}, time.Hour)
	s2.scan(context.Background())
	time.Sleep(20 * time.Millisecond)
	if n := w.total(); n != 0 {
		t.Fatalf("expected no writes when dir is a regular file, got %d", n)
	}
}

// --- 6. dedup / idempotent re-scan -----------------------------------------

// TestJSONLSourceRescanUnchangedNoResubmit verifies scanning an unchanged file
// twice does not re-submit: the offset is unchanged and no extra write occurs
// (size == off short-circuit; dedup would also make it harmless).
func TestJSONLSourceRescanUnchangedNoResubmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")
	jlsWriteFile(t, path, jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	ctx := context.Background()

	s.scan(ctx)
	waitFor(t, time.Second, func() bool { return w.total() == 1 })
	offAfter := s.offsets[path]
	writesAfter := w.total()

	// Second scan with no change: nothing new should be submitted.
	s.scan(ctx)
	time.Sleep(30 * time.Millisecond)
	if w.total() != writesAfter {
		t.Fatalf("unchanged re-scan submitted again: writes %d -> %d", writesAfter, w.total())
	}
	if s.offsets[path] != offAfter {
		t.Fatalf("offset changed on unchanged re-scan: %d -> %d", offAfter, s.offsets[path])
	}
}

// --- 7. non-jsonl files ignored --------------------------------------------

// TestJSONLSourceIgnoresNonJSONLFiles verifies files without a .jsonl suffix
// are not scanned, while .JSONL (mixed case) IS scanned (isJSONLName is
// case-insensitive).
func TestJSONLSourceIgnoresNonJSONLFiles(t *testing.T) {
	dir := t.TempDir()
	// Decoy non-transcript files that must be ignored.
	jlsWriteFile(t, filepath.Join(dir, "notes.txt"),
		jlsAssistant("decoy", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")
	jlsWriteFile(t, filepath.Join(dir, "data.json"),
		jlsAssistant("decoy2", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")
	jlsWriteFile(t, filepath.Join(dir, "sess.jsonl.bak"),
		jlsAssistant("decoy3", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")
	// A real transcript with an upper-case extension (must be picked up).
	jlsWriteFile(t, filepath.Join(dir, "real.JSONL"),
		jlsAssistant("real", "2026-05-18T13:00:00.000Z", "m", 3, 3)+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	s.scan(context.Background())
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 1 })

	ids := jlsSessionIDs(w)
	if ids["real"] == 0 {
		t.Errorf("real.JSONL (mixed case) was not ingested: %v", ids)
	}
	for _, decoy := range []string{"decoy", "decoy2", "decoy3"} {
		if ids[decoy] != 0 {
			t.Errorf("non-jsonl decoy %q was ingested: %v", decoy, ids)
		}
	}
}

// --- 8. recursive subdirectories -------------------------------------------

// TestJSONLSourceRecursesSubdirectories verifies WalkDir discovers transcripts
// nested under subdirectories (the real layout is
// <dir>/<enc-cwd>/<session>.jsonl).
func TestJSONLSourceRecursesSubdirectories(t *testing.T) {
	dir := t.TempDir()
	jlsWriteFile(t, filepath.Join(dir, "proj-a", "sess-1.jsonl"),
		jlsAssistant("sess-1", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")
	jlsWriteFile(t, filepath.Join(dir, "proj-b", "nested", "sess-2.jsonl"),
		jlsAssistant("sess-2", "2026-05-18T13:00:00.000Z", "m", 2, 2)+"\n")

	w := &fakeWriter{}
	s, _ := jlsNewSource(t, dir, w, 64)
	s.scan(context.Background())
	waitFor(t, time.Second, func() bool { return len(jlsCollectAPIRequests(w)) == 2 })

	ids := jlsSessionIDs(w)
	if ids["sess-1"] == 0 || ids["sess-2"] == 0 {
		t.Fatalf("nested transcripts not all discovered: %v", ids)
	}
}

// --- 9. ErrBusy backoff -----------------------------------------------------

// jlsBlockingWriter parks the worker inside the FIRST WriteBatch until gate is
// closed, signalling entry via entered. This lets a test deterministically pin
// the worker (so the queue stays full) without the race of polling Received,
// which increments at enqueue time rather than dequeue time.
type jlsBlockingWriter struct {
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (w *jlsBlockingWriter) WriteBatch(ctx context.Context, _ model.Batch) error {
	w.once.Do(func() {
		close(w.entered)
		<-w.gate // block only the first write
	})
	return ctx.Err()
}

// TestJSONLSourceSubmitRetriesOnBusy verifies submit() backs off and retries
// when the pipeline queue is momentarily full (ErrBusy), eventually delivering
// the batch and advancing the offset (no data loss). A cap-1 queue plus a writer
// pinned on its first call holds the queue full until we release it.
func TestJSONLSourceSubmitRetriesOnBusy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-a.jsonl")
	jlsWriteFile(t, path, jlsAssistant("sess-a", "2026-05-18T13:00:00.000Z", "m", 1, 1)+"\n")

	w := &jlsBlockingWriter{gate: make(chan struct{}), entered: make(chan struct{})}
	p, _ := newTestPipeline(t, w, 1)
	p.Start(context.Background())
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(w.gate) }) }
	t.Cleanup(func() {
		release()
		_ = p.Shutdown(context.Background())
	})
	s := NewJSONLSource(dir, p, jsonl.Options{}, time.Hour)
	ctx := context.Background()

	// Pin the worker: submit one item the worker dequeues and blocks on. Waiting
	// for `entered` guarantees the item has LEFT the queue (slot now free) and the
	// worker will not consume another item until we release the gate.
	if err := p.SubmitBatch(sampleBatch("pin", "m", 1, 1, 0)); err != nil {
		t.Fatalf("pin submit: %v", err)
	}
	<-w.entered

	// Fill the single free slot so the queue is now provably full; a probe submit
	// must be rejected as ErrBusy. (The worker is parked, so it cannot drain it.)
	if err := p.SubmitBatch(sampleBatch("fill", "m", 1, 1, 0)); err != nil {
		t.Fatalf("fill submit: %v", err)
	}
	if got := p.SubmitBatch(sampleBatch("probe", "m", 1, 1, 0)); got != ErrBusy {
		t.Fatalf("probe submit = %v, want ErrBusy (queue should be full)", got)
	}

	// Run ingestFile on its own goroutine: submit() must spin in the busy backoff
	// loop because the queue is saturated and the worker is pinned.
	done := make(chan struct{})
	go func() {
		s.ingestFile(ctx, path)
		close(done)
	}()

	// While the queue is full the source must keep retrying, not return or advance.
	time.Sleep(3 * busyBackoff)
	select {
	case <-done:
		t.Fatalf("ingestFile returned while queue was full; submit should be backing off")
	default:
	}
	if s.offsets[path] != 0 {
		t.Fatalf("offset advanced before submit succeeded: %d", s.offsets[path])
	}

	// Release the worker: the queue drains, the retried submit is accepted, and
	// ingestFile finishes with the offset advanced past the consumed line.
	release()
	waitFor(t, 2*time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if s.offsets[path] != int64(len(mustReadFile(t, path))) {
		t.Fatalf("offset = %d, want %d after busy-retry succeeded",
			s.offsets[path], len(mustReadFile(t, path)))
	}
	if p.Counters().RetrySignaled == 0 {
		t.Fatalf("expected RetrySignaled > 0 (the busy probe/retries)")
	}
}

// --- 10. pure functions -----------------------------------------------------

func TestUpToLastNewline(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantComplete string
		wantConsumed int
	}{
		{"empty", "", "", 0},
		{"no newline at all", "abc", "", 0},
		{"single line with newline", "abc\n", "abc\n", 4},
		{"two complete lines", "a\nb\n", "a\nb\n", 4},
		{"trailing partial after newline", "a\nb\npart", "a\nb\n", 4},
		{"only a newline", "\n", "\n", 1},
		{"partial then nothing", "x", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			complete, consumed := upToLastNewline([]byte(tc.in))
			if string(complete) != tc.wantComplete {
				t.Errorf("complete = %q, want %q", string(complete), tc.wantComplete)
			}
			if consumed != tc.wantConsumed {
				t.Errorf("consumed = %d, want %d", consumed, tc.wantConsumed)
			}
			// Invariant: consumed == len(complete).
			if consumed != len(complete) {
				t.Errorf("consumed %d != len(complete) %d", consumed, len(complete))
			}
		})
	}
}

func TestIsJSONLName(t *testing.T) {
	cases := map[string]bool{
		"a.jsonl":            true,
		"A.JSONL":            true,
		"session.JsOnL":      true,
		".jsonl":             true, // exactly the suffix
		"a.jsonl.bak":        false,
		"a.json":             false,
		"jsonl":              false, // no dot, too short / wrong
		"":                   false,
		"short":              false,
		"x.jsonlx":           false,
		"deep.name.v2.jsonl": true,
	}
	for name, want := range cases {
		if got := isJSONLName(name); got != want {
			t.Errorf("isJSONLName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBatchEmpty(t *testing.T) {
	if !batchEmpty(model.Batch{}) {
		t.Errorf("zero Batch should be empty")
	}
	if batchEmpty(model.Batch{Sessions: []model.Session{{ID: "s"}}}) {
		t.Errorf("a batch with a Session must NOT be empty (records first/last seen)")
	}
	if batchEmpty(model.Batch{APIRequests: []model.APIRequest{{SessionID: "s"}}}) {
		t.Errorf("a batch with an APIRequest must NOT be empty")
	}
	if batchEmpty(model.Batch{Events: []model.Event{{Name: "x"}}}) {
		t.Errorf("a batch with an Event must NOT be empty")
	}
	// SeriesStates alone is NOT counted as content by batchEmpty (it carries no
	// telemetry rows), so a batch with only SeriesStates is still "empty".
	if !batchEmpty(model.Batch{SeriesStates: []model.SeriesState{{Key: "k"}}}) {
		t.Errorf("a batch with only SeriesStates should be treated as empty")
	}
}

// --- shared small helper ----------------------------------------------------

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

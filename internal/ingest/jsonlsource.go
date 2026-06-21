package ingest

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/openwong2kim/wlog/internal/jsonl"
	"github.com/openwong2kim/wlog/internal/model"
)

// JSONLSource ingests Claude Code transcript JSONL files
// (~/.claude/projects/<enc-cwd>/<session-uuid>.jsonl) into the same pipeline as
// OTLP, giving wlog zero-config history the moment it is installed (PLAN v3
// feature 0). It does an initial recursive scan of every existing *.jsonl file
// and then polls for growth on a fixed interval, re-parsing only newly appended
// lines. Re-ingesting a line is harmless: the jsonl parser produces the same
// dedup_key as before (and as the OTLP path), so the store collapses duplicates
// to one row (EQ6).
//
// It uses only the standard library — a fixed-interval mtime/size poll, no
// fsnotify — to honour wlog's zero-extra-dependency rule (PLAN deps minimum).
//
// All file reads happen on the source's own goroutine; the only shared edge is
// pipeline.SubmitBatch, which is safe for concurrent callers and back-pressures
// via ErrBusy (the source retries rather than dropping data).
type JSONLSource struct {
	dir  string
	pipe *Pipeline
	opts jsonl.Options
	poll time.Duration

	// offsets tracks, per file, the byte offset up to which we have already
	// parsed COMPLETE lines. A partial final line (file still being appended) is
	// never counted, so it is re-read on the next poll once the newline lands.
	// Owned exclusively by Run's goroutine (no lock needed).
	offsets map[string]int64
}

// Default polling cadence for the JSONL watch. A few seconds is responsive
// enough for a developer cockpit while keeping idle CPU negligible.
const defaultJSONLPoll = 3 * time.Second

// busyBackoff is how long the source waits before retrying a SubmitBatch that
// returned ErrBusy (the pipeline queue was momentarily full).
const busyBackoff = 50 * time.Millisecond

// NewJSONLSource constructs a source that reads transcripts under dir and feeds
// them to p. A poll <= 0 uses defaultJSONLPoll. opts mirrors the OTLP parser's
// redaction option so --no-store-prompts applies to JSONL too.
func NewJSONLSource(dir string, p *Pipeline, opts jsonl.Options, poll time.Duration) *JSONLSource {
	if poll <= 0 {
		poll = defaultJSONLPoll
	}
	return &JSONLSource{
		dir:     dir,
		pipe:    p,
		opts:    opts,
		poll:    poll,
		offsets: make(map[string]int64),
	}
}

// Run performs the initial historical scan and then watches for appended lines
// until ctx is cancelled. It returns ctx.Err() on cancellation (nil-equivalent
// for callers that ignore it). It is non-fatal by design: a missing directory,
// an unreadable file, or a transient busy pipeline are all tolerated and retried
// on the next tick — wlog must keep serving OTLP even if no transcripts exist.
func (s *JSONLSource) Run(ctx context.Context) error {
	// Initial pass: ingest all existing files (catch up on history).
	s.scan(ctx)

	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.scan(ctx)
		}
	}
}

// scan walks dir for *.jsonl files and ingests any new content in each. A
// directory that does not exist (Claude Code not installed yet, or a custom
// --claude-dir that has not appeared) is silently skipped: the next tick retries
// in case it shows up later. Per-file errors are skipped so one bad file does
// not stall the others.
func (s *JSONLSource) scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Skip cleanly when the directory is absent. We do not treat this as an error
	// (OTLP-only operation is fully supported, PLAN v3 §9 Failure Modes).
	if fi, err := os.Stat(s.dir); err != nil || !fi.IsDir() {
		return
	}

	_ = filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			// Unreadable directory entry: skip it (and its subtree if it is a dir)
			// rather than aborting the whole walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !isJSONLName(d.Name()) {
			return nil
		}
		s.ingestFile(ctx, path)
		return nil
	})
}

// isJSONLName reports whether name is a transcript file (case-insensitive
// ".jsonl" suffix). filepath separators are irrelevant — WalkDir passes a base
// name here.
func isJSONLName(name string) bool {
	const ext = ".jsonl"
	if len(name) < len(ext) {
		return false
	}
	return equalFoldASCII(name[len(name)-len(ext):], ext)
}

// equalFoldASCII compares two equal-length ASCII strings case-insensitively
// (avoids strings.EqualFold's unicode machinery for a hot, ASCII-only check).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ingestFile reads the not-yet-seen tail of one transcript file, parses it, and
// submits the resulting batch. It advances the file's offset only past the last
// COMPLETE line, so a partial final line (append in progress) is re-read next
// tick. A truncated/rotated file (size shrank below our offset) is re-read from
// the start.
func (s *JSONLSource) ingestFile(ctx context.Context, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	size := fi.Size()
	off := s.offsets[path]

	switch {
	case size == off:
		return // unchanged since last read
	case size < off:
		off = 0 // truncated or rotated: start over (dedup keeps this idempotent)
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return
		}
	}

	// Read the appended region into memory, then trim to the last newline so a
	// half-written final line is excluded (and re-read once completed). Per-file
	// processing bounds memory to one file's unread tail at a time.
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	complete, consumed := upToLastNewline(data)
	if len(complete) == 0 {
		// No complete line yet (only a partial trailing line). Leave the offset so
		// the partial line is re-read next tick.
		return
	}

	batch, stats, perr := jsonl.Parse(bytes.NewReader(complete), s.opts)
	if perr != nil {
		// A read error on an in-memory reader is not expected; if it happens, do
		// not advance the offset so the region is retried next tick.
		return
	}

	// An assistant tool_use whose matching tool_result has not arrived yet leaves a
	// pending entry; the parser reports the byte offset of the FIRST such line
	// (Stats.PendingByteOffset, -1 when none). Hold the file offset just before it
	// so the next poll re-reads that tool_use TOGETHER with the tool_result that is
	// appended after this poll boundary — otherwise the result would be parsed in
	// isolation and lose its tool name / bash command (P2 fix). Re-parsing the
	// rewound region is idempotent (dedup_key), the same philosophy as deferring a
	// partial trailing line.
	consumed = s.holdbackForPendingTool(consumed, stats.PendingByteOffset)
	if consumed == 0 {
		// The earliest unpaired tool_use is the very first line of this region, so
		// there is nothing safe to consume yet: defer the whole region (offset
		// stays put) until the tool_result is appended on a later poll, exactly as
		// a partial trailing line is deferred. We do NOT submit here — submitting
		// would re-deliver the same batch every poll while the session is idle. The
		// turn's api_request is surfaced as soon as the pair completes (next poll
		// re-reads from here) or the holdback cap below forces progress. Bounded by
		// maxPendingHoldbackBytes so an abandoned tool_use cannot stall forever.
		return
	}

	if !batchEmpty(batch) {
		if err := s.submit(ctx, batch); err != nil {
			// ctx cancelled or pipeline stopped: do NOT advance the offset, so the
			// data is re-read after restart / on the next live tick (no loss).
			return
		}
	}

	// Advance only past the bytes we fully consumed (complete lines, capped to the
	// pending-tool holdback above).
	s.offsets[path] = off + int64(consumed)
}

// maxPendingHoldbackBytes bounds how far back the source rewinds to keep an
// unpaired tool_use with its (future) tool_result. A tool_use that straddles the
// CURRENT poll boundary is the trailing part of the just-read region, so its
// holdback is tiny. But a tool_use that never receives a result (an interrupted
// turn) must not stall ingestion of everything after it forever: once more than
// this many bytes have accumulated past the open tool_use, we treat it as
// abandoned and consume through it (the orphan tool_use is dropped, exactly as
// before this fix; the assistant api_request was already emitted). 1MB is far
// larger than any normal turn yet bounds the worst-case stall.
const maxPendingHoldbackBytes = 1 << 20

// holdbackForPendingTool reduces consumed so the file offset stops just before
// the earliest unpaired tool_use line (pendingByteOffset). It returns consumed
// unchanged when there is no pending tool_use (pendingByteOffset < 0) or when the
// region held back would exceed maxPendingHoldbackBytes (the tool_use is treated
// as abandoned, guaranteeing forward progress).
func (s *JSONLSource) holdbackForPendingTool(consumed int, pendingByteOffset int64) int {
	if pendingByteOffset < 0 || pendingByteOffset >= int64(consumed) {
		return consumed // nothing pending, or it is beyond the consumable region
	}
	if int64(consumed)-pendingByteOffset > maxPendingHoldbackBytes {
		// Too much data has piled up after the open tool_use: it is almost certainly
		// abandoned (no result will come). Consume through it to avoid a permanent
		// stall; the orphan tool_use is dropped (the assistant's api_request stands).
		return consumed
	}
	return int(pendingByteOffset)
}

// submit pushes a batch to the pipeline, retrying on ErrBusy with a short
// backoff (the pipeline queue was momentarily full). It returns an error only
// when ctx is cancelled or the pipeline has been shut down, so the caller knows
// the batch was NOT accepted and must not advance the file offset.
func (s *JSONLSource) submit(ctx context.Context, b model.Batch) error {
	for {
		err := s.pipe.SubmitBatch(b)
		if err == nil {
			return nil
		}
		if err == ErrBusy {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(busyBackoff):
				continue
			}
		}
		// errStopped (pipeline shutting down) or any other error: surface it so the
		// offset is not advanced.
		return err
	}
}

// upToLastNewline splits data at the last '\n', returning the leading slice of
// complete lines (including that final newline) and the number of bytes
// consumed. When data has no newline at all, it returns an empty slice and 0
// (the whole buffer is a partial line, to be re-read once a newline arrives).
func upToLastNewline(data []byte) (complete []byte, consumed int) {
	i := bytes.LastIndexByte(data, '\n')
	if i < 0 {
		return nil, 0
	}
	return data[:i+1], i + 1
}

// batchEmpty reports whether a parsed batch carries no telemetry worth writing.
// The jsonl parser always materializes a Session row for any line bearing a
// sessionId (even pure-metadata lines), so a batch with only Sessions and no
// child records is still worth a write — it records first/last seen and
// repo/branch. Thus only a fully empty batch is skipped.
func batchEmpty(b model.Batch) bool {
	return len(b.Sessions) == 0 &&
		len(b.APIRequests) == 0 &&
		len(b.ToolResults) == 0 &&
		len(b.ToolDecisions) == 0 &&
		len(b.Events) == 0 &&
		len(b.Spans) == 0 &&
		len(b.MetricPoints) == 0
}

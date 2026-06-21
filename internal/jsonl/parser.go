// Package jsonl converts Claude Code transcript JSONL (the per-session
// ~/.claude/projects/<enc-cwd>/<session-uuid>.jsonl files) into the wlog domain
// model (internal/model). It is the zero-config ingestion path (PLAN v3 feature
// 0): a transcript is read directly so installed-immediately history shows
// cost/tool/repo data without enabling OTLP.
//
// It is a separate parser from internal/otel (OTLP), but its output is the same
// model.Batch, so the rest of the pipeline (writer, dedup, bus) is reused
// unchanged. Where the two sources describe the SAME logical call they produce
// the SAME dedup_key (via the shared model.*DedupKey constructors) so the store
// collapses them to one row — the EQ6 cost-accuracy gate (PLAN v3 §9).
//
// Schema (confirmed against real ~/.claude/projects transcripts):
//   - Each line is one JSON object with a top-level "type".
//   - type=assistant carries message.{model,usage{...}} → one APIRequest, and
//     message.content[] may contain {type:tool_use, id, name, input{command}}.
//   - type=user may carry message.content[] {type:tool_result, tool_use_id,
//     is_error, content} (the result of an earlier tool_use) and/or a plain
//     prompt string → a user_prompt Event.
//   - type=summary is a conversation summary (no telemetry) → skipped.
//   - Other types (mode, permission-mode, file-history-snapshot, attachment,
//     last-prompt, ...) carry no telemetry → skipped.
//   - tool_decision (accept/reject × source) does NOT appear in the transcript
//     (it is OTLP-only; PLAN v3 §9 EQ5).
//   - Common envelope: timestamp (RFC3339 ms), sessionId, cwd, gitBranch,
//     version, userType. cwd → Session.Repo (project dir name); gitBranch →
//     Session.GitBranch.
//
// Unknown fields are preserved in attrs_json (same principle as the OTLP
// parser, PLAN §6): the known keys are mapped to columns, the rest survive in
// the raw attribute blob for forward-compatibility across Claude Code versions.
package jsonl

import (
	"bufio"
	"encoding/json"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/pricing"
)

// maskedValue replaces sensitive field content when prompt storage is disabled.
// Mirrors the otel parser's redaction sentinel so masked output is identical
// across sources.
const maskedValue = "[redacted]"

// mcpPrefix marks Claude Code MCP tool names: mcp__<server>__<tool>. The server
// segment is extracted into ToolResult.MCPServer.
const mcpPrefix = "mcp__"

// bashToolName is the built-in shell tool whose input.command is the bash
// command surfaced into ToolResult.BashCommand.
const bashToolName = "Bash"

// Options controls parsing behaviour. NoStorePrompts mirrors the otel parser's
// --no-store-prompts: sensitive free-text (prompt) and shell-command content is
// redacted before it reaches the batch, so the raw payload never persists.
type Options struct {
	// NoStorePrompts redacts prompt text and bash/tool command content.
	NoStorePrompts bool
}

// Stats reports what a parse pass observed. Skipped is incremented for lines
// that carry no telemetry OR cannot be decoded (incomplete/corrupt JSON — e.g. a
// final line still being appended by Claude Code). Nothing is dropped silently:
// a caller can surface Skipped/Malformed (PLAN v3 §9 "조용한 손실 금지").
type Stats struct {
	Lines     int // total non-empty lines seen
	Decoded   int // lines that decoded as JSON objects
	Malformed int // lines that failed JSON decode (incomplete/corrupt)
	Mapped    int // lines that produced at least one model record

	// PendingByteOffset is the byte offset (into the parsed stream) of the FIRST
	// assistant line whose tool_use never received a matching tool_result in this
	// pass — i.e. the safe point a streaming caller must rewind to so the next
	// pass re-reads that tool_use together with the tool_result that will arrive
	// on a later append (the tool_use↔tool_result pair otherwise straddles the
	// poll boundary and the result loses its tool name / bash command). It is -1
	// when every tool_use was paired (the caller may consume the whole buffer).
	// Re-parsing the rewound region is idempotent (dedup_key), matching the same
	// philosophy as deferring a partial trailing line. (PLAN v3 §9 EQ6.)
	PendingByteOffset int64
}

// Parse reads a Claude Code transcript stream (one JSON object per line) and
// returns the accumulated model.Batch plus parse Stats.
//
// Parsing is whole-stream, not strictly line-independent, because a tool's
// identity (name, bash command) lives in the assistant tool_use block while its
// success/failure arrives later in a user tool_result block keyed by
// tool_use_id. Parse buffers open tool_use calls and emits a model.ToolResult
// when the matching tool_result line is seen, joining the two.
//
// Empty and undecodable lines are skipped and counted (Stats.Malformed); a
// truncated final line (append-in-progress) is therefore tolerated and will be
// re-read on the next scan. Parse only returns a non-nil error for a read
// failure on the underlying stream (not for malformed lines).
func Parse(r io.Reader, opts Options) (model.Batch, Stats, error) {
	p := &parser{
		opts:     opts,
		sessions: newSessionAccumulator(),
		pending:  make(map[string]*pendingTool),
	}

	// Read line-by-line with bufio.Reader.ReadBytes (not bufio.Scanner) so we can
	// track the exact byte offset of each line's START in the stream: ReadBytes
	// returns the line INCLUDING its '\n' delimiter, so summing the consumed bytes
	// gives an offset that maps back precisely to the caller's buffer. This backs
	// Stats.PendingByteOffset (the rewind point for an unfinished tool_use that
	// straddles a poll boundary). A Scanner strips the delimiter, which would make
	// the offset arithmetic fragile (CRLF, final-line variants).
	br := bufio.NewReaderSize(r, 256*1024)
	var offset int64 // byte offset of the NEXT line's first byte
	for {
		line, err := br.ReadBytes('\n')
		lineStart := offset
		offset += int64(len(line))
		// Process a non-empty line (the final chunk before EOF may lack a newline;
		// the streaming caller strips partial trailing lines, so in practice every
		// line here is newline-terminated, but we still handle a trailing fragment).
		if len(strings.TrimSpace(string(line))) > 0 {
			p.stats.Lines++
			p.parseLine(line, lineStart)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			// A genuine read failure (not a per-line decode error). Return what we
			// accumulated so far together with the error.
			p.finish()
			return p.batch, p.stats, err
		}
	}

	p.finish()
	return p.batch, p.stats, nil
}

// parser holds the per-stream accumulation state.
type parser struct {
	opts     Options
	batch    model.Batch
	stats    Stats
	sessions *sessionAccumulator

	// pending maps an open tool_use id to its buffered call detail, awaiting the
	// tool_result that completes it.
	pending map[string]*pendingTool
}

// pendingTool is a tool_use call seen on an assistant line, buffered until its
// tool_result arrives on a later user line.
type pendingTool struct {
	sessionID   string
	toolName    string
	bashCommand string
	mcpServer   string
	tsMillis    int64 // call time (used only if the result line lacks a timestamp)
	lineStart   int64 // byte offset of the assistant line that opened this tool_use
}

// record is the subset of a transcript line we decode. Unknown keys are ignored
// here but preserved via the raw line in attrs_json builders. Pointers/`any`
// are used where a field may be absent or polymorphic (content is string OR
// array).
type record struct {
	Type      string   `json:"type"`
	UUID      string   `json:"uuid"`
	Timestamp string   `json:"timestamp"`
	SessionID string   `json:"sessionId"`
	CWD       string   `json:"cwd"`
	GitBranch string   `json:"gitBranch"`
	Version   string   `json:"version"`
	UserType  string   `json:"userType"`
	RequestID string   `json:"requestId"`
	Message   *message `json:"message"`
}

// message is the inner Claude API message envelope on user/assistant lines.
type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	ID      string          `json:"id"`
	Usage   *usage          `json:"usage"`
	Content json.RawMessage `json:"content"` // string OR []contentBlock
}

// usage is the assistant token accounting. The four counters map directly to
// model.Tokens.
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// contentBlock is one element of message.content[]. type discriminates:
// text / tool_use / tool_result (others are ignored but preserved in attrs).
type contentBlock struct {
	Type string `json:"type"`
	// text fields (a user-authored text block in array form, I4).
	Text string `json:"text"`
	// tool_use fields.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result fields.
	ToolUseID string `json:"tool_use_id"`
	IsError   *bool  `json:"is_error"`
}

// parseLine decodes one transcript line and dispatches by type. A decode
// failure (incomplete/corrupt line) is counted and skipped, never fatal.
// lineStart is the byte offset of this line in the parsed stream (threaded to
// handleAssistant so a buffered tool_use remembers where to rewind to).
func (p *parser) parseLine(line []byte, lineStart int64) {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		p.stats.Malformed++
		return
	}
	p.stats.Decoded++

	ts := parseTimestamp(rec.Timestamp)

	// Upsert session identity for any line that names a session (even pure
	// metadata lines like "mode" carry sessionId — recording first/last seen and
	// repo/branch from them is harmless and improves time bounds).
	if rec.SessionID != "" {
		p.sessions.observe(rec.SessionID, ts, &rec)
	}

	switch rec.Type {
	case "assistant":
		p.handleAssistant(&rec, ts, line, lineStart)
	case "user":
		p.handleUser(&rec, ts, line)
	default:
		// summary, mode, permission-mode, file-history-snapshot, attachment,
		// last-prompt, and any unknown type: no telemetry to map. Counted as
		// Decoded but not Mapped.
	}
}

// handleAssistant maps an assistant line to one APIRequest (from usage) and
// buffers any tool_use blocks for later join with their tool_result. lineStart
// is the assistant line's byte offset, stored on each buffered tool_use so an
// unpaired one can report a rewind point (Stats.PendingByteOffset).
func (p *parser) handleAssistant(rec *record, ts int64, line []byte, lineStart int64) {
	if rec.Message == nil {
		return
	}
	sid := rec.SessionID

	// usage → APIRequest. Skip only when there is no usage at all (e.g. a
	// streamed partial without accounting); a zero-token usage is still a real
	// request and is recorded.
	if rec.Message.Usage != nil {
		p.appendAPIRequest(rec, ts, line)
	}

	// Buffer tool_use calls; emit the matching ToolResult when its tool_result
	// arrives (handleUser).
	for _, blk := range decodeContentBlocks(rec.Message.Content) {
		if blk.Type != "tool_use" || blk.ID == "" {
			continue
		}
		pt := &pendingTool{
			sessionID: sid,
			toolName:  blk.Name,
			tsMillis:  ts,
			lineStart: lineStart,
		}
		if blk.Name == bashToolName {
			pt.bashCommand = extractCommand(blk.Input)
		}
		if srv := mcpServer(blk.Name); srv != "" {
			pt.mcpServer = srv
		}
		p.pending[blk.ID] = pt
	}
}

// handleUser maps a user line. It completes any tool_result blocks (joining to
// the buffered tool_use) and, when the line carries a plain prompt, records a
// user_prompt Event.
func (p *parser) handleUser(rec *record, ts int64, line []byte) {
	if rec.Message == nil {
		return
	}
	sid := rec.SessionID

	// content may be a plain prompt string OR an array of blocks. A string
	// prompt → one user_prompt Event keyed by the line uuid. The two forms are
	// mutually exclusive (a content value is either a string or an array), so
	// handling both cannot double-count.
	if promptStr, ok := decodeContentString(rec.Message.Content); ok {
		p.appendUserPrompt(rec, ts, line, promptStr, rec.UUID)
		return
	}

	// Array form: scan for tool_result blocks (completing buffered tool_use) and
	// for user-authored text blocks. I4: a text block IS user input — emitting
	// nothing for it was a silent loss (PLAN v3 §9 "조용한 손실 금지"). Each text
	// block becomes a user_prompt Event, discriminated in the dedup_key by the
	// line uuid PLUS the block index so multiple text blocks on one line (or a
	// text block sharing the line's instant with other records) never collide.
	for i, blk := range decodeContentBlocks(rec.Message.Content) {
		switch blk.Type {
		case "tool_result":
			p.appendToolResult(sid, ts, line, blk)
		case "text":
			// Skip empty text blocks (no user input to record).
			if strings.TrimSpace(blk.Text) == "" {
				continue
			}
			p.appendUserPrompt(rec, ts, line, blk.Text, rec.UUID+":"+strconv.Itoa(i))
		}
	}
}

// appendAPIRequest builds a model.APIRequest from an assistant message's usage.
// CostUSD is computed from the pricing table (transcripts carry no cost_usd).
// The dedup_key uses the shared model.APIRequestDedupKey so it matches the OTLP
// path for the same call (EQ6).
func (p *parser) appendAPIRequest(rec *record, ts int64, line []byte) {
	u := rec.Message.Usage
	mdl := rec.Message.Model
	tokens := model.Tokens{
		Input:         u.InputTokens,
		Output:        u.OutputTokens,
		CacheRead:     u.CacheReadInputTokens,
		CacheCreation: u.CacheCreationInputTokens,
	}

	dedup := model.APIRequestDedupKey(
		rec.SessionID, "api_request",
		nanosFromMillis(ts),
		mdl,
		strconv.FormatInt(tokens.Input, 10),
		strconv.FormatInt(tokens.Output, 10),
	)

	p.batch.APIRequests = append(p.batch.APIRequests, model.APIRequest{
		SessionID: rec.SessionID,
		TS:        ts,
		Model:     mdl,
		Tokens:    tokens,
		CostUSD:   estimateCost(mdl, tokens),
		// DurationMS/IsError/StatusCode: the transcript does not record API
		// latency or error status on the assistant message, so they stay zero
		// (false). api_error has no transcript representation → always
		// "api_request".
		//
		// CostUSD here is a pricing-table ESTIMATE (the transcript has no cost),
		// $0 for an unknown model. It must never overwrite an authoritative OTLP
		// cost at the same dedup_key, and a JSONL re-scan of an OTLP-filled row
		// must be a no-op — both are enforced by the store upsert (PLAN v3 §9).
		CostSource: model.CostSourceJSONLEstimate,
		DedupKey:   dedup,
		AttrsJSON:  p.rawAttrsLine(line),
	})

	if rec.SessionID != "" {
		// Token/cost came from the (event-equivalent) api_request path, which per
		// PLAN §6.5 wins over the metric fallback.
		p.sessions.markEventTokens(rec.SessionID)
	}
	p.stats.Mapped++
}

// appendToolResult emits a model.ToolResult by joining a tool_result block to
// the buffered tool_use it completes. ToolName/BashCommand/MCPServer come from
// the buffered call; Success comes from the result's is_error (absent = success).
func (p *parser) appendToolResult(sid string, ts int64, line []byte, blk contentBlock) {
	pt := p.pending[blk.ToolUseID]

	toolName := ""
	bash := ""
	mcp := ""
	if pt != nil {
		toolName = pt.toolName
		bash = pt.bashCommand
		mcp = pt.mcpServer
		if sid == "" {
			sid = pt.sessionID
		}
		if ts == 0 {
			ts = pt.tsMillis
		}
		delete(p.pending, blk.ToolUseID)
	}

	success := true
	if blk.IsError != nil {
		success = !*blk.IsError
	}

	if p.opts.NoStorePrompts && bash != "" {
		bash = maskedValue
	}

	// Discriminate the dedup_key by the tool_use_id so MULTIPLE tool_results of the
	// SAME tool_name on ONE transcript line stay distinct: all such results share
	// the line's single top-level timestamp, so the plain (session+ts+tool_name)
	// key collides and the tool_results.dedup_key UNIQUE constraint would drop all
	// but the first (parallel/batched same-tool command history lost). tool_use_id
	// is the most stable per-result discriminator (it is the id the tool_result
	// references back to its tool_use). When it is absent (a malformed/legacy
	// tool_result with no tool_use_id) we fall back to the cross-source key so the
	// record is still emitted.
	//
	// This is a JSONL-INTERNAL discriminator: Claude Code's OTLP tool_result carries
	// no tool_use_id, so an OTLP/JSONL pair for the same run no longer collapses.
	// That is acceptable — tool_result is command-history display only and carries
	// NO cost/token (api_request, which is the cost-accuracy axis, is unaffected and
	// keeps its cross-source EQ6 key). Preventing intra-line loss takes priority.
	var dedup string
	if blk.ToolUseID != "" {
		dedup = model.ToolResultDedupKeyWithDiscriminator(sid, nanosFromMillis(ts), toolName, blk.ToolUseID)
	} else {
		dedup = model.ToolResultDedupKey(sid, nanosFromMillis(ts), toolName)
	}

	p.batch.ToolResults = append(p.batch.ToolResults, model.ToolResult{
		SessionID:   sid,
		TS:          ts,
		ToolName:    toolName,
		Success:     success,
		BashCommand: bash,
		MCPServer:   mcp,
		// DurationMS is not derivable from the transcript tool_result (no timing
		// field) → 0.
		DedupKey:  dedup,
		AttrsJSON: p.rawAttrsLine(line),
	})
	if sid != "" {
		p.sessions.markHasEvents(sid)
	}
	p.stats.Mapped++
}

// appendUserPrompt records a user_prompt generic Event for a user message's
// prompt text, mirroring the OTLP user_prompt event (PLAN §6.3). It serves both
// the plain-string content form and an in-array text block (I4). The prompt text
// itself is never stored as a column; only its length is surfaced, and the raw
// line attrs are redacted when NoStorePrompts is set.
//
// dedupExtra is the per-event discriminator folded into the dedup_key: the
// string form passes the line uuid; the array form passes uuid+":"+blockIndex so
// distinct text blocks on the same line stay distinct. The OTLP path uses the
// log body for the same purpose; both go through model.EventDedupKey.
func (p *parser) appendUserPrompt(rec *record, ts int64, line []byte, prompt, dedupExtra string) {
	const name = "user_prompt"

	attrs := map[string]any{
		"prompt_length": len(prompt),
	}
	if !p.opts.NoStorePrompts {
		attrs["prompt"] = prompt
	}

	dedup := model.EventDedupKey(rec.SessionID, name, nanosFromMillis(ts), dedupExtra)

	p.batch.Events = append(p.batch.Events, model.Event{
		SessionID: rec.SessionID,
		TS:        ts,
		Name:      name,
		DedupKey:  dedup,
		AttrsJSON: marshalAttrs(attrs),
	})
	if rec.SessionID != "" {
		p.sessions.markHasEvents(rec.SessionID)
	}
	p.stats.Mapped++
}

// finish flushes the accumulated sessions into the batch. Any tool_use calls
// that never received a tool_result (e.g. an interrupted final turn, or a
// result still being appended) are intentionally NOT emitted: a ToolResult
// without its outcome would be misleading, and the next scan re-reads the file
// and pairs them once the result line lands (idempotent via dedup_key).
//
// It also computes Stats.PendingByteOffset: the smallest byte offset among the
// still-unpaired tool_use lines (or -1 if none). A streaming caller advances its
// file offset only up to this point so the next pass re-reads the open tool_use
// alongside the tool_result appended after the poll boundary, recovering the
// tool name / bash command that would otherwise be lost (P2 fix). Re-parsing the
// rewound region is harmless (dedup_key idempotency).
func (p *parser) finish() {
	p.sessions.ensureForBatch(&p.batch)

	earliest := int64(-1)
	for _, pt := range p.pending {
		if earliest == -1 || pt.lineStart < earliest {
			earliest = pt.lineStart
		}
	}
	p.stats.PendingByteOffset = earliest
}

// --- content decoding helpers ---

// decodeContentString returns the message content as a plain prompt string when
// it is a JSON string (the common user-prompt form). ok is false when content
// is an array or absent.
func decodeContentString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	// A JSON string starts with a quote; an array starts with '['.
	if raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// decodeContentBlocks returns the message content as a block array. A
// non-array (string/absent/corrupt) yields nil — callers then treat it via
// decodeContentString or skip it.
func decodeContentBlocks(raw json.RawMessage) []contentBlock {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// extractCommand pulls input.command out of a Bash tool_use block's input
// object. Missing/corrupt input yields "".
func extractCommand(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	return in.Command
}

// mcpServer extracts the <server> segment from an MCP tool name of the form
// mcp__<server>__<tool>. Returns "" for non-MCP tool names.
func mcpServer(toolName string) string {
	if !strings.HasPrefix(toolName, mcpPrefix) {
		return ""
	}
	rest := toolName[len(mcpPrefix):]
	if i := strings.Index(rest, "__"); i >= 0 {
		return rest[:i]
	}
	// mcp__<server> with no tool segment: the remainder is the server.
	return rest
}

// --- timestamp / cost / attrs helpers ---

// parseTimestamp parses a transcript RFC3339 timestamp (millisecond precision,
// e.g. "2026-05-18T13:43:39.311Z") to unix-ms. An empty/unparseable timestamp
// yields 0 (the caller leaves time bounds to other lines / receive context).
func parseTimestamp(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// nanosFromMillis renders a unix-ms instant as the base-10 unix-NANOSECOND
// string used in dedup_keys. The transcript only has millisecond precision, so
// the low six digits are always zero. The OTLP path (otel.dedupNanosFromMillis)
// truncates LogRecord.TimeUnixNano to ms and renders it the SAME way before
// building the api_request dedup_key, so the same logical call seen via both
// paths hashes to one key and collapses to one row (C1, EQ6, PLAN v3 §9). These
// two helpers MUST stay byte-for-byte identical. A zero instant renders "0".
func nanosFromMillis(ms int64) string {
	if ms == 0 {
		return "0"
	}
	return strconv.FormatInt(ms*1_000_000, 10)
}

// estimateCost computes USD for a request from the pricing table (transcripts
// carry no cost_usd). An unknown model yields 0 — the cost API flags derived
// figures as estimated and surfaces unknown-model gaps separately (PLAN v3 §9
// Failure Modes: pricing "미지 모델" → $0).
func estimateCost(mdl string, t model.Tokens) float64 {
	pr, ok := pricing.Lookup(mdl)
	if !ok {
		return 0
	}
	return (float64(t.Input)*pr.Input +
		float64(t.Output)*pr.Output +
		float64(t.CacheRead)*pr.CacheRead +
		float64(t.CacheCreation)*pr.CacheWrite) / 1e6
}

// rawAttrs serializes the original transcript line into the attrs_json blob so
// unknown/unmapped fields survive (forward-compat, PLAN §6). When
// NoStorePrompts is set this would otherwise leak prompt/command text, so the
// raw line is NOT stored verbatim in that mode — a minimal marker object is
// stored instead and the (already-masked) structured columns carry the safe
// subset. We keep it simple: store the compacted line when allowed, "{}" when
// redacting (the structured columns already hold the non-sensitive facts).
func (p *parser) rawAttrsLine(line []byte) string {
	if p.opts.NoStorePrompts {
		return "{}"
	}
	return rawAttrs(line)
}

// rawAttrs compacts a raw JSON line into a single-line JSON string for storage.
// A non-object/corrupt line (already filtered upstream) falls back to "{}".
func rawAttrs(line []byte) string {
	// Validate + compact so attrs_json is always well-formed single-line JSON.
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return "{}"
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// marshalAttrs serializes a small attribute map to a JSON object string. Returns
// "{}" on failure so the column is always valid JSON.
func marshalAttrs(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// repoFromCWD derives a repo/project display name from a session cwd. It is the
// final path element (the project directory name) with both separator styles
// handled, since transcripts on Windows carry backslash paths
// (C:\Users\rizz\...) and POSIX ones carry forward slashes. The encoded
// ~/.claude/projects folder name replaces separators with '-', so the raw cwd
// is preferred (PLAN v3 feature 3). Returns "" for an empty cwd.
func repoFromCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	// Normalize Windows backslashes to forward slashes, then use path.Base (the
	// slash-based, OS-INDEPENDENT base) so wlog derives the same repo name no
	// matter which OS it runs on (a Windows transcript ingested on Linux, etc.).
	// filepath.Base would be host-OS-dependent (e.g. it returns "\\" for "C:").
	norm := strings.ReplaceAll(cwd, "\\", "/")
	norm = strings.TrimRight(norm, "/")
	if norm == "" {
		return ""
	}
	base := path.Base(norm)
	// path.Base returns "." for empty and "/" for root; a bare drive volume
	// (e.g. "C:") is not a useful repo name either.
	if base == "." || base == "/" || strings.HasSuffix(base, ":") {
		return ""
	}
	return base
}

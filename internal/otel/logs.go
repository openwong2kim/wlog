package otel

import (
	"strconv"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// maskedValue replaces sensitive field content when prompt storage is disabled.
const maskedValue = "[redacted]"

// attrsForStore returns the attribute list to serialize into a record's
// AttrsJSON. When --no-store-prompts is set, sensitive prompt/shell-command keys
// are redacted so the raw payload never reaches the store (C1). Otherwise the
// list passes through unchanged.
func (p *Parser) attrsForStore(kvs []*commonpb.KeyValue) []*commonpb.KeyValue {
	if p.opts.NoStorePrompts {
		return maskSensitiveAttrs(kvs)
	}
	return kvs
}

// ParseLogs converts an OTLP logs export request into a model.Batch. Each
// LogRecord is classified by its normalized event name (PLAN §6.1) into an
// APIRequest (api_request / api_error), ToolDecision (tool_decision),
// ToolResult (tool_result), or a generic Event (user_prompt, mcp_server_connection,
// and any unclassified name — the original name is preserved). Every
// classified row gets a dedup_key for idempotent re-send handling. Sessions are
// upserted with HasEvents=true.
//
// Records with no resolvable session id are still parsed (session_id stays "")
// — the store associates them under an empty session; they are not rejected.
// Reject counting here is reserved for records that cannot be interpreted at all
// (currently none — a log record always maps to at least a generic Event), so
// RejectInfo is returned for symmetry and future-proofing.
func (p *Parser) ParseLogs(req *collectorlogspb.ExportLogsServiceRequest) (model.Batch, RejectInfo) {
	var batch model.Batch
	var rej RejectInfo
	if req == nil {
		return batch, rej
	}

	nowMillis := time.Now().UnixMilli()
	sessions := newSessionAccumulator()

	for _, rl := range req.GetResourceLogs() {
		resAttrs := attrsToMap(resourceAttrs(rl.GetResource()))
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				p.parseLogRecord(lr, resAttrs, nowMillis, &batch, sessions)
			}
		}
	}

	// Guarantee a sessions row for every distinct child SessionID (including the
	// empty/anonymous id) so no child INSERT violates the sessions FK and forces a
	// full-batch rollback (C2).
	sessions.ensureForBatch(&batch)
	return batch, rej
}

func (p *Parser) parseLogRecord(lr *logspb.LogRecord, resAttrs map[string]Value, nowMillis int64, batch *model.Batch, sessions *sessionAccumulator) {
	if lr == nil {
		return
	}
	attrs := attrsToMap(lr.GetAttributes())
	name := normalizeEventName(lr.GetEventName(), attrs)
	sid := sessionID(attrs, resAttrs)

	ts := nanoToMillis(lr.GetTimeUnixNano())
	if ts == 0 {
		ts = nanoToMillis(lr.GetObservedTimeUnixNano())
	}
	if ts == 0 {
		ts = nowMillis
	}

	// Always upsert session identity and mark it has events.
	if sid != "" {
		sessions.observe(sid, ts, attrs, resAttrs)
		sessions.markHasEvents(sid)
	}

	switch name {
	case EventAPIRequest, EventAPIError:
		p.appendAPIRequest(name, sid, ts, attrs, lr, batch, sessions)
	case EventToolDecision:
		p.appendToolDecision(sid, ts, attrs, lr, batch)
	case EventToolResult:
		p.appendToolResult(sid, ts, attrs, lr, batch)
	default:
		// user_prompt, mcp_server_connection, and any unclassified/unknown name
		// → generic Event with the original (prefix-normalized) name preserved.
		p.appendEvent(name, sid, ts, attrs, lr, batch)
	}
}

func (p *Parser) appendAPIRequest(name, sid string, ts int64, attrs map[string]Value, lr *logspb.LogRecord, batch *model.Batch, sessions *sessionAccumulator) {
	isError := name == EventAPIError
	model_ := attrs["model"].String()

	tokens := model.Tokens{
		Input:         attrs["input_tokens"].Int(),
		Output:        attrs["output_tokens"].Int(),
		CacheRead:     firstInt(attrs, "cache_read_tokens", "cache_read"),
		CacheCreation: firstInt(attrs, "cache_creation_tokens", "cache_creation"),
	}

	// Shared dedup_key constructor (model.APIRequestDedupKey) — the JSONL parser
	// calls the same function so an api_request seen via both OTLP and the
	// transcript collapses to one row (EQ6, PLAN v3 §9).
	//
	// C1: the dedup timestamp MUST be at millisecond resolution, not raw
	// nanoseconds. Claude Code's api_request OTLP event carries no stable id we
	// could key on (only session.id/model/tokens/cost/duration — see the golden
	// fixtures), so the timestamp is part of the identity. The transcript only
	// has millisecond precision (RFC3339 ms), so the JSONL path renders its
	// dedup ts as ms→ns with the low six digits always zero. Feeding raw
	// nanoseconds here would make the two keys disagree for essentially every
	// call (the ns almost never lands exactly on a ms boundary), splitting one
	// api_request into two rows and DOUBLING SUM(cost_usd)/tokens. We therefore
	// truncate to ms and render in the SAME ms→ns form the JSONL path uses
	// (dedupNanosFromMillis), so the same logical call hashes to the same key.
	dedup := model.APIRequestDedupKey(
		sid, name,
		dedupNanosFromMillis(ts),
		model_,
		strconv.FormatInt(tokens.Input, 10),
		strconv.FormatInt(tokens.Output, 10),
	)

	batch.APIRequests = append(batch.APIRequests, model.APIRequest{
		SessionID:  sid,
		TS:         ts,
		Model:      model_,
		Tokens:     tokens,
		CostUSD:    firstFloat(attrs, "cost_usd", "cost"),
		DurationMS: firstInt(attrs, "duration_ms", "duration"),
		IsError:    isError,
		StatusCode: int(firstInt(attrs, "status_code", "http.status_code")),
		// OTLP carries Claude Code's own cost_usd, so this figure is authoritative
		// (PLAN v3 §9). On a dedup_key collision with the JSONL estimate path, the
		// store keeps this value (and these tokens) — see insertAPIRequests.
		CostSource: model.CostSourceOTLP,
		DedupKey:   dedup,
		AttrsJSON:  marshalAttrs(p.attrsForStore(lr.GetAttributes())),
	})

	// Token/cost source is the event path (PLAN §6.5: api_request wins over
	// the token.usage/cost.usage metric fallback).
	if sid != "" {
		sessions.markEventTokens(sid)
	}
}

func (p *Parser) appendToolDecision(sid string, ts int64, attrs map[string]Value, lr *logspb.LogRecord, batch *model.Batch) {
	decision := attrs["decision"].String()
	source := attrs["source"].String()
	toolName := firstString(attrs, "tool_name", "tool")

	// Dedup timestamp at MILLISECOND resolution, rendered ms→ns the SAME way the
	// JSONL path does (dedupNanosFromMillis == jsonl.nanosFromMillis), exactly like
	// api_request (C1). tool_decision is OTLP-only today, but normalizing now keeps
	// the key construction uniform with tool_result/api_request and future-proofs a
	// transcript source. ts already carries the ms-resolved instant (with the
	// observed-time / now fallbacks parseLogRecord applied).
	dedup := model.ToolDecisionDedupKey(
		sid,
		dedupNanosFromMillis(ts),
		toolName, decision, source,
	)

	batch.ToolDecisions = append(batch.ToolDecisions, model.ToolDecision{
		SessionID: sid,
		TS:        ts,
		Decision:  decision,
		Source:    source,
		ToolName:  toolName,
		DedupKey:  dedup,
		AttrsJSON: marshalAttrs(p.attrsForStore(lr.GetAttributes())),
	})
}

func (p *Parser) appendToolResult(sid string, ts int64, attrs map[string]Value, lr *logspb.LogRecord, batch *model.Batch) {
	toolName := firstString(attrs, "tool_name", "tool")
	td := decodeToolParameters(attrs)

	bash := td.BashCommand
	if bash == "" {
		bash = td.FullCommand
	}
	if p.opts.NoStorePrompts {
		// bash_command / full_command can contain sensitive shell input.
		if bash != "" {
			bash = maskedValue
		}
	}

	// Dedup timestamp at MILLISECOND resolution (C1), rendered ms→ns identically to
	// the JSONL path (otel.dedupNanosFromMillis is byte-for-byte jsonl.nanosFromMillis).
	// Before this, tool_result hashed the raw LogRecord.TimeUnixNano while JSONL
	// hashed the ms instant, so the SAME Bash/MCP run seen via BOTH OTLP and the
	// transcript produced two keys → a duplicate command-history row. Truncating to
	// ms here makes the two keys agree and the store collapses them to one row.
	dedup := model.ToolResultDedupKey(
		sid,
		dedupNanosFromMillis(ts),
		toolName,
	)

	batch.ToolResults = append(batch.ToolResults, model.ToolResult{
		SessionID:   sid,
		TS:          ts,
		ToolName:    toolName,
		Success:     attrs["success"].Bool(),
		DurationMS:  firstInt(attrs, "duration_ms", "duration"),
		BashCommand: bash,
		MCPServer:   td.MCPServer,
		DedupKey:    dedup,
		AttrsJSON:   marshalAttrs(p.attrsForStore(lr.GetAttributes())),
	})
}

func (p *Parser) appendEvent(name, sid string, ts int64, attrs map[string]Value, lr *logspb.LogRecord, batch *model.Batch) {
	// Generic events (user_prompt, mcp_server_connection, unknown names) may carry
	// raw prompt text or shell input in any attribute. attrsForStore redacts the
	// full sensitive key set when --no-store-prompts is on (C1), not just
	// user_prompt — an unclassified event could carry the same payload.
	kvs := p.attrsForStore(lr.GetAttributes())

	dedup := model.EventDedupKey(
		sid, name,
		strconv.FormatUint(lr.GetTimeUnixNano(), 10),
		bodyString(lr),
	)

	batch.Events = append(batch.Events, model.Event{
		SessionID: sid,
		TS:        ts,
		Name:      name,
		DedupKey:  dedup,
		AttrsJSON: marshalAttrs(kvs),
	})
	_ = attrs
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// testEnv bundles a real store, a live bus, and an HTTP test server wired to a
// fresh Server. Each test gets an isolated on-disk DB (a unique temp file) so
// the shared in-memory cache does not leak data between tests.
type testEnv struct {
	t        *testing.T
	store    *store.Store
	bus      *ingest.Bus
	srv      *Server
	ts       *httptest.Server
	counters ingest.Counters
	dbPath   string
}

// newTestEnv opens a fresh file-backed store, builds a Server, and starts an
// httptest server. It registers cleanup. A counters closure returns env.counters
// so tests can adjust the health counters.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "wlog-test.db")
	st, err := store.Open(dbPath, false, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	bus := ingest.NewBus(0)

	env := &testEnv{t: t, store: st, bus: bus, dbPath: dbPath}
	cfg := config.Default()
	cfg.DBPath = dbPath
	env.srv = New(cfg, st.ReadDB(), bus, func() ingest.Counters { return env.counters }, dbPath)
	env.ts = httptest.NewServer(env.srv.Handler())

	t.Cleanup(func() {
		env.ts.Close()
		_ = st.Close()
	})
	return env
}

// write applies a batch through the single writer (the production path).
func (e *testEnv) write(b model.Batch) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.WriteBatch(ctx, b); err != nil {
		e.t.Fatalf("WriteBatch: %v", err)
	}
}

// get performs a GET against the test server and returns status + decoded body.
func (e *testEnv) get(path string, out any) int {
	e.t.Helper()
	resp, err := http.Get(e.ts.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

// --- seed builders -----------------------------------------------------------

// seedSession upserts a bare session row.
func seedSession(id string, firstSeen, lastSeen int64, tokenSource string, hasEvents bool) model.Session {
	return model.Session{
		ID:          id,
		FirstSeen:   firstSeen,
		LastSeen:    lastSeen,
		TokenSource: tokenSource,
		HasEvents:   hasEvents,
		ModelSet:    "",
	}
}

// apiReq builds an api_request row with a deterministic dedup key.
func apiReq(sid string, ts int64, model_ string, in, out, cr, cc int64, cost float64, dur int64) model.APIRequest {
	return model.APIRequest{
		SessionID:  sid,
		TS:         ts,
		Model:      model_,
		Tokens:     model.Tokens{Input: in, Output: out, CacheRead: cr, CacheCreation: cc},
		CostUSD:    cost,
		DurationMS: dur,
		DedupKey:   model.DedupKey(sid, "api_request", itoa(ts), model_, itoa(in), itoa(out)),
	}
}

// apiErr builds an api_error row.
func apiErr(sid string, ts int64, model_ string, status int, errMsg string) model.APIRequest {
	return model.APIRequest{
		SessionID:  sid,
		TS:         ts,
		Model:      model_,
		IsError:    true,
		StatusCode: status,
		DedupKey:   model.DedupKey(sid, "api_error", itoa(ts), model_),
		AttrsJSON:  `{"error":"` + errMsg + `"}`,
	}
}

// toolDecision builds a tool_decision row.
func toolDecision(sid string, ts int64, decision, source, tool string) model.ToolDecision {
	return model.ToolDecision{
		SessionID: sid,
		TS:        ts,
		Decision:  decision,
		Source:    source,
		ToolName:  tool,
		DedupKey:  model.DedupKey(sid, "tool_decision", itoa(ts), tool, decision, source),
	}
}

// toolResult builds a tool_result row.
func toolResult(sid string, ts int64, tool string, success bool, dur int64, bash, mcp string) model.ToolResult {
	return model.ToolResult{
		SessionID:   sid,
		TS:          ts,
		ToolName:    tool,
		Success:     success,
		DurationMS:  dur,
		BashCommand: bash,
		MCPServer:   mcp,
		DedupKey:    model.DedupKey(sid, "tool_result", itoa(ts), tool),
	}
}

// promptEvent builds a user_prompt generic event carrying prompt_length only
// (the --no-store-prompts shape: length without the prompt text).
func promptEvent(sid string, ts int64, promptLen int) model.Event {
	return model.Event{
		SessionID: sid,
		TS:        ts,
		Name:      "user_prompt",
		DedupKey:  model.DedupKey(sid, "user_prompt", itoa(ts)),
		AttrsJSON: `{"prompt_length":` + itoa(int64(promptLen)) + `}`,
	}
}

// promptEventText builds a user_prompt event with both prompt_length and the
// stored prompt text (the default storage shape).
func promptEventText(sid string, ts int64, text string) model.Event {
	attrs, _ := json.Marshal(map[string]any{
		"prompt_length": len(text),
		"prompt":        text,
	})
	return model.Event{
		SessionID: sid,
		TS:        ts,
		Name:      "user_prompt",
		DedupKey:  model.DedupKey(sid, "user_prompt", itoa(ts)),
		AttrsJSON: string(attrs),
	}
}

// metricPoint builds a token/cost/active_time metric point. attrKey is the
// token type (input/output/...) for token.usage, the model for cost.usage, or
// "" for active_time.
func metricPoint(sid string, ts int64, name, attrKey string, delta float64, kind int, startNano, timeNano int64) model.MetricPoint {
	dims := map[string]string{"session.id": sid}
	if attrKey != "" {
		dims["type"] = attrKey
	}
	return model.MetricPoint{
		SessionID:     sid,
		TS:            ts,
		Name:          name,
		AttrKey:       attrKey,
		ValueDelta:    delta,
		ValueKind:     kind,
		SeriesKey:     model.SeriesKey(name+"|"+attrKey, dims),
		StartUnixNano: startNano,
		TimeUnixNano:  timeNano,
	}
}

func itoa(n int64) string {
	return jsonInt(n)
}

// jsonInt formats an int64 without importing strconv repeatedly in builders.
func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

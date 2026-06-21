package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/openwong2kim/wlog/internal/ingest"
)

// tickInterval is the server-side cadence for the SSE "tick" event (PLAN §10 /
// §9). The client throttles its own rendering to 2s; the server emits the
// coalesced snapshot every second so reconnects converge quickly.
const tickInterval = time.Second

// tickPayload is the SSE "tick" event data (PLAN §10). SessionID is a pointer so
// it serializes as JSON null when there is no active session.
type tickPayload struct {
	TS            int64      `json:"ts"`
	SessionID     *string    `json:"session_id"`
	CostUSD       float64    `json:"cost_usd"`
	Tokens        tokensJSON `json:"tokens"`
	ActiveTimeSec float64    `json:"active_time_sec"`
	ActiveTool    *string    `json:"active_tool"`
}

// handleNow streams Server-Sent Events: a server-cadenced "tick" snapshot, the
// per-event "log" stream forwarded from the bus, "drop" overflow markers, and
// connection "status" events. It runs until the client disconnects (request
// context cancelled). Last-Event-ID is accepted but not required — reconnect is
// safe because tick carries the full current snapshot.
func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		internalError(w, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering if any
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()

	sub := s.bus.Subscribe()
	defer sub.Close()

	// Announce live state immediately, then emit the first tick so a fresh
	// connection has data without waiting a full interval.
	writeSSE(w, "status", map[string]any{"state": "live"})
	s.writeTick(w)
	flusher.Flush()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected; sub.Close runs via defer.
			return

		case <-ticker.C:
			s.writeTick(w)
			flusher.Flush()

		case msg, ok := <-sub.C():
			if !ok {
				// Bus closed the subscriber (shutdown). Tell the client to reconnect.
				writeSSE(w, "status", map[string]any{"state": "reconnecting"})
				flusher.Flush()
				return
			}
			switch msg.Kind {
			case "_drop":
				writeSSE(w, "drop", map[string]any{"dropped": msg.Dropped})
			default: // "log"
				writeSSE(w, "log", logEventPayload(msg.Event))
			}
			flusher.Flush()
		}
	}
}

// writeTick builds and writes the current "tick" event from the bus snapshot.
func (s *Server) writeTick(w http.ResponseWriter) {
	snap := s.bus.Snapshot()
	p := tickPayload{
		TS:            snap.TS,
		CostUSD:       snap.CostUSD,
		ActiveTimeSec: snap.ActiveTimeSec,
		Tokens: tokensJSON{
			Input:         snap.Tokens.Input,
			Output:        snap.Tokens.Output,
			CacheRead:     snap.Tokens.CacheRead,
			CacheCreation: snap.Tokens.CacheCreation,
		},
	}
	if p.TS == 0 {
		p.TS = time.Now().UnixMilli()
	}
	if snap.SessionID != "" {
		sid := snap.SessionID
		p.SessionID = &sid
	}
	if snap.ActiveTool != "" {
		tool := snap.ActiveTool
		p.ActiveTool = &tool
	}
	writeSSE(w, "tick", p)
}

// logEventPayload builds the SSE "log" event data: {ts, kind, summary, ...kind
// fields} (PLAN §10). The kind-specific fields from the bus event are merged in;
// ts/kind/summary take precedence over any collision.
func logEventPayload(ev ingest.LogEvent) map[string]any {
	m := make(map[string]any, len(ev.Fields)+3)
	for k, v := range ev.Fields {
		m[k] = v
	}
	m["ts"] = ev.TS
	m["kind"] = ev.Kind
	m["summary"] = ev.Summary
	return m
}

// writeSSE serializes data as JSON and writes one SSE event frame
// (event: <name>\n data: <json>\n\n). A marshal failure is silently skipped so
// the stream survives a single bad payload.
func writeSSE(w http.ResponseWriter, event string, data any) {
	buf, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, buf)
}

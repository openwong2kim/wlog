package otel

import "github.com/openwong2kim/wlog/internal/model"

// sessionAccumulator collects per-session identity/metadata across a single
// parse pass. Sessions carry no pre-aggregated counters (REVIEWS A F5 — pre-agg
// columns removed; cost/token aggregation is on-the-fly at query time). We only
// upsert identity, time bounds (event-time min/max, order-independent — Eng
// F12), display metadata, token_source, and has_events.
type sessionAccumulator struct {
	order []string
	byID  map[string]*model.Session
}

func newSessionAccumulator() *sessionAccumulator {
	return &sessionAccumulator{byID: make(map[string]*model.Session)}
}

// observe upserts a session from an event/metric occurrence. ts is unix-ms event
// time; primary/resource are the attribute scopes (datapoint or log record first,
// resource fallback) used to pull display metadata.
func (a *sessionAccumulator) observe(id string, ts int64, primary, resource map[string]Value) {
	if id == "" {
		return
	}
	s, ok := a.byID[id]
	if !ok {
		s = &model.Session{ID: id, FirstSeen: ts, LastSeen: ts}
		a.byID[id] = s
		a.order = append(a.order, id)
	}

	// Event-time min/max, order-independent.
	if ts > 0 {
		if s.FirstSeen == 0 || ts < s.FirstSeen {
			s.FirstSeen = ts
		}
		if ts > s.LastSeen {
			s.LastSeen = ts
		}
	}

	if s.AppVersion == "" {
		s.AppVersion = stringAttr(primary, resource, "app.version", "service.version")
	}
	if s.UserEmail == "" {
		s.UserEmail = stringAttr(primary, resource, "user.email")
	}
	if s.OrgID == "" {
		s.OrgID = stringAttr(primary, resource, "organization.id", "org.id", "org_id")
	}
	if model := stringAttr(primary, resource, "model"); model != "" {
		s.ModelSet = mergeModelSet(s.ModelSet, model)
	}
}

// markTokenSource records that token/cost data for a session came from metrics
// (the api_request event path sets "events"; see markEventTokenSource). Per
// PLAN §6.5 the event path wins, so we only set "metrics" if not already
// pinned to "events".
func (a *sessionAccumulator) markTokenSource(id, metricName string) {
	if id == "" {
		return
	}
	if metricName != "claude_code.token.usage" && metricName != "token.usage" &&
		metricName != "claude_code.cost.usage" && metricName != "cost.usage" {
		return
	}
	if s, ok := a.byID[id]; ok && s.TokenSource == "" {
		s.TokenSource = "metrics"
	}
}

// markEventTokens flags that token/cost data came from events (api_request),
// which takes priority over the metrics fallback, and that the session has
// events.
func (a *sessionAccumulator) markEventTokens(id string) {
	if s, ok := a.byID[id]; ok {
		s.TokenSource = "events"
		s.HasEvents = true
	}
}

// markHasEvents flags that the session produced at least one log event.
func (a *sessionAccumulator) markHasEvents(id string) {
	if s, ok := a.byID[id]; ok {
		s.HasEvents = true
	}
}

// sessions returns the accumulated sessions in first-seen insertion order
// (deterministic batch ordering).
func (a *sessionAccumulator) sessions() []model.Session {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]model.Session, 0, len(a.order))
	for _, id := range a.order {
		out = append(out, *a.byID[id])
	}
	return out
}

// ensure guarantees a Session row exists for id (including the empty id), so a
// child record referencing it never violates the sessions foreign key (C2). If
// the session was never observed (e.g. session.id absent on a metric/log → id
// == ""), a minimal row is synthesized with FirstSeen/LastSeen = ts. observe
// is NOT called here because the empty-id path is intentionally skipped by
// observe (no display metadata to pull for an anonymous session).
func (a *sessionAccumulator) ensure(id string, ts int64) {
	if _, ok := a.byID[id]; ok {
		if ts > 0 {
			s := a.byID[id]
			if s.FirstSeen == 0 || ts < s.FirstSeen {
				s.FirstSeen = ts
			}
			if ts > s.LastSeen {
				s.LastSeen = ts
			}
		}
		return
	}
	a.byID[id] = &model.Session{ID: id, FirstSeen: ts, LastSeen: ts}
	a.order = append(a.order, id)
}

// ensureForBatch walks every child record in the batch and guarantees a Session
// row exists for each distinct SessionID it references (including ""), then
// writes the accumulated sessions back into the batch (C2). Without this, a
// record whose session.id was absent carries SessionID "" but has no matching
// sessions row, so the child INSERT violates the REFERENCES sessions foreign key
// and the store rolls back the ENTIRE batch — losing every valid row with it.
//
// Synthesized rows use the child record's timestamp for FirstSeen/LastSeen so
// the empty/anonymous session still has sane time bounds.
func (a *sessionAccumulator) ensureForBatch(batch *model.Batch) {
	for _, r := range batch.APIRequests {
		a.ensure(r.SessionID, r.TS)
	}
	for _, d := range batch.ToolDecisions {
		a.ensure(d.SessionID, d.TS)
	}
	for _, tr := range batch.ToolResults {
		a.ensure(tr.SessionID, tr.TS)
	}
	for _, e := range batch.Events {
		a.ensure(e.SessionID, e.TS)
	}
	for _, sp := range batch.Spans {
		a.ensure(sp.SessionID, sp.StartTS)
	}
	for _, mp := range batch.MetricPoints {
		a.ensure(mp.SessionID, mp.TS)
	}
	batch.Sessions = a.sessions()
}

// mergeModelSet keeps a comma-separated, de-duplicated set of model names in
// stable insertion order (display only).
func mergeModelSet(existing, model string) string {
	if existing == "" {
		return model
	}
	// Linear scan; the set of models per session is tiny.
	start := 0
	for i := 0; i <= len(existing); i++ {
		if i == len(existing) || existing[i] == ',' {
			if existing[start:i] == model {
				return existing // already present
			}
			start = i + 1
		}
	}
	return existing + "," + model
}

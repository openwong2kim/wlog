package jsonl

import "github.com/openwong2kim/wlog/internal/model"

// sessionAccumulator collects per-session identity/metadata across one parse
// pass, mirroring internal/otel's accumulator (no pre-aggregated counters —
// cost/token aggregation is on-the-fly at query time). It additionally captures
// the JSONL-only Repo (from cwd) and GitBranch (from gitBranch) dimensions
// (PLAN v3 feature 3, OQ4 resolved via JSONL).
//
// All "first non-empty wins" fields (AppVersion, UserEmail, Repo, GitBranch)
// are set once and never overwritten with a later empty value, so a metadata
// line that omits a field cannot clobber a value an earlier line supplied
// (matches the store upsert rule: do not overwrite known with empty).
type sessionAccumulator struct {
	order []string
	byID  map[string]*model.Session
}

func newSessionAccumulator() *sessionAccumulator {
	return &sessionAccumulator{byID: make(map[string]*model.Session)}
}

// observe upserts a session from one transcript line. ts is the line's unix-ms
// timestamp (0 when absent); rec supplies the identity/metadata fields.
func (a *sessionAccumulator) observe(id string, ts int64, rec *record) {
	if id == "" {
		return
	}
	s, ok := a.byID[id]
	if !ok {
		s = &model.Session{ID: id, FirstSeen: ts, LastSeen: ts}
		a.byID[id] = s
		a.order = append(a.order, id)
	}

	// Event-time min/max, order-independent (a transcript is append-ordered, but
	// we stay defensive and do not assume it).
	if ts > 0 {
		if s.FirstSeen == 0 || ts < s.FirstSeen {
			s.FirstSeen = ts
		}
		if ts > s.LastSeen {
			s.LastSeen = ts
		}
	}

	if s.AppVersion == "" && rec.Version != "" {
		s.AppVersion = rec.Version
	}
	if s.Repo == "" {
		if r := repoFromCWD(rec.CWD); r != "" {
			s.Repo = r
		}
	}
	if s.GitBranch == "" && rec.GitBranch != "" {
		s.GitBranch = rec.GitBranch
	}
	// Model is observed on assistant lines (message.model); merge into the
	// display-only model set.
	if rec.Message != nil && rec.Message.Model != "" {
		s.ModelSet = mergeModelSet(s.ModelSet, rec.Message.Model)
	}
}

// markEventTokens flags that token/cost came from the api_request path (which
// wins over the metric fallback, PLAN §6.5) and that the session has events.
func (a *sessionAccumulator) markEventTokens(id string) {
	if s, ok := a.byID[id]; ok {
		s.TokenSource = "events"
		s.HasEvents = true
	}
}

// markHasEvents flags that the session produced at least one mapped record.
func (a *sessionAccumulator) markHasEvents(id string) {
	if s, ok := a.byID[id]; ok {
		s.HasEvents = true
	}
}

// sessions returns the accumulated sessions in first-seen insertion order.
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

// ensure guarantees a Session row exists for id (including ""), updating its
// time bounds, so a child record referencing it never violates the sessions
// foreign key (C2, matching the otel parser).
func (a *sessionAccumulator) ensure(id string, ts int64) {
	if s, ok := a.byID[id]; ok {
		if ts > 0 {
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

// ensureForBatch guarantees a Session row for every distinct SessionID the
// batch's child records reference (including ""), then writes the accumulated
// sessions back into the batch (C2 — a child whose session was never observed
// would otherwise violate the REFERENCES sessions FK and roll back the whole
// batch).
func (a *sessionAccumulator) ensureForBatch(batch *model.Batch) {
	for _, r := range batch.APIRequests {
		a.ensure(r.SessionID, r.TS)
	}
	for _, tr := range batch.ToolResults {
		a.ensure(tr.SessionID, tr.TS)
	}
	for _, e := range batch.Events {
		a.ensure(e.SessionID, e.TS)
	}
	batch.Sessions = a.sessions()
}

// mergeModelSet keeps a comma-separated, de-duplicated set of model names in
// stable insertion order (display only). Mirrors internal/otel.mergeModelSet.
func mergeModelSet(existing, model string) string {
	if existing == "" {
		return model
	}
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

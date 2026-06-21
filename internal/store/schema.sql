-- wlog SQLite schema (PLAN §8). Pure-Go modernc driver, WAL, single writer.
-- No pre-aggregation columns on sessions (drift removed; API does on-the-fly
-- GROUP BY). All child tables CASCADE on session delete. Idempotency via
-- dedup_key UNIQUE (events) / UNIQUE(series_key,start,time) (metric_points) /
-- UNIQUE(trace_id,span_id) (spans). This file is applied at schema version 1;
-- see migrate.go (PRAGMA user_version) for forward migrations.

CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    first_seen   INTEGER NOT NULL, -- unix-ms, min(event time | receive time)
    last_seen    INTEGER NOT NULL, -- unix-ms, max(event time | receive time)
    app_version  TEXT,
    model_set    TEXT,
    user_email   TEXT,
    org_id       TEXT,
    token_source TEXT,             -- 'events' | 'metrics' (display hint only)
    has_events   INTEGER NOT NULL DEFAULT 0,
    repo         TEXT,             -- project/repo name from JSONL cwd (aggregation dim, PLAN v3 §3)
    git_branch   TEXT,             -- git branch from the JSONL transcript (may be empty)
    attrs_json   TEXT
);

CREATE TABLE IF NOT EXISTS api_requests (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ts             INTEGER NOT NULL,
    model          TEXT,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cache_creation INTEGER NOT NULL DEFAULT 0,
    cost_usd       REAL    NOT NULL DEFAULT 0,
    duration_ms    INTEGER NOT NULL DEFAULT 0,
    is_error       INTEGER NOT NULL DEFAULT 0,
    status_code    INTEGER NOT NULL DEFAULT 0,
    cost_source    TEXT,             -- 'otlp' (authoritative) | 'jsonl-estimate' (PLAN v3 §9 cost authority)
    dedup_key      TEXT NOT NULL UNIQUE,
    attrs_json     TEXT
);

CREATE TABLE IF NOT EXISTS tool_decisions (
    id         INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ts         INTEGER NOT NULL,
    decision   TEXT,
    source     TEXT,
    tool_name  TEXT,
    dedup_key  TEXT NOT NULL UNIQUE,
    attrs_json TEXT
);

CREATE TABLE IF NOT EXISTS tool_results (
    id           INTEGER PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ts           INTEGER NOT NULL,
    tool_name    TEXT,
    success      INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    bash_command TEXT,
    mcp_server   TEXT,
    dedup_key    TEXT NOT NULL UNIQUE,
    attrs_json   TEXT
);

CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ts         INTEGER NOT NULL,
    name       TEXT,
    dedup_key  TEXT NOT NULL UNIQUE,
    attrs_json TEXT
);

CREATE TABLE IF NOT EXISTS spans (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    trace_id       TEXT NOT NULL,
    span_id        TEXT NOT NULL,
    parent_span_id TEXT,
    name           TEXT,
    start_ts       INTEGER NOT NULL DEFAULT 0,
    end_ts         INTEGER NOT NULL DEFAULT 0,
    status         TEXT,
    attrs_json     TEXT,
    UNIQUE(trace_id, span_id)
);

CREATE TABLE IF NOT EXISTS metric_points (
    id              INTEGER PRIMARY KEY,
    session_id      TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ts              INTEGER NOT NULL,
    name            TEXT,
    attr_key        TEXT,
    value_delta     REAL    NOT NULL DEFAULT 0,
    value_kind      INTEGER NOT NULL DEFAULT 0,
    series_key      TEXT NOT NULL,
    start_unixnano  INTEGER NOT NULL DEFAULT 0,
    time_unixnano   INTEGER NOT NULL DEFAULT 0,
    attrs_json      TEXT,
    UNIQUE(series_key, start_unixnano, time_unixnano)
);

CREATE TABLE IF NOT EXISTS series_state (
    series_key          TEXT PRIMARY KEY,
    last_value          REAL    NOT NULL DEFAULT 0,
    last_start_unixnano INTEGER NOT NULL DEFAULT 0,
    last_point_unixnano INTEGER NOT NULL DEFAULT 0,
    temporality         INTEGER NOT NULL DEFAULT 0,
    baseline_known      INTEGER NOT NULL DEFAULT 0,
    updated             INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT
);

-- Query indexes: (session_id, ts) on every per-session signal table; last_seen
-- on sessions for retention scans and recency sort.
CREATE INDEX IF NOT EXISTS idx_api_requests_session_ts   ON api_requests(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_tool_decisions_session_ts ON tool_decisions(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_tool_results_session_ts   ON tool_results(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_session_ts         ON events(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_spans_session_ts          ON spans(session_id, start_ts);
CREATE INDEX IF NOT EXISTS idx_metric_points_session_ts  ON metric_points(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_sessions_last_seen        ON sessions(last_seen);
-- NOTE: idx_sessions_repo (on the v2 repo column, PLAN v3 §3) is intentionally
-- NOT created here. schema.sql is applied unconditionally on every Open BEFORE
-- migrations run (store.Open), so an index referencing the repo column would fail
-- against a pre-v2 on-disk DB whose sessions table predates that column. The repo
-- index is therefore created by the v1->v2 migration (existing DBs) and by the
-- fresh-DB stamp path in migrate.go (new DBs), keeping both shapes identical.

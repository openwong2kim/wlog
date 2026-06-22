# Changelog

All notable changes to wlog are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-06-22

### Added

- **Daily usage heatmap** — a GitHub-style "잔디" contribution grid of tokens (or
  cost) per local calendar day, with a tokens/cost toggle and the shared period
  filter sizing the grid.
- **Shared period filter** (`7d · 30d · 90d · 1y · All`) across Now, Sessions,
  Cost, Daily, and Tools; plus per-model and hour/day-bucket filters on Cost.
- **Native desktop app** (Tauri) that bundles `wlog` as a sidecar: one window
  starts the local server and UI and tears it down on close — no terminal needed.
- **Single static binary** OTLP receiver + embedded web UI for Claude Code
  telemetry. No daemon, Docker, or external database. `CGO_ENABLED=0` build
  using pure-Go `modernc.org/sqlite`.
- **OTLP ingestion** over gRPC (`:4317`) and HTTP (`:4318`) for all three
  signals — metrics, logs (events), and traces (spans stored raw in M1).
  Loopback bind by default with a Host-header check (DNS-rebinding guard);
  non-loopback binds require `--unsafe` or `--auth-token`.
- **Backpressure that never silently drops**: a bounded internal queue signals
  retryable rejection (gRPC `Unavailable` + RetryInfo / HTTP `503` + `Retry-After`)
  when full, so OTLP clients retry and no data is lost. Message-size limits and
  gzip-bomb protection guard the receiver.
- **Delta temporality normalization**: cumulative/delta Sum data points are
  normalized into per-interval deltas with persistent `series_state`,
  reset/stale/first-observation handling, and idempotent dedup
  (`UNIQUE(series_key, start_unixnano, time_unixnano)`).
- **Single-source-per-axis aggregation** to avoid double-counting: tokens/cost
  prefer `api_request` events and fall back to metrics; active time, lines of
  code, and decision counts always come from metrics; tool decisions/results
  always from events.
- **Persistent SQLite storage** with a single-writer goroutine, a WAL read pool,
  schema migrations with pre-migration backup and single-transaction rollback,
  and `ON DELETE CASCADE` retention cleanup (default: 90 days or 1000 sessions,
  whichever comes first).
- **Embedded web UI** (Vanilla TS + Vite + uPlot, no framework) with Onboarding,
  Now (live SSE tail), Sessions, Cost, Tools, and Timeline screens. WCAG 2.1 AA,
  `prefers-color-scheme` theming, hash routing, fully offline.
- **Privacy-first defaults**: prompts and tool arguments are received only when
  Claude Code opts in, and `--no-store-prompts` refuses to persist them. A
  three-stage in-UI privacy banner surfaces logging and bind status.
- **JSON HTTP API**: `/api/health`, `/api/sessions`, `/api/sessions/:id`,
  `/api/sessions/:id/timeline`, `/api/cost`, `/api/tools`, `/api/now` (SSE),
  `/api/export`, and `/api/setup`.
- **CLI** (`cobra`): default receiver+UI run, `wlog tail`, `wlog export`,
  `wlog version`, and `wlog --print-claude-setup`. Full flag surface for ports,
  bind/auth, database path/memory, retention, and receive limits, plus `WLOG_*`
  environment overrides.
- **Dogfooding replay harness** (`tools/replay`) that generates and replays
  synthetic Claude Code OTLP sessions over gRPC/HTTP for end-to-end verification.
- **Build & release tooling**: `Taskfile.yml` (go-task), `.goreleaser.yaml`
  (GoReleaser v2 with UI prebuild hook, SBOM, Homebrew tap, and Scoop bucket),
  and a GitHub Actions CI pipeline (vet, race tests, cross-compile matrix,
  staticcheck).

### Hardening (post-review QA + Codex implementation review)

- **Auth is now enforced**: `--auth-token` rejects non-loopback requests lacking a
  valid `Authorization: Bearer` (or SSE `?token=`) on both the API and the OTLP
  receiver (gRPC interceptor + HTTP); loopback peers stay exempt for local use.
- **Redaction is complete**: `--no-store-prompts` now also strips raw
  `tool_parameters`/shell commands/prompts from stored attribute JSON.
- **DB file permissions** set to `0600` on creation (POSIX).
- **Crash/shutdown safety**: delta normalization is transactional (rolled back on
  write failure so no counter drift), accepted requests commit on an independent
  write context (no loss on SIGINT), and orphaned `series_state` is pruned on
  retention.
- **Aggregation correctness**: `/api/cost` selects event-vs-metric source
  per session (metrics-only sessions no longer disappear); model breakdown matches
  the session's cost source; NaN/Inf metric points are rejected.
- **Robustness**: raw (pre-decompression) request size is capped via
  `MaxBytesReader`; records without a session id no longer roll back the whole
  batch; explicit flags always beat `WLOG_*` env; SSE drop markers no longer
  under-report. Single `wlog: error:` prefix on CLI errors.

### Visualization (post-dogfood, multi-lens critique)

- **Cost screen is now about money, not just tokens**: a cumulative-spend step line
  plus `total spend` and `burn rate` ($/min · tok/min) headline numbers; the
  input/output token chart is demoted below them.
- **Cache savings in dollars**: the cache-hit-ratio card now shows an estimated
  `~$X saved` derived from a built-in per-model price table (`internal/pricing`),
  labeled `(est.)`.
- **Model breakdown** share column rendered as an inline proportional monochrome bar.
- **Now screen burn rate**: live `$/min · tok/min` computed client-side from a
  rolling window of SSE ticks (no backend change); shows `—/min` when idle.
- **Live error salience**: `api_error` and failed `tool_result` lines in the Now
  tail are prefixed with `✕` and rendered in the reject amber (color + symbol).
- **Fix**: reject-hotspot bar widths are now proportional to the rejection count
  (equal counts render equal bars).

### Known limitations

- OTLP `partial_success` is returned empty; permanently-rejected data points are
  surfaced via `/api/health` `rejected_permanent` counters (the retryable
  backpressure model is the primary delivery guarantee).
- `go test -race` runs in CI (Linux) only; the local Windows toolchain has no C
  compiler, so the project builds and tests with `CGO_ENABLED=0`.

[Unreleased]: https://github.com/openwong2kim/wlog/commits/main

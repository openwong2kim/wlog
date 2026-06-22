# Contributing to wlog

Thanks for your interest in wlog! This is a single-binary, local-only tool, so
the contribution loop is short.

## Requirements

- **Go 1.26+**
- **Node 22+** (only to build the embedded web UI)
- Always build with **`CGO_ENABLED=0`** — the SQLite driver is pure Go, so no C
  toolchain is needed.

## Build & run

```bash
# 1. Build the embedded UI (outputs into internal/web/dist)
cd ui && npm ci && npm run build && cd ..

# 2. Build / run the binary
CGO_ENABLED=0 go build -o wlog ./cmd/wlog
./wlog
```

`internal/web/dist` is committed so `go install` works without Node. If you change
anything under `ui/`, rebuild the UI and commit the regenerated `dist` in the same
PR.

A `Taskfile.yml` is included if you use [go-task](https://taskfile.dev):
`task build`, `task run`, `task test`, `task cross`.

## Tests

```bash
go vet ./...
go test ./...
CGO_ENABLED=1 go test ./... -race   # the race detector needs cgo + a C compiler
```

Please add tests with behavior changes: a regression test for bug fixes, and tests
covering **both** branches when you add a conditional. Don't commit code that makes
existing tests fail.

## Conventions

- Match the surrounding code's style, naming, and comment density.
- Conventional commit prefixes (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`,
  `refactor:`, `test:`) — they drive the release changelog.
- Keep the UI text- and whitespace-forward (monospace numbers, single accent,
  minimal motion); see existing views under `ui/src/views`.

## Pull requests

1. Fork and branch from `main`.
2. Make the change with tests; run `go test ./...` and `npm run build`.
3. Open a PR describing **what** changed and **why**, with before/after notes or a
   screenshot for UI changes.

## Releases

Releases are automated: pushing a `v*` tag runs GoReleaser
(`.github/workflows/release.yml`), which builds the cross-platform binaries,
checksums, and SBOM and publishes the GitHub release. Rebuild + commit the UI
`dist` before tagging.

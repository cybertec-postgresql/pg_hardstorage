# v1.5.0 release readiness — integration/v1.5.0

Working notes for the release. Delete this file before tagging; it is
scaffolding, not documentation.

## What the branch contains

- `fix/56-wal-stream-systemd-unit` — issue #56 plus the four Dependabot
  merges (#57-#60)
- `fix/llm-streaming-timeout` — three LLM-helper fixes
- the changelog + env-var documentation for both

## Verified on this branch

| Gate | Result |
|---|---|
| `go build ./cmd/... ./internal/... ./compat/...` | pass |
| `go vet` (same trees) | pass |
| `gofmt -l cmd internal compat` | clean |
| `go test ./cmd/... ./internal/... ./compat/...` | **pass** |
| `mkdocs build --strict` | clean |
| `govulncheck -mode=binary` | **0 reachable** |

## Still to run before tagging

These were NOT run on this branch, because each rebuilds
`bin/pg_hardstorage` and an LLM evaluation run was using that binary:

- [ ] `make test-release-gate`
- [ ] `make test-scenarios` (needs `postgresql-client-17` for a clean
      174/174 — 6 scenarios fail and 9 skip on a host without it)
- [ ] `./run_compat_testing.sh --matrix smoke` (`default`/`wide` need an
      amd64 host: `archlinux:latest` has no arm64 image)
- [ ] `./run_testing.sh 34 2h` (caps to 21 cells on arm64 — the full
      arm64 matrix)

## Release mechanics

Version comes from `git describe` via ldflags; there is no version
string to bump. `debian/changelog` is stale at 0.1.1 and is not part
of the ritual — nfpm builds the packages.

1. Cut `## [Unreleased]` to `## [1.5.0] — <date>`, `make sync-llm-docs`
2. Write `docs/release-notes/v1.5.md`, add to `release-notes/index.md`
   and the `mkdocs.yml` nav
3. PR into `main` (the org ruleset requires one approval from someone
   other than the last pusher — it cannot be self-merged)
4. Annotated tag `v1.5.0`; pushing it fires release.yml, docs.yml and
   docs-doctest.yml

`PUBLISH_CONTAINERS` is `true` and working as of v1.4.2, so images
publish without further intervention.

## Why 1.5.0 and not 1.4.3

Minor, on the same precedent as v1.3.0 and v1.4.0: behaviour changes
that automation can observe. Nothing on disk or on the wire changes.

- a new systemd unit ships (`pg_hardstorage-wal-stream@.service`)
- the LLM system prompt is ~2.4x smaller, so answers differ in
  latency and, where the omitted help mattered, in content
- a new env var (`PG_HARDSTORAGE_LLM_HOT_HELP_BYTES`)

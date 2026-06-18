# server/

The REST HTTP server. Agents and `pg_hardstorage server` operators talk to this;
nothing else.

## What lives here

`net/http` mux wiring, request handlers, mTLS + bearer-auth glue, the pluggable
jobs backend (memory or Postgres), and the deletion sweeper. The Go gRPC server
is unbuilt — proto contracts live under `../../proto/` but there's no server
code here yet.

## Key files / subdirs

- `server.go` — `Server` struct, listener, TLS config, lifecycle
- `routes.go` — mux registration. Ten routes:
  - `GET /v1/healthz`, `GET /v1/readyz` (unauthenticated)
  - `GET /v1/version`
  - `/v1/deployments`, `/v1/deployments/`
  - `/v1/agents`, `/v1/agents/heartbeat`
  - `/v1/jobs`, `/v1/jobs/`, `/v1/jobs/claim`
- `agents.go` — register + heartbeat handlers; agent presence table
- `jobs.go` — job submit/list/get/cancel handlers
- `jobs_backend.go` — backend interface (`SubmitJob`, `ClaimJob`, `UpdateJob`)
- `jobs_memory.go` — in-process backend for tests + single-node
- `jobs_pg.go` — Postgres-backed backend for production
- `storage_glue.go` — bridges incoming job specs to the storage plugin
  registry
- `sweeper_*.go` — background reaper for completed/expired jobs

## Auth

Every authenticated route is wrapped by `s.requireAuth(...)` in `routes.go`.
mTLS is enforced at the listener; bearer tokens are a fallback path for tooling.

## Read next

- `../agent/README.md` — the primary client of these endpoints
- `../../proto/` — gRPC contracts (server side TBD)
- `../../api/openapi.yaml` — the published REST schema

## Don't put X here

- Long-running job execution — handlers enqueue, agents execute.
- CLI logic — handlers should not import `internal/cli`.
- Direct PG-wire calls — go through `internal/repo/` or job dispatch.

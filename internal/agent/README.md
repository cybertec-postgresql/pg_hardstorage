# agent/

The long-lived supervised process that runs on every PG host. It claims jobs
from the server, executes them, heartbeats, and reports.

## What lives here

The controlplane loop (claim → route → execute → ack), per-verb executors,
and the supervisor's job lifecycle. No HTTP server lives here — that's
`internal/server/`. The agent is the *client* of that server.

## Key files / subdirs

- `controlplane.go` — main loop: heartbeat + job poll + RBAC + fleet-view
  ingest
- `executor.go` — generic job runner: takes a claimed job, dispatches to the
  typed executor
- `router_executor.go` — verb → executor dispatch table
- `restore_executor.go` — invokes the restore pipeline under the agent's RBAC
  context
- `verify_executor.go` — invokes verify/scrub under agent RBAC
- `*_integration_test.go` — exercises the loop against a real `server/` over
  loopback

## Lifecycle

1. Process starts, loads config, registers with `/v1/agents`.
2. Heartbeats to `/v1/agents/heartbeat` on a fixed cadence.
3. Polls `/v1/jobs/claim` for assigned work.
4. Routes the claimed job through `router_executor` to a typed executor.
5. Streams progress + final result back to `/v1/jobs/<id>`.
6. On SIGTERM, drains current job (best-effort), unregisters, exits.

## Read next

- `../server/README.md` — the HTTP surface this process talks to
- `../cli/agent.go` — the `agent` verb that launches this loop
- `../approval/` — gating for high-impact jobs the agent refuses to
  auto-execute

## Don't put X here

- HTTP routing — that lives in `internal/server/`.
- Job storage backends — `internal/server/jobs_*.go` owns that.
- Verb-specific business logic — call into `internal/restore/`,
  `internal/repo/`, etc.

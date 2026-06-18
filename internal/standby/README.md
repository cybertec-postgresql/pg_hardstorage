# standby/ (v0.5)

Hot-standby PostgreSQL fed entirely by the backup pipeline: a
continuously-updated read-only replica with no streaming-replication path back
to the primary.

**Today:** state machine + repo glue. `standby create` restores the latest
committed backup into the target dir, writes `standby.signal`, and configures
`restore_command` to pull newly-archived WAL from the same repo. State is
tracked in a file so `standby list` and `standby destroy` work.

**Coming (v0.5):** full bring-up wiring — automatic PG start, primary_conninfo
streaming when co-located with the agent, replica-slot acknowledgements +
`hot_standby_feedback`.

## Why fed from the repo, not the primary

Repository-fed standby decouples the replica from the primary's wire-protocol
connectivity. The agent's existing WAL pipeline already puts WAL in the repo;
the standby's `restore_command` pulls from the same place. Zero new traffic on
the primary, zero new failure modes that aren't already exercised by `restore`.

## Key files

- `standby.go` — `Create`, `List`, `Destroy`, state file
  (`paths.State()/standby.json`)
- `repo_glue.go` — repository-side bookkeeping for the bring-up
- `standby_test.go` / `internal_helpers_test.go` / `bringup_integration_test.go`
  — lifecycle + bring-up coverage

What v0.1 deliberately does NOT do: start the PG process (the operator brings PG
up via systemd / pg_ctl / Docker; the Result body emits the recommended
invocation), stream WAL via `primary_conninfo`, manage replica-slot acks.

## Read next

- `../timetravel/README.md` — sibling: pinned-in-time read-only PG (timetravel
  diverges from production; standby follows)
- `../restore/README.md` — the restore primitives standby calls into
- `../wal/inventory/` — the WAL coverage `restore_command` pulls from
- `../README.md` — parent index

## Don't put X here

- Streaming-replication wire code — that's not how this surface works; it's
  repo-fed.

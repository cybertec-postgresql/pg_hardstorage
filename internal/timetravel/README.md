# timetravel/ (v1.0)

Ephemeral read-only PostgreSQL pinned to any historical LSN — resolves
natural-time, RFC3339, LSN, or backup-id targets, with a TTL'd session state
file.

**Today:** target resolution + restore + recovery-signal wiring. `timetravel
create db1 --at "2026-04-01T00:00:00Z" --target /var/tmp/db1@apr1` restores the
backup containing the target LSN, writes `recovery.signal`, configures
`restore_command`, and sets `recovery_target_*` so PG enters recovery, replays
to the target, and pauses (no auto-promotion — keeps the cluster readable
without diverging from production).

**Coming (v1.0):** auto-tear-down daemon, supervisor integration, named
natural-time shorthands beyond the v0.1 set, multi-target sessions.

## Timetravel vs standby

A standby follows production indefinitely; a timetravel session is pinned at a
moment and self-expires. They share `internal/restore` plumbing but keep
separate state files because their lifecycles diverge.

## Key files

- `timetravel.go` — `Create`, `List`, `Cleanup`, target resolution (RFC3339 /
  natural-time / LSN / backup-id), TTL state
- `repo_glue.go` — repository-side bookkeeping
- `timetravel_test.go` — target resolution, recovery-signal generation, TTL
  sweep

What v0.1 deliberately does NOT do: start the PG process (operator runs `pg_ctl
-D <target> ...` from the Result body), daemon-driven auto-tear-down
(`timetravel cleanup` is a manual sweep, optionally wired to cron).

## Read next

- `../standby/README.md` — sibling: live-following replica (timetravel is the
  pinned-in-time variant)
- `../restore/naturaltime/` — natural-time parser shared with `restore --to`
- `../restore/README.md` — restore primitives this surface calls into
- `../../docs/explanation/wal-pipeline.md` — WAL replay and PITR background
- `../README.md` — parent index

## Don't put X here

- PG process supervision — out of scope until v1.0.
- Long-term snapshot storage — timetravel sessions are ephemeral by design.

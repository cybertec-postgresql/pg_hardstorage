# pg/testkit/

The Postgres test fixture: `StartPostgres(t)` returns a configured, throw-away
PG container with a connection string. Used by every `//go:build integration`
test in the repo.

## What lives here

A thin wrapper over `testcontainers-go` that boots a real Postgres in Docker,
applies the settings `pg_hardstorage` needs for its integration tests
(`wal_level=logical`, `max_wal_senders`, a replication role, `summarize_wal=on`
on PG 17+), and tears it down when the test ends. No mocks — every
wire-protocol test runs against a real server.

## Key files

- `pg.go` — `StartPostgres(t *testing.T, opts ...Opt) *Postgres`; `*Postgres`
  exposes `DSN()`, `ReplicationDSN()`, `Version()`, `Exec`, `Stop`. Options
  select PG major version, extra GUCs, init SQL, network mode

## Usage

```go
//go:build integration
// +build integration

func TestThing(t *testing.T) {
    pg := testkit.StartPostgres(t, testkit.WithVersion(17))
    defer pg.Stop()
    // ... drive pg.DSN() / pg.ReplicationDSN()
}
```

## Default GUCs applied

- `wal_level=logical`
- `max_wal_senders >= 10`
- `max_replication_slots >= 10`
- `summarize_wal=on` (PG 17+; required for incremental backups)
- a replication role with login + `REPLICATION`

## Read next

- `../basebackup/integration_test.go` — a typical consumer
- `../replication/integration_test.go`, `../logicalreceiver/integration_test.go`
  — more
- `../../backup/README.md` — most outer integration tests still use this
  fixture
- `../../wal/README.md` and `../../restore/README.md` — same

## Don't put X here

- Unit-test helpers — `testkit` is integration-only; unit tests should not
  pull this in (`//go:build integration` guards prevent it).
- Production code — fixtures only; nothing here ships in a release binary.
- Multi-node / Patroni clusters — out of scope; use `test/fleets/` for that.

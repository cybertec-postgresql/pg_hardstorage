# patroni/

A small REST client for Patroni's HTTP API — just enough to make
`pg_hardstorage` HA-aware without taking a hard dependency on Patroni itself.

## What lives here

A typed wrapper over `/cluster`, `/leader`, `/replica`, `/history`, and friends,
plus the long-polling watcher the backup runner uses to suspend during a
failover. The slot-continuity check (`EnsureSlot`) that detects when Patroni
recreated a physical slot underneath us lives one layer down in
`internal/pg/replication/`; this package is just the HTTP client +
change-watcher.

## Key files

- `client.go` — `Client.Cluster()`, `Leader()`, `IsLeaderCheck()`,
  `History()`; context-aware, TLS-aware, sane timeouts; `BaseURL()` exposed for
  diagnostic events (issue #74)
- `client_test.go` — table-driven tests against recorded fixtures; pins the
  `ErrUnreachable` wrap shape that preserves the underlying transport error
- `follower.go` — long-poll watcher that fires on leader change, replica
  promotion, or scope membership drift
- `follower_test.go` — covers reconnect, jitter, and stale-leader edge cases

## Read next

- `../pg/replication/README.md` — `EnsureSlot` + slot-recreation gap detection
  live there
- `../config/README.md` — `DeploymentConfig.Patroni`
- `../wal/README.md` — leader-follow coordinator that consumes this client
- `../standby/README.md` — failover-aware backup scheduling

## Don't put X here

- DCS (etcd/Consul/ZK) clients — talk to Patroni's REST API, not the DCS
  directly.
- Patroni configuration generation — `pg_hardstorage` reads Patroni state,
  never writes it.
- Cluster-state caching — this is a thin client; cache one level up if you
  need it.

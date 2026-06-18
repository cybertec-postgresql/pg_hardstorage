# topology

PG infrastructure-provider abstraction — a `Topology` brings up a PG (single
instance, replica pair, …), exposes a connection string, and tears it down
again.

## What lives here

A scenario's `topology:` block picks a provider; the runner calls `Up`, runs the
scenario, calls `Down`. Today two providers are real (`local-docker` and
`patroni-local-docker` via docker-compose); five stubs (`kind`, `k8s-remote`,
`ssh-inventory`, `cloud-vms`, `firecracker`) return a structured error from `Up`
so a scenario referencing an unimplemented provider fails fast with a clear
message rather than running against the wrong topology.

`local_docker.go` is the dominant provider for L1–L3. It uses
testcontainers-go and auto-enables the GUCs scenarios need:

- `wal_level=logical`
- `wal_keep_size=256MB`
- `summarize_wal=on` (PG 17+, required for incremental backup)

Scenario-level `extra_gucs` are merged on top so an individual scenario can flip
additional settings without touching the provider code.

## Key files / subdirs

- `topology.go` — `Topology` interface, provider registry, stub providers
- `local_docker.go` — single PG via testcontainers-go; auto-GUC + `extra_gucs`
  merge
- `patroni.go` — 3-node Patroni cluster via docker-compose
- `patroni_integration_test.go` — failover/leader-rotation integration tests

## Read next

- `../sink/README.md` — sibling package for storage backends
- `../runner/runner.go` — how the runner consumes a `Topology` across a
  scenario lifecycle
- `dockerfiles/testbed/` — the testbed images these topologies launch

## Don't put X here

No load or assertion logic. A topology is responsible for bringing PG up to a
usable state and exposing a DSN — nothing else.

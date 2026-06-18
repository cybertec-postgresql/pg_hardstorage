# inject

Fault-injection registry — the catalogue of named faults a scenario's
`inject:` step (or a soak run) can apply against the running fleet.

## What lives here

Each `Fault` implements `Apply(ctx, args, targets)` and returns a `Recovery`
callback the driver invokes after a heal window. Some faults are inherently
irreversible (e.g. `signal sig=9` — the process is dead, no recovery is
possible) and return `NoRecovery`. Targeting is a separate concern: `target.go`
resolves selectors like `agent_random`, `pg_random`, `repo` into a concrete
`TargetSet` the fault then operates on.

Adding a new fault is one Go type plus one registry entry: implement `Name()` to
match the YAML key, implement `Apply` to perform the action, return a `Recovery`
(or `NoRecovery`). A scenario references the fault by name plus key=value
arguments parsed from its `inject:` step.

## Key files / subdirs

- `inject.go` — `Fault` and `Recovery` interfaces, registry, dispatch
- `faults.go` — core in-process faults: `signal`, `disk_full`,
  `cgroup_squeeze`, `network_block`
- `faults_extra.go` — extended catalogue: `toxiproxy`, `sql`, `libfaketime`,
  …
- `parse.go` — parser for the `kind=name [k=v ...]` action string
- `target.go` — selector resolver (`agent_random`, `pg_random`, `repo`, …)
- `target_docker.go` — Docker-targeting back-end (looks up containers by
  project label)
- `target_fake.go` — in-memory fake targets used by unit tests
- `faults_extra_test.go`, `inject_test.go`, `parse_test.go`,
  `target_docker_test.go` — per-source unit tests

## Read next

- `../runner/steps.go` — how the `inject:` step kind dispatches into this
  package
- `../validate/README.md` — how the soak orchestrator picks + applies faults
  on its loop
- `../topology/README.md` — the topology that owns the targets these faults
  aim at

## Don't put X here

No PG-cluster lifecycle (that's `topology/`). No storage-sink lifecycle (that's
`sink/`). Faults are pure verbs — they apply, they recover, they don't bring
infrastructure up or down.

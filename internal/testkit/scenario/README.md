# scenario

YAML parser + schema for `pg_hardstorage.scenario.v1` — the testkit's
top-level executable unit.

## What lives here

A scenario file declares which topology to bring up, which load file to run,
which steps to execute, and what to assert. This package parses that YAML into
Go structs the runner consumes. The parser is KnownFields-strict (same posture
as the load parser): typos in step names, assertion kinds, or top-level keys
fail loudly rather than silently no-op'ing.

Steps use the single-key-map shape:

```yaml
steps:
  - take_backup: { name: full_0 }
  - run_load:    { duration: 30s }
  - restore:     { from: full_0, into: ./out }
```

A custom `UnmarshalYAML` on `Step` handles that shape — the YAML key chooses
the step kind, the value is the kind-specific payload. The same shape is used
for the `asserts:` block, with assertion kinds keyed by name.

## Key files / subdirs

- `scenario.go` — `Scenario`, `Topology`, `Step` structs + custom step
  `UnmarshalYAML`; `SchemaScenario = "pg_hardstorage.scenario.v1"`
- `scenario_test.go` — schema round-trip tests + step-shape edge cases

## Read next

- `../runner/README.md` — how the parsed scenario is executed
- `../assert/README.md` — the assertion DSL referenced from the `asserts:`
  block
- `test/scenarios/README.md` — the on-disk scenario corpus and tier
  conventions

## Don't put X here

No execution logic — parsing only. No assertion semantics — those live in
`assert/`. No validation of cross-step references (e.g. "this `restore` step
names a backup no prior step produced") — that's a runner-level pre-flight
check at start-of-Run, not a parser concern.

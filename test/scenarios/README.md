# scenarios

All `*.scenario.yaml` files driven by `pg_hardstorage_testkit scenario run`.
Currently 173 scenarios across 8 tiers.

## What lives here

One file per scenario. Naming convention:
`<tier>_<topic>_<variant>.scenario.yaml`. Each file declares `schema:
pg_hardstorage.scenario.v1`, picks a topology, references a load file under
`test/load/`, runs steps, and asserts. Recently-added incremental-backup suite
is documented in the commit log; cross-cluster DR + operator-shim coverage lives
under L5–L8.

## Tiers

| Tier | Budget    | When to run                                          |
|------|-----------|------------------------------------------------------|
| L1   | ~5 min    | PR gate; every push                                  |
| L2   | ~30 min   | Nightly main; pre-merge for non-trivial PRs          |
| L3   | ~2 h      | Nightly main; weekly release candidate              |
| L4   | ~8 h      | Weekly main; on-demand for storage-layer changes     |
| L5   | ~24 h     | Pre-release; k8s scenarios spec-only today (see note) |
| L6   | multi-day | Pre-release; long-soak DR exercises                  |
| L7   | multi-day | Pre-release; compat-shim matrix vs CNPG/Crunchy/Zalando |
| L8   | ~24 h     | Release candidate; full operator-driven matrix sweep |

Current count per tier: L1=8, L2=59, L3=31, L4=20, L5=6, L6=3, L7=8, L8=36.

> Note: k8s scenarios in L5 are **spec-only** today — the `kind` topology is
> unbuilt, so they parse + validate but `Up` fails fast. They serve as a
> standing target while the topology backend lands.

## Read next

- `../load/README.md` — the load corpus referenced from each scenario's
  `load:` block
- `internal/testkit/scenario/README.md` — YAML schema definition
- `internal/testkit/runner/README.md` — what each step kind does

## Don't put X here

No load files (those go in `../load/`). No fixtures or operator-argv corpora
(those go in `../fixtures/`). One scenario per file — composing several
scenarios into a suite is the job of the matrix scheduler, not a single YAML.

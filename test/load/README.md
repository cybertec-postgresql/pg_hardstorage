# load

Deterministic load files. Schema `pg_hardstorage.testload.v1`. Each scenario's
`load:` block names one of these.

## What lives here

Two files today. Phases declare ops (`create_table`, `insert_rows`,
`update_rows`, `create_index`, `vacuum`, …). Row content is generated from a
chacha20 PRNG seeded by the file's `seed:`, so a re-run with the same seed
produces byte-identical rows.

## Re-runnability rule

The runner's `run_load { duration: Xs }` step can re-apply a load file
repeatedly across the wall-clock budget. If the file is **not** designed for
re-runnability, the second pass will trip a uniqueness constraint or a `relation
already exists` error and the scenario fails for the wrong reason.

A re-runnable load file:

- Uses `CREATE TABLE IF NOT EXISTS` for DDL (idempotent).
- Avoids `UNIQUE` constraints on columns the deterministic generators repeat
  across passes (e.g. faker emails — pass 2 emits the same
  `user-N@example.test` and trips the unique index).
- Picks indexes that tolerate duplicates (plain btree on `created_at`, not
  unique on email).

Concrete contrast:

- `oltp_smoke.load.yaml` — **one-shot**, has a `UNIQUE` index on email. Fine
  for the L1 PR-gate scenario that applies the load exactly once and asserts the
  digest matches.
- `failover_loop.load.yaml` — **re-runnable**, no unique indexes, deliberately
  shaped for the L4 patroni-failover-loop scenario that loops the load across
  multiple leader changes. Each pass appends ~1000 rows.

When adding a new load file, state in its header comment which mode it supports.

## Read next

- `internal/testkit/load/README.md` — engine internals, op catalogue, PRNG
- `../scenarios/README.md` — the scenarios that reference these files

## Don't put X here

No scenario YAML. No DDL-only fixtures (load files always declare a phase order,
not raw SQL).

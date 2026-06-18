# load

The testkit's deterministic load engine — a `*.load.yaml` file declares phases
of operations against PG; the engine drives PG through them with a chacha20 PRNG
so the bit-exact same sequence of rows lands on every run with the same seed.

## What lives here

Each load file declares `schema: pg_hardstorage.testload.v1`, a `seed:`, a
locale, and a list of phases. Phases contain ops:

- `create_table {name, schema}`
- `insert_rows {table, count, generator}`
- `update_rows {table, fraction, where}`
- `delete_rows {table, fraction, where}`
- `create_index {table, columns, unique?}`
- `vacuum {table?, full?}`
- `checkpoint {label}`

Generators (`faker_users`, `faker_orders`, …) emit row content
deterministically from the PRNG. The checkpoint NDJSON stream emitter writes a
canonical record of every row inserted, so restore-verify can re-derive
byte-equality without re-running the load against a fresh PG. Heavier ops
(`copy_in` via COPY FROM, `alter_table`, `reindex`) land here as the verifier
matures.

## Key files / subdirs

- `load.go` — YAML parser, op registry, top-level `Apply`
- `checkpoint/` — NDJSON checkpoint stream emitter for restore-verify
- `prng/` — chacha20-stream PRNG seeded by the load file's `seed:` field
- `load_test.go` — schema + op round-trip tests

## Read next

- `test/load/README.md` — the on-disk load corpus + the re-runnability rule
- `../runner/restored_load.go` — how the runner replays load against a
  restored PG to compare digests
- `../scenario/README.md` — how a scenario references a load file

## Don't put X here

No PG-version-specific quirks — the load engine targets plain SQL and trusts
the topology to be configured (replication slots, `wal_level`, …) before it
runs. No randomness sourced from `math/rand` or `crypto/rand` — every byte
comes from the seeded PRNG so a re-run is byte-identical.

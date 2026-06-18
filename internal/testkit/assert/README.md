# assert

The testkit's declarative assertion DSL — a scenario file's `asserts:` block
is a list of single-key maps where the key names the assertion kind.

## What lives here

The runner invokes each assertion against the scenario context (live `*sql.DB`,
repo path, last backup ID, captured LSN, …). Each `Result` records pass/fail
plus a human-readable message; failures don't short-circuit by default so an
operator gets every diff in one shot. Adding a new assertion is one Go type +
one registry entry plus a YAML key — same pattern as inject's `Fault`.

Example block:

```yaml
asserts:
  - count_exact:   { table: users, value: 1000000 }
  - count_range:   { table: orders, min: 800000, max: 900000 }
  - digest_match:  { table: users, columns: [id, email], algo: crc64iso,
                     expected: "abc..." }
  - lsn_at_least:  "0/3F5A1B40"
  - pg_amcheck:    { passes: true }
  - audit_chain_intact: true
  - no_orphan_chunks:   true
```

## Key files / subdirs

- `assert.go` — DSL kinds + registry. Currently: `count_exact`, `count_range`,
  `lsn_at_least`, `audit_chain_intact`, `pg_amcheck`, `pg_verifybackup`, `sql`,
  `digest_match`, `page_aware_hash_match`, `schema_fingerprint_match`,
  `no_orphan_chunks`, `no_uncommitted_manifests`, `prom_metric`
- `assert_test.go` — per-kind unit tests

## Read next

- `../scenario/scenario.go` — the YAML the `asserts:` block lives in
- `../runner/runner.go` — how each `Assertion` is invoked at scenario tail and
  at `assert:` steps

## Don't put X here

No PG fixture setup or topology orchestration. Assertions are read-only
verifiers — they observe state and report; they never mutate the cluster,
repo, or filesystem.

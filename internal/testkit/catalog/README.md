# catalog

The testkit's source-of-truth for supported (OS × PG version × architecture)
combinations and the operator drop-in matrix.

## What lives here

`oses.yaml` and `operators.yaml` are `go:embed`'d into the binary so a
checked-out copy of `pg_hardstorage_testkit` always carries the catalog it was
built with. Tests load the embedded catalog directly; operators wanting to
override (internal distro fork, in-house operator variant) point
`PG_HARDSTORAGE_TESTKIT_CATALOG` at an alternative path.

The catalog is intentionally small — ~10 distros × ~4 PG versions × 2 arches
≈ ~70 cells. Wider matrices land here only after the image-build template
grows to support them.

`operators.yaml` documents the K8s operators the compat shims target:

- **CNPG** — `barman-cloud-*` binaries → `pg-hardstorage-barman` +
  `-wal-archive`
- **Crunchy** — `pgbackrest` binary → `pg-hardstorage-pgbackrest`
- **Zalando** — `wal-g` binary → `pg-hardstorage-walg`

The operator's own backup CRD fires the backup; the operator doesn't know it's
not talking to the original tool — that's the drop-in-replacement claim tested
honestly.

## Key files / subdirs

- `catalog.go` — parser, `Default()` (embedded), and the `Catalog` type
- `oses.yaml` — `(OS × PG × arch)` matrix; family-level defaults; weekly
  drift-check against upstream PGDG
- `operators.yaml` — Kubernetes operator matrix used by `run_k8s_testing.sh`
- `catalog_test.go` — schema + lookup tests

## Read next

- `dockerfiles/testbed/` — the testbed images the catalog refers to
- `cmd/pg_hardstorage_testkit/fleet.go` — `fleet add` validates entries
  against this catalog
- `cmd/pg_hardstorage_testkit/image.go` — `image build --only os=<id>` pulls
  dimensions from here

## Don't put X here

No scenario enumeration — there is no scenario catalog; scenarios are walked
off disk under `test/scenarios/`. No image-build logic — that's the testbed
Dockerfile + `image build` subcommand.

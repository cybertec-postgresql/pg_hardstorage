# fixtures

Golden-output and operator-argv fixtures consumed by tests across `internal/`.

## What lives here

Today this directory holds only the operator-argv corpora — one subdirectory
per supported Kubernetes operator, each with its own README plus the actual
`argv-fixtures.ndjson` and `manifests/` the compat shims must accept verbatim.
Each NDJSON record captures one (operator, operator-version, PG-version,
scenario) argv sample as observed in the wild; the shims
(`cmd/pg-hardstorage-barman*`, `cmd/pg-hardstorage-pgbackrest`,
`cmd/pg-hardstorage-walg`) are tested against these to prove drop-in-replacement
parity.

The Zalando subdirectory carries a `-raw.ndjson` alongside the normalised file
so the original capture format remains reviewable after normalisation; the
others have only the normalised form.

This is the index — read the per-operator READMEs for the actual argv
specifications and the manifest samples that produced each capture.

## Pattern for adding more golden fixtures

When future test suites need other golden outputs (CLI render goldens,
audit-bundle samples, recovery-report fixtures), land them as sibling
subdirectories alongside `operator-argv/`, each with its own README. Keep this
top-level README as a thin index pointing at them.

## Key files / subdirs

- `operator-argv/cnpg/` — CloudNativePG (barman-cloud-* argv); see its README
- `operator-argv/crunchy/` — Crunchy Postgres Operator (pgBackRest argv); see
  its README
- `operator-argv/zalando/` — Zalando postgres-operator (WAL-G argv); has both
  `-raw.ndjson` and the normalised fixture; see its README

## Read next

- `operator-argv/cnpg/README.md`, `operator-argv/crunchy/README.md`,
  `operator-argv/zalando/README.md` — the per-operator argv documentation
- `internal/testkit/catalog/operators.yaml` — the matrix that decides which
  operator versions get tested
- `cmd/pg-hardstorage-barman/`, `cmd/pg-hardstorage-pgbackrest/`,
  `cmd/pg-hardstorage-walg/` — the compat shims consuming these fixtures

## Don't put X here

No scenario YAML (`../scenarios/`). No load files (`../load/`). No live PG dumps
or large binary blobs — fixtures here must be small, text-form, and reviewable
in a PR diff.

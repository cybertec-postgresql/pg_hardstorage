# config/

The single source of truth for `pg_hardstorage`'s declarative configuration:
schema, loader, validator, and drop-in merge order.

## What lives here

The `pg_hardstorage.config.v1` YAML schema parsed into `DeploymentConfig`, plus
the merge rules that turn `/etc/pg_hardstorage/pg_hardstorage.yaml` +
`/etc/pg_hardstorage/conf.d/*.yaml` + environment overrides into one validated
object. Schema versioning is explicit so older configs fail loud rather than
silently lose fields.

## Key files

- `config.go` — `DeploymentConfig` (TDE, Patroni, retention, classification,
  residency, SLO, sinks, plugins), `Load`, `Merge`, `Validate`
- `config_test.go` — round-trip + drift tests against the sample config in
  `share/pg_hardstorage.sample.yaml`
- `patroni_validation_test.go` — cross-field rules for the Patroni block (DCS
  URL set ↔ rest-api URL set, slot name vs scope)

## Drop-in merge order

1. Compiled-in defaults
2. `/etc/pg_hardstorage/pg_hardstorage.yaml` (or `--config`)
3. `/etc/pg_hardstorage/conf.d/*.yaml` — lexicographic, last-write-wins per
   key
4. `PG_HARDSTORAGE_*` env overrides
5. Command-line flags

## Read next

- `../paths/README.md` — where the loader looks
- `../patroni/README.md` — consumer of the `patroni:` block
- `share/pg_hardstorage.sample.yaml` — the canonical reference config
- `docs/reference/configuration.md` — user-facing key docs

## Don't put X here

- Plugin-specific config schemas — those live next to the plugin
  (`internal/plugin/<tier>/<name>/`).
- CLI flag parsing — that's `internal/cli`.
- Runtime mutable state — config is load-once, immutable thereafter.

# ext

PostgreSQL server-side extension that augments pg_hardstorage with in-database
hooks the external binary can't get from the wire protocol — currently catalog
introspection helpers used by chain verification and partial restore.

## What lives here

A standard PGXS-built extension (`.control`, versioned `.sql`, `Makefile`). The
Go side that loads and calls into this extension lives under
`../internal/dbext/`. Built artefacts ship in the `pg-hardstorage-extension`
package (Debian) and the matching RPM.

## Key files / subdirs

- `pg_hardstorage_extension/pg_hardstorage.control` — PostgreSQL extension
  manifest (name, default_version, schema)
- `pg_hardstorage_extension/pg_hardstorage--1.0.sql` — version 1.0 SQL:
  tables, functions, grants
- `pg_hardstorage_extension/Makefile` — PGXS build glue; honours `PG_CONFIG`
  for cross-major builds

## Read next

- `../internal/dbext/` — Go client that calls into the functions defined here
- `../test/scenarios/L2_db_extension.scenario.yaml` — end-to-end install / use
  / uninstall scenario
- `../docs/reference/build-flavours.md` — how the extension is matrixed across
  PG majors

## Don't put X here

- Client-side Go code — that's `../internal/dbext/`.
- Catalog-mutating migrations unrelated to the extension's surface area.
- Multi-version upgrade SQL without bumping `default_version` and adding a new
  versioned `.sql`.

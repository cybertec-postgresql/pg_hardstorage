# ext

PostgreSQL server-side extension that augments pg_hardstorage with
in-database backup-state introspection the external binary can't get
from the wire protocol: the `pg_hardstorage.backups` / `health` /
`rpo` views plus the `pg_hardstorage_writer` role and upsert
functions that write the backing tables.  Operator-facing today —
no built-in data-plane path calls the upserts yet, so the views
stay empty until one lands (or an operator upserts directly).

## What lives here

A standard PGXS-built extension (`.control`, versioned `.sql`, `Makefile`).
The Go side that installs this SQL (on-disk `CREATE EXTENSION` or inline
`db install-extension`) lives under `../internal/dbext/`.  The extension
ships as source under `ext/`; no dedicated `pg-hardstorage-extension`
Debian package exists — the `debian/` packaging (pg-hardstorage, -common,
-server, -compat-*) does not install the extension.

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

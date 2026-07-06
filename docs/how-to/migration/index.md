---
title: Migration how-to guides
description: Recipe pages for moving a deployment from another
              backup tool onto pg_hardstorage.
---

# Migration how-to guides

Move a deployment from another backup tool onto
pg_hardstorage. Every page in this section follows the
same dual-write + retention drain pattern: stand up
pg_hardstorage alongside the legacy tool, validate, pick a
cutover line, drain the legacy tool's retention floor.

No backup rewrite is required, and **no repo-format
conversion is planned** — legacy repo formats are
binary-tagged and undocumented externally; we have no
plans to read them. The pattern below treats every
migration as an **operational cutover**: run both tools
side-by-side during a retention window, then retire the
old repo when its window expires. Old backups stay
restorable by the legacy tool itself for as long as you
keep the binary around.

For pgBackRest and Barman, the one-shot config translator
(`pg_hardstorage compat translate --from ...`) ships in the
main `pg_hardstorage` binary today. The **drop-in shim
binaries** (`pg-hardstorage-pgbackrest`,
`pg-hardstorage-barman`) are **built from source** — their
`cmd/pg-hardstorage-*` packages live in the repo, but no
release artifact (tarball, `.deb`/`.rpm`, or container
image) ships them. Compile them yourself, then drop them on
PATH so existing cron jobs and `archive_command` settings
keep working but produce native pg_hardstorage backups. See
the per-tool pages.

## Pages

- [Migrate from pgBackRest](from-pgbackrest.md)
- [Migrate from WAL-G](from-walg.md)
- [Migrate from Barman](from-barman.md)

## Why dual-write rather than format conversion

Legacy repo formats aren't byte-compatible with ours.
Converting in place would mean re-chunking every backup
through FastCDC, re-signing manifests, re-encrypting under
the new envelope. Real work, proportional to repo size.

Dual-write costs nothing extra — pg_hardstorage's WAL slot
streams from PG independently of whatever the legacy tool
is doing. The transition window covers the legacy tool's
retention floor, then the legacy tool retires.

For cluster operators (Zalando, Crunchy PGO) who want to
keep the operator surface and just swap the backup binary
underneath, see the in-pod shim approach:
[pgBackRest shim](../kubernetes/pgbackrest-shim.md) /
[Barman shim](../kubernetes/barman-shim.md) /
[WAL-G shim](../kubernetes/walg-shim.md).

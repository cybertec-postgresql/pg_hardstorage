---
title: TDE awareness
description: How pg_hardstorage handles source PostgreSQL deployments with Transparent Data Encryption (CYBERTEC PGEE, pg_tde, EDB TDE) — and why most of the system needs no changes.
tags:
  - encryption
  - tde
  - cybertec-enterprise
  - pg_tde
  - compliance
---

# TDE awareness

PostgreSQL forks with Transparent Data Encryption (TDE) — most
notably **CYBERTEC PostgreSQL Enterprise Edition (PGEE)**, plus
`pg_tde`, EDB TDE, and the family of out-of-tree patches —
encrypt heap files, indexes, the control file, and WAL **at
rest**.  On disk those bytes are ciphertext; only PG itself,
holding the data-encryption key, can read them.

`pg_hardstorage` is byte-opaque almost everywhere by design.  The
chunker is content-defined (not page-aware), the storage layer
treats input as raw bytes, the WAL receiver relays records
without parsing.  Under TDE that all "just works": PGEE decrypts
at the replication boundary, so `BASE_BACKUP`,
`START_REPLICATION`, and `START_REPLICATION ... LOGICAL` deliver
plaintext over the wire and our pipeline never sees ciphertext.

But there is **one** code path where pg_hardstorage reads bytes
**directly off the source's filesystem**, and that path breaks
silently against ciphertext.  This page documents the contract,
the single flag that handles it, and the propagation to restore.

---

## The flag

Add to the deployment's `pg_hardstorage.yaml`:

```yaml
deployments:
  db1:
    pg_connection: postgres://backup@db1.example.com/postgres
    repo: s3://acme-pg-backups/
    tde:
      enabled: true
      engine: cybertec_enterprise   # free-form; informational
      key_ref: kms-secret://prod/pgee  # operator-supplied; informational
```

Or pass `--tde` on the relevant CLI surfaces (currently
`pg_hardstorage wal push`).  Default is `tde.enabled: false` —
the historical behaviour with strict on-disk header parsing.

Once set, the agent treats EVERY path that would otherwise parse
PG byte layout off disk as "ciphertext, don't peek".  The
relaxation is symmetric: backup-side, restore-side, and the
manifest stamps so future restores know.

---

## What changes under TDE

### `wal push` (`archive_command` target)

The canonical archive_command shape derives the cluster's
`system_identifier` from the first-page `XLogLongPageHeader` of
the segment file PG hands the archiver — saves a libpq round-trip
per push (issue #8).

Under TDE the segment file on disk is ciphertext.  Reading 32
bytes at offset 0 either fails the `XLP_LONG_HEADER` sanity check
(noisy) or, worse, **happens to satisfy it and stamps a bogus
xlp_sysid** on the segment manifest (silent corruption of the
repo's cross-cluster contamination guard).

With `--tde` (or `tde.enabled: true` in deployment config), `wal
push` **never reads the segment header**.  The operator MUST
supply the system identifier via one of:

```bash
# Option A — explicit (recommended for archive_command lines):
pg_hardstorage wal push db1 %p --repo s3://... --tde \
    --system-identifier 7388123456789012345

# Option B — libpq round-trip per push:
pg_hardstorage wal push db1 %p --repo s3://... --tde \
    --pg-connection postgres://backup@db1/postgres
```

Refusing without either fires `usage.missing_flag` with a
remediation pointer at this page.  That's deliberate: a silent
"works for the first segment, mis-stamps the second" failure
mode would be a compliance nightmare to debug after the fact.

To get the `system_identifier` once:

```sql
SELECT system_identifier FROM pg_control_system();
```

Then bake it into the `archive_command` line.

### Backup manifest stamping

Every backup taken from a TDE-declared deployment writes a
`SourceTDE` block on the manifest:

```json
{
  "schema": "pg_hardstorage.manifest.v1",
  "backup_id": "db1.full.20260527T093017Z",
  ...
  "source_tde": {
    "engine": "cybertec_enterprise",
    "key_ref": "kms-secret://prod/pgee"
  },
  ...
}
```

The block is informational (`pg_hardstorage` never branches on
`engine` or `key_ref` values).  Its purpose is to propagate the
posture to restore-time tooling and to the audit trail.

### Restore preflight

`pg_hardstorage restore` reads `Manifest.SourceTDE`.  When
non-nil:

- The restored data directory will contain ciphertext heap files,
  ciphertext WAL, an encrypted control file.  Booting it under a
  **vanilla PostgreSQL** (no TDE extension, no key access) will
  fail at startup with `FATAL: incorrect checksum in control
  file` or similar — the target PG sees random-looking bytes.
- The restored cluster MUST boot under a TDE-capable PG with
  access to the same key set.  This is the operator's
  responsibility; pg_hardstorage stamps the `engine` and
  `key_ref` so the operator (or an automation) can verify match
  before pointing PG at the restored dir.
- `pg_verifybackup` against the chain-flattened datadir is
  **NOT meaningful** for a TDE-backed restore.  PG's
  `backup_manifest` records SHA-256 over plaintext; the bytes
  on disk are ciphertext until the target's TDE engine reads
  them.  The restore step skips the verifybackup gate when
  `SourceTDE != nil` and logs an explicit "TDE: pg_verifybackup
  skipped, run inside TDE-capable sandbox" notice.

### `assert_restored_match` (test-kit)

The scenario runner's sandbox boots a vanilla `postgres:N-alpine`
image against the restored datadir.  For TDE-backed scenarios
the operator MUST swap the sandbox image to a TDE-capable build
(`cybertec/pgee:17`-style) with key access; otherwise the
sandbox PG aborts at startup.  The current testkit topology
doesn't ship a TDE-capable image — TDE scenarios are operator-
hosted L4/L5 today.

---

## What does NOT change under TDE

The big surprise on first read is how little of the codebase
needs to know.  These paths are all byte-opaque already and
require zero changes:

| Path | Why it works under TDE |
| --- | --- |
| `BASE_BACKUP` over `START_REPLICATION` | PGEE decrypts above the replication boundary; bytes-on-the-wire are plaintext.  Even if some engine kept ciphertext on the wire, our chunker would store the bytes and PG would decrypt them back at restore time. |
| `START_REPLICATION` (physical WAL) | Same — server-side decryption. |
| `START_REPLICATION ... LOGICAL` (CDC) | Logical decoding is a server-side operation that emits row-level changes; ciphertext stops at the buffer manager. |
| `IDENTIFY_SYSTEM` | A server function call returning plaintext text columns. |
| FastCDC chunker | Content-defined splits; doesn't assume page boundaries or header layouts. |
| Manifest signing / Merkle audit chain | Operates on our own bytes, not on PG's. |
| Repository-side envelope encryption (KEK + DEK) | Orthogonal: encrypts whatever bytes the chunker fed it.  TDE source + repo encryption is defence in depth, both layers active. |
| Incremental BASE_BACKUP (PG 17+) | PG's `summarize_wal` runs on plaintext WAL inside the server; the wire delivery is plaintext.  TDE doesn't interact with the incremental protocol. |

The only validation `pg_hardstorage` does that PG itself can't
do is on **content-defined chunk hashes** — those are over our
chunk bytes (which may be ciphertext from a TDE-encrypted source
or plaintext from a non-TDE one) and so they round-trip
identically regardless of source posture.

---

## When you don't have TDE

`tde.enabled: false` (the default) leaves every code path on its
historical strict-inspection path.  The deployment's manifests
will have no `source_tde` block; restore behaves as always.

A repo can hold a mix of TDE-source and non-TDE-source backups
side by side (e.g. one deployment per cluster, one cluster on
PGEE, one on vanilla PG).  The per-manifest `source_tde` stamp
is what restore consults; there's no repo-wide TDE flag.

---

## What if the operator forgets to set it?

The single observable failure mode is `wal push`:

- **Without** `--tde` on a TDE source: `wal push` reads the
  segment file's first 32 bytes (ciphertext), the
  `XLP_LONG_HEADER` check **may** pass by chance against random
  bytes (1-in-65536 probability per push), and `xlp_sysid`
  fields point at noise.  Subsequent restores cross-checking the
  segment manifest against the backup's `SystemIdentifier`
  refuse the segments with `repo.system_identifier_mismatch`.
  The signal is loud at restore time, the diagnostic is the
  bogus 8-byte field in the segment manifest.

- Manifest `SourceTDE` left nil despite TDE source: restore
  doesn't refuse a vanilla-PG target (because the manifest
  doesn't know).  A restore into vanilla PG fails at PG startup
  with control-file errors — diagnostic is loud but the
  remediation path is more painful than a config flag would
  have been.

Both failure modes are noisy, not silent.  Setting `tde.enabled:
true` once at deployment-add time avoids them both.

---

## Detection (manual)

`pg_hardstorage` does not currently auto-detect TDE.  An
auto-detect probe (`SELECT extname FROM pg_extension WHERE
extname IN ('pg_tde', 'cybertec_enterprise')`) is a candidate
for a follow-up; for now the operator declares it explicitly,
which matches how the rest of the deployment config works
(connection string, retention policy, classification tag are
all operator-declared).

To check on the source DB:

```sql
-- pg_tde:
SELECT extname FROM pg_extension WHERE extname = 'pg_tde';

-- CYBERTEC PGEE: encryption is built in; look at the GUC:
SHOW data_encryption;
-- If non-empty/non-'off', TDE is active.
```

Set `tde.enabled: true` accordingly.

---

## See also

- [Envelope encryption](envelope-encryption.md) — repo-side KEK
  + DEK layering, orthogonal to TDE.
- [Comparison vs pgBackRest / WAL-G / Barman](comparison-pgbackrest-walg-barman.md)
  — none of these handle TDE specially either; the contract
  documented here is unique to pg_hardstorage.

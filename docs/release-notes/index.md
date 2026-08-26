---
title: Release notes
description: Curated release-by-release summaries.
---

# Release notes

Curated, user-facing release notes — what's new, what
changed, what to know when upgrading.  The full granular
[`CHANGELOG`](../changelog.md) carries every commit-level
entry; release-notes pages distil the highlights for an
operator deciding whether to upgrade.

## Releases

- **[v1.3](v1.3.md)** — the unrun-tests release.  A scenario corpus
  where 162 of 174 files were wired into no target, and what running
  them found: an air-gap bundle feature that never worked for any
  compressed repository, and a recovery drill that could overwrite live
  tablespaces.  Adds `recovery drill --tablespace-mapping`, a
  per-deployment `drill:` block, and day/week units on the retention
  duration flags.
- **[v1.2](v1.2.md)** — the failover release.  A chaos gate that
  drives real Patroni faults (DCS outages, compound storms,
  retention janitors mid-storm) and then restores and boots the
  backups it took, plus the silent WAL-gap and storage-walk
  data-loss classes it surfaced and fixed.  Adds `restore
  --to-latest`, the `backup --tde` family with `source_tde`
  manifest stamping, an append-only repository mode, and
  `keyring install`; fixes the sidecar chart's keyring so it
  actually loads.  Backward compatible — no migration.
- **[v1.1](v1.1.md)** — cloud KMS becomes configurable in
  `pg_hardstorage.yaml` (`kms.providers[]` + per-deployment
  `kek_ref`), which is what finally lets the agent's
  scheduled and control-plane backups use a cloud KEK; the
  `scp://` backend goes from unusable to working; plus
  fleet-scale improvements (sharded audit chains, a
  deployment index, agent poll jitter, a job-concurrency cap)
  and correctness + WORM-compliance hardening.  Backward
  compatible — no migration.
- **[v1.0](v1.0.md)** — the first stable release.  Five
  Tier-1 KMS providers (AWS / GCP / Azure / Vault / HSM),
  six Tier-1 storage backends (fs / s3 / gcs / azblob /
  sftp / scp), Patroni-aware WAL streaming, LLM-assisted
  operations, two verifier sandboxes, full compliance
  surface.  24-month schema-compatibility commitment.

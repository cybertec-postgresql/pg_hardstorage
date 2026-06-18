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

- **[Unreleased](unreleased.md)** — changes on `main` since
  v1.0: fleet-scale improvements (sharded audit chains, a
  deployment index, agent poll jitter, a job-concurrency cap)
  plus correctness and WORM-compliance hardening.  Backward
  compatible — no migration.
- **[v1.0](v1.0.md)** — the first stable release.  Five
  Tier-1 KMS providers (AWS / GCP / Azure / Vault / HSM),
  five Tier-1 storage backends (fs / s3 / gcs / azblob /
  sftp), Patroni-aware WAL streaming, LLM-assisted
  operations, two verifier sandboxes, full compliance
  surface.  24-month schema-compatibility commitment.

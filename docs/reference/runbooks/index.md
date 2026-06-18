---
title: Runbooks
description: Disaster-recovery runbooks for named scenarios.
---

# Runbooks

The seven named disaster scenarios (R1-R7) ship with the
binary, surfaced through `pg_hardstorage doctor` and
`pg_hardstorage runbook generate`.  Each runbook is one
page, ends with a "did this work?" feedback link, and is
versioned alongside the binary.

| ID | Scenario | Page |
| --- | --- | --- |
| R1 | Repo region is gone | [R1](R1-repo-region-gone.md) |
| R2 | KMS key is destroyed | [R2](R2-kms-key-destroyed.md) |
| R3 | PG is gone, only backups remain | [R3](R3-cold-start-from-backups.md) |
| R4 | Repo corruption at rest detected by scrub | [R4](R4-repo-corruption-at-rest.md) |
| R5 | Half-applied PITR | [R5](R5-half-applied-pitr.md) |
| R6 | Slot dropped during failover, gap detected | [R6](R6-slot-dropped-gap.md) |
| R7 | Patroni split-brain | [R7](R7-patroni-split-brain.md) |

Plus the [control-plane setup](control-plane-setup.md)
deployment runbook for greenfield installs.

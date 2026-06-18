---
title: Add a deployment
description: Wire a new PostgreSQL deployment into pg_hardstorage.yaml
              with one CLI call.
tags:
  - deployment
  - configuration
---

# Add a deployment

> A *deployment* is the unit of "what we back up": one PG service.
> Adding it to the config takes one CLI call, validates the
> connection, and is ready to back up immediately.

## What you need

- A reachable PostgreSQL instance (primary or HA leader).
- A login role that can run `BASE_BACKUP` and create a physical
  replication slot — typically `REPLICATION` plus the `pg_read_all_data`
  default role.
- A repository URL that already exists. If not, create it with
  [`pg_hardstorage repo init`](../../reference/cli/pg_hardstorage_repo_init.md);
  see the per-backend recipes ([S3](repository-s3.md),
  [Azure Blob](repository-azblob.md), [GCS](repository-gcs.md),
  [SFTP](repository-sftp.md)).

## Steps

### 1. Add the deployment

```bash
# RUNNABLE
pg_hardstorage deployment add db1 \
    --connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo file:///srv/pg_hardstorage/repo
```

```console
deployment "db1" added
  pg_connection: postgres://pgbackup@db1.example.com/postgres
  repo:          file:///srv/pg_hardstorage/repo
  schedule:      backup=every 6h, rotate=daily_at 04:00
  probe:         ok (PG 17.2)
```

The CLI runs a connection probe by default; pass `--skip-probe`
when adding a deployment whose PG instance isn't online yet
(common in IaC bring-up flows).

### 2. (Optional) Tune the schedule

The two `--schedule-*` flags accept the same expressions as the
[schedule subcommand](../operating/schedule-backups.md):

```bash
pg_hardstorage deployment add db1 \
    --connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo s3://acme-pg-backups/?region=eu-central-1 \
    --schedule-backup 'daily_at 02:00' \
    --schedule-rotate 'daily_at 04:00'
```

Set `off` (or omit and call `pg_hardstorage schedule … off` later)
to leave a deployment unscheduled — useful for ad-hoc test
clusters.

### 3. Verify

```bash
pg_hardstorage deployment list
pg_hardstorage doctor db1
```

`doctor` runs the full preflight: paths, keystore, repo
writability, slot health. Each finding prints a `Suggested fix:`
line with the exact remediation command.

## What just happened

The CLI merged a new entry under `deployments.db1` in your active
config file (resolved via the
[XDG/FHS lookup chain](../../operations/operator-guide.md#11-configuration)).
The probe hit `pg_isready` and confirmed PG version, so a typo'd
host doesn't sit unnoticed in your config until backup time.

The resulting YAML looks like:

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: file:///srv/pg_hardstorage/repo
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
```

## Multi-tenant scoping

Pass `--tenant <id>` to scope the deployment for fleet-wide
operations. Tenants share the config file but every audit/event/
manifest carries the tenant tag, so cross-tenant operations
(retention, KMS, holds) are isolated by name.

```bash
pg_hardstorage deployment add db1 \
    --tenant acme-prod \
    --connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo s3://acme-pg-backups/
```

## Replacing an existing deployment

Re-running `deployment add` with the same name asks for
confirmation. Pass `--yes` for non-interactive replace (Ansible /
Terraform shape).

## Source PostgreSQL with TDE (CYBERTEC PGEE, pg_tde, EDB TDE)

If the source PostgreSQL has Transparent Data Encryption
enabled, edit the generated `pg_hardstorage.yaml` and add the
`tde:` block to the deployment:

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: s3://acme-pg-backups/
    tde:
      enabled: true
      engine: cybertec_enterprise      # free-form; informational
      key_ref: kms-secret://prod/pgee  # operator-supplied; opaque
```

This switches every code path that would otherwise parse PG byte
layout off the source filesystem (the `wal push` archive_command
shim, primarily) into "ciphertext, don't peek" mode.  Every
backup taken from a TDE-declared deployment carries a
`source_tde` block on its manifest so restore-time tooling can
refuse vanilla-PG targets and skip checksum gates that cannot be
meaningful against ciphertext.

The backup wire protocol itself does NOT change under TDE —
PGEE / pg_tde decrypt at the replication boundary so BASE_BACKUP
and START_REPLICATION deliver plaintext to pg_hardstorage.  See
[TDE awareness](../../explanation/tde-awareness.md) for the full
story (including the operator-facing failure modes if the flag
is forgotten).

## Troubleshooting

**`auth.pg_unreachable`** — the probe couldn't connect.
Common causes: firewall, `pg_hba.conf` missing the role,
`replication=database` not allowed. Run `pg_hardstorage deployment test db1`
after fixing.

**`conflict.deployment_exists`** — name already in use.
Re-run with `--yes` to replace, or pick a different name.

**`storage.unreachable`** — the repo URL doesn't resolve.
Confirm the URL parses and (for object stores) that the
credential chain is set in the agent's environment.

## Next steps

- [Set retention](../operating/set-retention.md)
- [Schedule backups](../operating/schedule-backups.md)
- [Take the first backup](../../tutorials/getting-started.md)
- [`deployment` CLI reference](../../reference/cli/pg_hardstorage_deployment.md)

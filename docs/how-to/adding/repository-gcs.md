---
title: Add a GCS repository
description: URL form, ADC authentication, storage class, and
              private-endpoint wiring for a GCS-backed
              pg_hardstorage repository.
tags:
  - repo
  - gcs
  - gcp
---

# Add a GCS repository

> The `gcs://` scheme stores chunks in a Google Cloud Storage
> bucket. Authentication uses Application Default Credentials
> (ADC), so GCE / GKE workloads pick up identity automatically.

## URL form

```text
gcs://<bucket>[/<prefix>][?storage_class=<class>&endpoint=<url>&credentials_file=<path>]
```

| Key | Meaning |
| --- | --- |
| `storage_class` | Default `StorageClass` for `Put` (`STANDARD`, `NEARLINE`, `COLDLINE`, `ARCHIVE`). |
| `endpoint` | Override the public Google host. Required under air-gap strict mode when using Private Google Access. |
| `credentials_file` | Path to a service-account JSON key file. Used in CI; production uses ADC instead. |

## What you need

- A GCS bucket in the target project.
- ADC reachable from the agent: GCE/GKE metadata service,
  Workload Identity (`gcloud iam workload-identity-pools …`),
  or `gcloud auth application-default login` in dev.
- Roles: `roles/storage.objectAdmin` on the bucket. For object
  retention (WORM) add `roles/storage.objectAdmin` is sufficient
  — the role already includes the retention permissions.

## Steps

### 1. Default ADC, public endpoint

```bash
# RUNNABLE
pg_hardstorage repo init 'gcs://acme-pg-backups'
```

```console
✓ Repository initialised
  URL:    gcs://acme-pg-backups
  ID:     54663a3581cb6b76c87e8c962daa186b
  Schema: pg_hardstorage.repo.v1
  Created: 2026-07-06T13:55:51Z
```

Workload Identity on GKE: bind the Kubernetes service account to
a Google service account with `roles/storage.objectAdmin`; the
ADC chain handles the rest.

### 2. Bucket with prefix and storage class

```bash
pg_hardstorage repo init \
    'gcs://acme-pg-backups/prod/cluster-a?storage_class=NEARLINE'
```

`NEARLINE` is the right default for backups: cheap-at-rest, fast
restore, with a 30-day minimum storage cost. `COLDLINE` (90-day
minimum) and `ARCHIVE` (365-day minimum) trade lower per-GB
cost for early-deletion fees.

### 3. CI / service-account JSON key

```bash
pg_hardstorage repo init \
    'gcs://acme-pg-backups?credentials_file=/etc/pg_hardstorage/gcp-key.json'
```

Mount the secret as a file (Kubernetes Secret with
`subPath`, GitHub Actions Secret written to disk, etc.) — the
plugin reads it via `gcsoption.WithCredentialsFile`.

### 4. Private Google Access / VPC

When the agent runs in a VPC with PGA enabled, the bucket
endpoint resolves to a private IP. Pin the endpoint when running
under air-gap strict mode:

```bash
PG_HARDSTORAGE_AIRGAPPED=1 pg_hardstorage repo init \
    'gcs://acme-pg-backups?endpoint=https://storage.googleapis.com'
```

The air-gap policy admits endpoints listed in
`airgap.allowlist` — match the host (`storage.googleapis.com`
or the VPC-resolved address) there.

### 5. Verify

```bash
pg_hardstorage repo check gcs://acme-pg-backups
```

## IAM bindings

Minimal GCS role:

```text
roles/storage.objectAdmin     # read + write + delete on objects
roles/storage.bucketViewer    # ListBuckets / GetBucket
```

For Object Retention / Bucket Lock (WORM), the same
`storage.objectAdmin` covers retention writes; bucket-level lock
must be applied with `gcloud storage buckets update --lock-retention-policy`
out-of-band.

## Conditional writes

GCS supports `If: Conditions{DoesNotExist: true}` natively, so
the StoragePlugin's `IfNotExists` semantic resolves atomically at
the GCS server. Concurrent `repo init` calls race-safe out of
the box.

## Troubleshooting

**`storage.unreachable`** at init — ADC didn't resolve.
`gcloud auth application-default print-access-token` from the
agent host. On GKE, confirm the Workload Identity binding:

```bash
gcloud iam service-accounts get-iam-policy \
    pg-hardstorage@acme.iam.gserviceaccount.com
```

**`storageClass` rejected** — the bucket's location class may
not allow the requested object class (regional vs multi-regional).
Re-create the bucket with a compatible location class.

**Air-gap refusal of the public endpoint** — set
`endpoint=` to a private resolution and add it to
`airgap.allowlist`.

## Next steps

- [Add a deployment](deployment.md) wired to this repo
- [Add GCP KMS as the KMS provider](kms-gcp.md)
- [Pin residency](../operating/data-residency.md)
- [`repo init` CLI reference](../../reference/cli/pg_hardstorage_repo_init.md)

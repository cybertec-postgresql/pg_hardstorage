---
title: Set data-residency policy
description: Pin a deployment's repository to one or more allowed
              regions; refuse mismatches at backup time.
tags:
  - residency
  - compliance
  - data-protection
---

# Set data-residency policy

> Residency policy constrains a deployment's repository to a
> set of allowed regions. The plugin's `Region()` is compared
> against the deployment's allowlist; mismatches surface in
> `doctor` and (v1.0+) at the residency gate before
> `pg_backup_start`.

## Match rules

Region match is case-insensitive and hyphen-aware prefix:

| Policy | Matches |
| --- | --- |
| `["eu"]` | `eu-west-1`, `eu-central-1`, `eu-north-1`, … |
| `["eu-west-1"]` | `eu-west-1` only |
| `["eu", "us"]` | EU **or** US regions |
| `[]` | No constraint (default) |

The `fs` storage plugin reports an empty region (`"region
unknown"`) and **fails any non-empty residency check** —
local-disk repos can't enforce residency, and silently
treating that as a pass would defeat the purpose.

## What you need

- A deployment with a configured object-store repo (S3, GCS,
  Azure Blob, …). The storage plugin must implement
  `RegionAware`; the [S3](../adding/repository-s3.md), [GCS](../adding/repository-gcs.md),
  and [Azure Blob](../adding/repository-azblob.md) plugins all do.

## Steps

### 1. Pin to a region

```bash
pg_hardstorage residency set db1 eu-west-1
```

```console
✓ db1: residency = [eu-west-1] (was [])
```

### 2. Pin to a continent

```bash
pg_hardstorage residency set db1 eu
```

`eu` matches every region whose code starts with `eu-`. Useful
for GDPR-style "stay in the EU but I don't care which AZ"
posture.

### 3. Pin to multiple regions

```bash
pg_hardstorage residency set db1 eu-central-1 eu-west-1
```

Multiple positional regions become a list — match is
"any of these."

### 4. List all deployments' policies

```bash
pg_hardstorage residency list
```

```console
3 deployment(s)
  DEPLOYMENT  RESIDENCY
  db1         eu-west-1
  db2         eu
  db3         —
```

### 5. Verify the configured repo's region matches

```bash
pg_hardstorage residency check db1
```

```console
residency check — db1
  Repo:      s3://acme-pg-backups/?region=eu-west-1
  Region:    eu-west-1
  Allowed:   eu-west-1
  ✓ region "eu-west-1" matches policy entry "eu-west-1" exactly
```

A mismatch returns exit code 9 and a `verify.residency_violation`
error:

```console
ERROR verify.residency_violation: residency check: region "us-east-1" does not match any allowed entry [eu-west-1]
  hint: either update the deployment's repo to a region that matches the policy, or relax the policy with `pg_hardstorage residency set` / `clear`.
    run: pg_hardstorage residency list
```

`doctor` runs the same check and surfaces the result among its
findings.

### 6. Clear a policy

```bash
pg_hardstorage residency clear db1
```

The deployment's `residency:` is dropped from the YAML.

## Configuration shape

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: s3://acme-pg-backups/?region=eu-west-1
    residency:
      - eu-west-1
```

The list goes under the deployment, not at the top level —
each deployment has its own residency.

## Today's enforcement surface

- `pg_hardstorage residency check <deployment>` — explicit gate.
- `pg_hardstorage doctor` — surfaces mismatches as findings.
- v1.0+: a residency gate in the runner refuses
  `pg_backup_start` if the resolved repo region doesn't match.

In v0.1, the policy is **advisory at backup time**. Wire
`residency check` into your IaC pipeline (Terraform external
data source, GitHub Actions step, Argo workflow gate) to
enforce it pre-deployment.

## Combining with KMS residency

Residency on the *repo* doesn't constrain the *KEK*. For full
data-domiciling, also pin the KEK provider to the same region:

- AWS KMS — CMK is regional; the KEKRef encodes the ARN
  (which encodes the region).
- GCP KMS — CryptoKey location encoded in the KEKRef.
- Azure Key Vault — vault location encoded in the KEKRef.

See [Add AWS KMS](../adding/kms-aws.md), [GCP KMS](../adding/kms-gcp.md),
[Azure Key Vault](../adding/kms-azure.md).

## Troubleshooting

**`verify.residency_violation`** (repo region unknown) — the
deployment's repo is `file://…`, which doesn't report a region.
Move to an object store, or clear the residency policy.

**`verify.residency_violation`** (region mismatch) — the repo's
region doesn't match the allowlist. Either move the repo or relax
the policy.

**`region` empty on S3-compatible endpoints** — MinIO, R2,
Wasabi often don't report a meaningful region. Pin
`?region=...` in the URL to the canonical name your residency
policy expects.

## Next steps

- [Add a deployment](../adding/deployment.md) with a region-pinned repo
- [Set retention](set-retention.md)
- [`residency` CLI reference](../../reference/cli/pg_hardstorage_residency.md)
- [Runbook R1: repo region gone](../../reference/runbooks/R1-repo-region-gone.md)

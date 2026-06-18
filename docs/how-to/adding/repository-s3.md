---
title: Add an S3 repository
description: URL form, IAM, region, and S3-compatible endpoint
              wiring for an S3-backed pg_hardstorage repository.
tags:
  - repo
  - s3
  - aws
---

# Add an S3 repository

> The `s3://` scheme covers AWS S3, MinIO, Cloudflare R2, and
> Backblaze B2 — anything that speaks the S3 API. Initialisation
> is one CLI call; auth uses the AWS SDK's default credential
> chain.

## URL form

```text
s3://<bucket>/<optional-prefix>?region=<r>&endpoint=<url>&path_style=true&storage_class=<class>
```

Query parameters:

| Key | Meaning |
| --- | --- |
| `region` | AWS region. Required for AWS S3 if not in env / profile. |
| `endpoint` | S3-compatible endpoint (MinIO, R2, Wasabi). Omit for AWS. |
| `path_style` | `true` forces path-style addressing. Required for MinIO and other endpoints whose bucket names aren't DNS-safe. Implicitly `true` whenever `endpoint` is set. |
| `storage_class` | Default `StorageClass` for `Put` (`STANDARD`, `STANDARD_IA`, `GLACIER_IR`, etc.). |

## What you need

- A bucket. The CLI does **not** create the bucket — that's
  Terraform / IaC territory.
- AWS credentials reachable through the SDK's default chain:
  env vars (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`),
  IRSA, IMDSv2, AWS SSO, or `~/.aws/credentials`.
- `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, `s3:ListBucket`
  on the bucket. Add `s3:PutObjectRetention` /
  `s3:PutObjectLegalHold` if you also want WORM (Object Lock).

## Steps

### 1. AWS S3

```bash
# RUNNABLE
pg_hardstorage repo init 's3://acme-pg-backups/?region=eu-central-1'
```

```console
repo: s3://acme-pg-backups/?region=eu-central-1
mode: ok    region: eu-central-1
```

### 2. AWS S3 with a prefix

A prefix lets one bucket host multiple repos (per-tenant /
per-cluster):

```bash
pg_hardstorage repo init \
    's3://acme-pg-backups/prod/cluster-a?region=eu-central-1'
```

### 3. AWS S3 with WORM (Object Lock)

```bash
pg_hardstorage repo init 's3://acme-pg-backups/?region=eu-central-1' \
    --worm-mode compliance \
    --worm-retention 7y
```

`compliance` is regulatory-grade: even root credentials cannot
delete an object before the retention deadline. Pick `governance`
when an IAM `BypassGovernance` principal is acceptable. WORM is
set at init time only — it cannot be flipped on later.

### 4. MinIO / Cloudflare R2 / Wasabi

```bash
pg_hardstorage repo init \
    's3://repo/?endpoint=https://minio.acme.example.com&path_style=true'
```

```bash
pg_hardstorage repo init \
    's3://acme-pg-backups/?endpoint=https://<accountid>.r2.cloudflarestorage.com'
```

For R2 set `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` to the
R2 access keypair. Region is ignored on R2; the SDK still wants a
non-empty value, which the plugin defaults to `us-east-1` when
unset.

### 5. Verify writability

```bash
# RUNNABLE
pg_hardstorage repo check s3://acme-pg-backups/
```

Roundtrips a manifest signature, sample-checks a chunk reference,
and reports the total live byte count. Mismatches return exit 9.

## IAM policy (minimum)

Replace `<bucket>` with the bucket name (and `<prefix>` if the URL
includes one):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "RepoBucket",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:GetObjectTagging",
        "s3:PutObjectTagging"
      ],
      "Resource": "arn:aws:s3:::<bucket>/*"
    },
    {
      "Sid": "RepoBucketList",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::<bucket>"
    }
  ]
}
```

Add `s3:PutObjectRetention` and `s3:PutObjectLegalHold` if you
use WORM; add `s3:BypassGovernanceRetention` only on the
break-glass principal.

## Troubleshooting

**`storage.region_mismatch`** — the bucket lives in a different
region than declared. Fix the `?region=` parameter or the bucket.

**`storage.unreachable`** at init — credential chain returns
nothing. `aws sts get-caller-identity` from the same shell;
on EKS, confirm the IRSA service-account binding.

**`storage.access_denied`** — IAM is short. Compare against the
policy above; check bucket policy and SCPs (Service Control
Policies on the org account often block delete).

**Path-style errors against MinIO** — set `path_style=true`.
The plugin already implies it whenever `endpoint=` is set, but a
typoed bucket name still fails fast.

## Next steps

- [Add a deployment](deployment.md) wired to this repo
- [Set residency](../operating/data-residency.md) so the
  deployment refuses non-EU repos
- [Set retention](../operating/set-retention.md)
- [`repo init` CLI reference](../../reference/cli/pg_hardstorage_repo_init.md)
- [Repository runbook R1: repo region gone](../../reference/runbooks/R1-repo-region-gone.md)

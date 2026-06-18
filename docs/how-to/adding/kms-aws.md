---
title: Add AWS KMS
description: KEKRef format, IAM policy, and FIPS posture for the
              AWS KMS provider.
tags:
  - kms
  - aws
  - encryption
---

# Add AWS KMS

> AWS KMS is the strongest production-grade KEK posture
> pg_hardstorage offers without bringing PKCS#11 / on-prem HSM
> into the binary. The CMK's bytes never leave AWS KMS — we only
> ever see `Encrypt` / `Decrypt` ciphertext blobs.

## KEKRef format

```text
aws-kms://<key-id-or-arn>
```

All three AWS-accepted forms work; the host part is handed
verbatim to the SDK's `KeyId` parameter:

| Form | Example |
| --- | --- |
| ARN | `aws-kms://arn:aws:kms:us-east-1:123456789012:key/abcd1234-...` |
| Alias | `aws-kms://alias/pg-hardstorage-prod` |
| Key ID | `aws-kms://12345678-1234-1234-1234-123456789012` |

## What you need

- A KMS Customer Master Key (CMK) of type `SYMMETRIC_DEFAULT`
  (the AES-256-GCM-friendly default).
- An identity (IAM role / IRSA / EC2 instance profile) with
  `kms:Encrypt`, `kms:Decrypt`, and `kms:DescribeKey` on that
  key. Add `kms:ScheduleKeyDeletion` if you intend to issue
  `pg_hardstorage kms shred`.
- A region (CMKs are regional). The same region you keep the
  repo in if residency policy applies.

## Steps

### 1. Configure the provider in `pg_hardstorage.yaml`

```yaml
kms:
  providers:
    - kek_ref: aws-kms://alias/pg-hardstorage-prod
      config:
        region: us-east-1
        pending_window_days: 30   # 7..30; window between shred and destruction
        use_fips_endpoint: true   # routes to kms-fips.<region>.amazonaws.com
```

`pending_window_days` is AWS's "cool off" — you can cancel a
scheduled deletion within that window. 30 (the AWS maximum) is
the default and the value to pick unless legal mandates a tighter
posture.

### 2. Reference the KEK from the deployment

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: s3://acme-pg-backups/?region=us-east-1
    kek_ref: aws-kms://alias/pg-hardstorage-prod
```

The `kek_ref` is recorded in every backup manifest under
`encryption.kek_ref`; on `restore` and `verify` the same provider
unwraps the DEK transparently.

### 3. Take the first encrypted backup

```bash
pg_hardstorage backup db1 --encrypt
```

`--encrypt` makes encryption mandatory: the command errors out
if the KEK is unreachable, so a misset IAM role can't silently
produce plaintext backups.

### 4. Verify

```bash
pg_hardstorage kms verify --repo s3://acme-pg-backups/
```

Walks every committed manifest and confirms the wrapped DEK
unwraps under the resolved KEK. Mismatches return exit 9.

## Minimum IAM policy

Replace `<account>` and `<key-uuid>` with the actual values:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PgHardstorageDEKWrap",
      "Effect": "Allow",
      "Action": [
        "kms:Encrypt",
        "kms:Decrypt",
        "kms:DescribeKey"
      ],
      "Resource": "arn:aws:kms:us-east-1:<account>:key/<key-uuid>"
    }
  ]
}
```

For `kms shred`:

```json
{
  "Sid": "PgHardstorageShred",
  "Effect": "Allow",
  "Action": ["kms:ScheduleKeyDeletion", "kms:CancelKeyDeletion"],
  "Resource": "arn:aws:kms:us-east-1:<account>:key/<key-uuid>"
}
```

## FIPS posture

AWS KMS is FIPS 140-2 Level 3 in `us-gov-{west,east}-1`,
`us-east-1`, and `us-west-2`. Set `use_fips_endpoint: true` to
route requests to `kms-fips.<region>.amazonaws.com`. The
provider's `FIPSMode()` then reports `true`, which downstream
compliance reports propagate as evidence.

## Crypto-shred

```bash
aws kms schedule-key-deletion --key-id <key-id> --pending-window-in-days 7

pg_hardstorage audit append kms.shred --repo s3://acme-backups/ \
    --reason "GDPR Art 17 #4421; scheduled deletion of AWS KMS key <key-id>"
```

`kms shred` destroys the *local* keyring KEK; an AWS-KMS-wrapped
backup is crypto-shredded in AWS. `ScheduleKeyDeletion` runs with a
`pending_window_days`. After
the window elapses, AWS destroys the key material; every backup
whose wrapped DEK was wrapped under this CMK becomes
permanently unrecoverable — the GDPR Art. 17 contract. The
audit chain records the schedule + deletion-date as the
compliance artifact. See the [crypto-shred how-to](../operating/crypto-shred.md).

## Air-gap interaction

AWS KMS only resolves over the public internet, **or** via a VPC
endpoint with a private IP. Operators running under
`PG_HARDSTORAGE_AIRGAPPED=1` configure a VPC interface endpoint
(`com.amazonaws.<region>.kms`) and add its DNS name to
`airgap.allowlist`.

## Troubleshooting

**`AccessDeniedException`** — the IAM action list is short.
Compare to the policy above; check the key's *key policy* (a
distinct construct that supplements IAM and can independently
deny operations).

**`InvalidArnException`** — the alias prefix is missing
(`alias/` is part of the alias name) or the ARN has the wrong
account.

**`KMSInvalidStateException` on `Decrypt`** — the key is in
`PendingDeletion`. Either cancel the scheduled deletion or
write off the affected backups; this is the GDPR-shred contract
operating exactly as designed.

## Next steps

- [Rotate the KEK](../operating/rotate-kek.md)
- [Crypto-shred](../operating/crypto-shred.md)
- [`kms` CLI reference](../../reference/cli/pg_hardstorage_kms.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)

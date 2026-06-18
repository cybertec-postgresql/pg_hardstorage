---
title: Add GCP KMS
description: KEKRef format, IAM bindings, and version-pinning
              rules for the GCP KMS provider.
tags:
  - kms
  - gcp
  - encryption
---

# Add GCP KMS

> GCP KMS keeps the cloud-side KEK inside Google's HSMs (Software
> / HSM / External / Cloud-HSM protection levels). pg_hardstorage
> only sees `Encrypt` / `Decrypt` ciphertext blobs.

## KEKRef format

```text
gcp-kms://projects/<proj>/locations/<loc>/keyRings/<ring>/cryptoKeys/<key>
gcp-kms://projects/<proj>/locations/<loc>/keyRings/<ring>/cryptoKeys/<key>/cryptoKeyVersions/<v>
```

| Form | Used for |
| --- | --- |
| Without `/cryptoKeyVersions/<v>` | `Wrap`/`Unwrap` — Encrypt routes to the primary version; Decrypt picks the version from ciphertext metadata. |
| With version | **Required for `Shred`.** GCP destroys versions, not keys; without an explicit version a Shred would have to enumerate which is racy. |

## What you need

- A KMS keyring in the project + location of choice. Pick a
  location that matches the repo's region for residency.
- A CryptoKey with purpose `ENCRYPT_DECRYPT`. Pick protection
  level `HSM` for FIPS 140-2 Level 3, `SOFTWARE` for FIPS 140-2
  Level 1.
- IAM:
  - `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key for
    backup/restore.
  - `roles/cloudkms.admin` (or a custom role with
    `cloudkms.cryptoKeyVersions.destroy`) on the key for
    `kms shred`.

## Steps

### 1. Configure the provider in `pg_hardstorage.yaml`

```yaml
kms:
  providers:
    - kek_ref: gcp-kms://projects/acme-prod/locations/europe-west1/keyRings/pg-hardstorage/cryptoKeys/db1
      config:
        use_fips_mode: true   # operator declaration; matches HSM-protected keys
```

`use_fips_mode` is operator-declared because the SDK can't
infer protection level from the wire — Software-protected and
HSM-protected keys look identical at the API surface.

### 2. Reference the KEK from the deployment

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: gcs://acme-pg-backups
    kek_ref: gcp-kms://projects/acme-prod/locations/europe-west1/keyRings/pg-hardstorage/cryptoKeys/db1
```

### 3. Take the first encrypted backup

```bash
pg_hardstorage backup db1 --encrypt
```

### 4. Verify

```bash
pg_hardstorage kms verify --repo gcs://acme-pg-backups
```

## IAM bindings

```bash
# For backup / restore
gcloud kms keys add-iam-policy-binding db1 \
    --keyring pg-hardstorage --location europe-west1 \
    --member 'serviceAccount:pg-hardstorage@acme-prod.iam.gserviceaccount.com' \
    --role roles/cloudkms.cryptoKeyEncrypterDecrypter

# For kms shred — separate, more privileged binding
gcloud kms keys add-iam-policy-binding db1 \
    --keyring pg-hardstorage --location europe-west1 \
    --member 'serviceAccount:pg-hardstorage-shredder@acme-prod.iam.gserviceaccount.com' \
    --role roles/cloudkms.admin
```

Use a separate identity for shredding — pairs nicely with the
[n-of-m approval flow](../operating/n-of-m-approvals.md).

## Authentication

Google's [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials)
chain handles auth: GCE/GKE metadata service, Workload Identity,
or `gcloud auth application-default login` in dev. The same
chain that the [GCS storage plugin](repository-gcs.md) uses.

For CI without Workload Identity, point at a service-account
JSON key:

```yaml
kms:
  providers:
    - kek_ref: gcp-kms://projects/acme-prod/locations/europe-west1/keyRings/pg-hardstorage/cryptoKeys/db1
      config:
        credentials_file: /etc/pg_hardstorage/gcp-kms-key.json
```

## Crypto-shred

`pg_hardstorage kms shred` destroys the *local* keyring KEK; a
GCP-KMS-wrapped backup is crypto-shredded by destroying the KEK
**version** in Cloud KMS, which makes every DEK wrapped with it
unrecoverable. Record the act in the audit chain for the
compliance trail:

```bash
gcloud kms keys versions destroy 3 \
    --key db1 --keyring pg-hardstorage --location europe-west1

pg_hardstorage audit append kms.shred --repo s3://acme-backups/ \
    --reason "GDPR Art 17 #4421; destroyed gcp-kms .../cryptoKeys/db1 version 3"
```

Destroying the specific key **version** keeps the shred
surgical. `gcloud` calls `DestroyCryptoKeyVersion`; GCP marks the version
`DESTROY_SCHEDULED` immediately and destroys the key material
after the configured cooldown (24h by default; controlled by the
`destroy_scheduled_duration` field on the parent key — set at
key-creation time, not by pg_hardstorage).

See [Crypto-shred](../operating/crypto-shred.md) for the
end-to-end flow.

## Air-gap interaction

GCP KMS is reachable over private VPC endpoints (Private Google
Access). Set
`endpoint: 'https://cloudkms.googleapis.com'` (or the private-IP
resolution) in the provider config and add the host to
`airgap.allowlist`.

## Troubleshooting

**`PERMISSION_DENIED`** — the identity is missing
`cryptoKeyEncrypterDecrypter`. The role lives at the key scope,
not the project scope; binding it at project scope works but is
overprivileged.

**`FAILED_PRECONDITION` on shred** — the KEKRef is missing
`/cryptoKeyVersions/<v>`. GCP doesn't destroy whole keys, only
versions.

**`UNAVAILABLE` retried by SDK** — transient; ride out the SDK's
exponential backoff. Common during regional control-plane
deploys.

## Next steps

- [Rotate the KEK](../operating/rotate-kek.md)
- [Crypto-shred](../operating/crypto-shred.md)
- [Add a GCS repository](repository-gcs.md)
- [`kms` CLI reference](../../reference/cli/pg_hardstorage_kms.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)

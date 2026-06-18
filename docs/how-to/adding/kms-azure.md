---
title: Add Azure Key Vault
description: KEKRef format, the Azure default credential chain,
              and soft-delete semantics for the Azure Key Vault
              KMS provider.
tags:
  - kms
  - azure
  - encryption
---

# Add Azure Key Vault

> Azure Key Vault keeps the cloud-side KEK inside the vault's
> HSM. Premium tier and Managed HSM are FIPS 140-2 Level 3
> validated; Standard tier is Level 2. pg_hardstorage uses
> the dedicated `WrapKey` / `UnwrapKey` operations.

## KEKRef format

```text
azure-kv://<vault-name>/<key-name>
azure-kv://<vault-name>/<key-name>/<version>
```

The version-pinned form is **required for `Shred`** — Azure
soft-deletes whole keys, but pg_hardstorage records the version
that was active for each backup so you can attribute precisely
which versions are at risk.

Bare vault names resolve against `*.vault.azure.net`; for
sovereign clouds the vault host is dotted (and accepted
verbatim) — same convention as the
[azblob storage plugin](repository-azblob.md).

## What you need

- An Azure Key Vault (Standard, Premium, or Managed HSM).
- A key in that vault, type RSA-HSM (recommended) or RSA — the
  provider defaults to RSA-OAEP-256 for wrap/unwrap.
- An identity with **Key Vault Crypto User** role (or
  `wrapKey` + `unwrapKey` in an access policy) on the key.
- For `kms shred`, the identity also needs **Key Vault Crypto
  Officer** (or `delete` permission).

## Steps

### 1. Configure the provider in `pg_hardstorage.yaml`

```yaml
kms:
  providers:
    - kek_ref: azure-kv://acme-pg-vault/db1-kek
      config:
        use_fips_mode: true   # operator declaration; matches Premium / Managed HSM
```

### 2. Reference the KEK from the deployment

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: azblob://acmebackups/prod
    kek_ref: azure-kv://acme-pg-vault/db1-kek
```

### 3. Take the first encrypted backup

```bash
pg_hardstorage backup db1 --encrypt
```

### 4. Verify

```bash
pg_hardstorage kms verify --repo azblob://acmebackups/prod
```

## Authentication

`DefaultAzureCredential` chains through env vars → Workload
Identity → managed identity → Azure CLI. Operators on AKS / VM
get managed-identity for free; CI uses service-principal env
vars; developers use `az login`. Same chain as
[the azblob storage plugin](repository-azblob.md#auth-chain-order).

## Role assignment

```bash
# Crypto User: wrap, unwrap, encrypt, decrypt — sufficient for backup / restore
az role assignment create \
    --assignee-object-id <agent-managed-identity-object-id> \
    --assignee-principal-type ServicePrincipal \
    --role "Key Vault Crypto User" \
    --scope $(az keyvault key show --vault-name acme-pg-vault --name db1-kek --query id -o tsv)

# Crypto Officer: adds delete — required only for kms shred
az role assignment create \
    --assignee-object-id <shred-identity-object-id> \
    --assignee-principal-type ServicePrincipal \
    --role "Key Vault Crypto Officer" \
    --scope $(az keyvault key show --vault-name acme-pg-vault --name db1-kek --query id -o tsv)
```

Use a separate identity for the shred role and pair with the
[n-of-m approval flow](../operating/n-of-m-approvals.md).

## Sovereign clouds

```yaml
kms:
  providers:
    - kek_ref: azure-kv://acme-pg-vault.vault.usgovcloudapi.net/db1-kek
```

The dotted host is taken literally; the SDK's auth endpoint
follows when `AZURE_AUTHORITY_HOST` is set (or implied by the
sovereign-cloud VM the agent runs on).

## Crypto-shred

```bash
az keyvault key delete --vault-name acme-pg-vault --name db1-kek

pg_hardstorage audit append kms.shred --repo s3://acme-backups/ \
    --reason "GDPR Art 17 #4421; deleted azure-kv acme-pg-vault/db1-kek"
```

`kms shred` destroys the *local* keyring KEK; an Azure-KV-wrapped
backup is crypto-shredded in Azure. `az keyvault key delete` calls
`DeleteKey`. Azure moves the key into
soft-delete state; full destruction occurs after the vault's
recovery window (default 90 days; configurable 7-90 at vault
creation time).

For immediate destruction, follow up out-of-band with
`az keyvault key purge` from a privileged operator. We don't
issue purges directly because they're irrecoverable and the
soft-delete safety net catches operator errors.

See [Crypto-shred](../operating/crypto-shred.md) for the full
flow.

## Troubleshooting

**`Forbidden`** — RBAC vs. access-policy authorisation model
mismatch. Modern vaults use Azure RBAC; legacy vaults use
in-vault access policies. Check the vault's
"Permission model" in the portal.

**`KeyDisabled`** — the key was disabled (often by Defender for
Cloud auto-remediation). Re-enable in the portal or via
`az keyvault key set-attributes --enabled true`.

**`KeyNotFound`** after a soft-delete** — the key is in the
soft-delete state. Either restore it
(`az keyvault key recover`) or accept the destruction.

## Next steps

- [Rotate the KEK](../operating/rotate-kek.md)
- [Crypto-shred](../operating/crypto-shred.md)
- [Add an Azure Blob repository](repository-azblob.md)
- [`kms` CLI reference](../../reference/cli/pg_hardstorage_kms.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)

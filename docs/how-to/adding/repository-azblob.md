---
title: Add an Azure Blob repository
description: URL form, the Azure default credential chain, and
              sovereign-cloud handling for an Azure Blob-backed
              pg_hardstorage repository.
tags:
  - repo
  - azblob
  - azure
---

# Add an Azure Blob repository

> The `azblob://` scheme stores chunks in an Azure Storage account
> container. Auth flows through `DefaultAzureCredential`, so
> managed-identity, env-var, and `az login` all work without
> per-environment plumbing.

## URL form

```text
azblob://<account>/<container>[/<prefix>][?option=value&...]
```

| Form | Resolves to |
| --- | --- |
| `azblob://acmebackups/prod` | `https://acmebackups.blob.core.windows.net`, container `prod` |
| `azblob://acmebackups/prod/db1` | …, container `prod`, prefix `db1` |
| `azblob://acmebackups.blob.core.usgovcloudapi.net/prod` | sovereign cloud — host is taken literally |

Bare account names get the public-cloud `.blob.core.windows.net`
suffix. A dotted host is used verbatim, which is how the same
URL form covers Azure US Government, Azure China
(`*.blob.core.chinacloudapi.cn`), Azure Germany, and the
test endpoint Azurite.

Query parameters:

| Key | Meaning |
| --- | --- |
| `access_tier` | `hot`, `cool`, or `archive` for `Put` defaults. |
| `endpoint` | Override the resolved service URL (Azurite, private endpoint). |
| `account_key` | Base64-encoded shared key. Builds a `SharedKeyCredential` instead of going through the default chain. **Avoid** in production — prefer managed identity. |

## What you need

- A storage account in the target subscription.
- A container created on that account (the CLI does **not**
  create one).
- An identity with `Storage Blob Data Contributor` (read +
  write + delete) on the container — managed identity for the
  agent host, a service principal for CI, or `az login` in dev.

## Steps

### 1. Public cloud, managed identity

```bash
# RUNNABLE
pg_hardstorage repo init 'azblob://acmebackups/prod'
```

```console
repo: azblob://acmebackups/prod
mode: ok    region: <azure-region-from-account>
```

The agent's pod identity / VM-managed identity is read by
`DefaultAzureCredential`; nothing else to wire.

### 2. Public cloud with a tier

```bash
pg_hardstorage repo init 'azblob://acmebackups/prod?access_tier=cool'
```

`cool` makes new chunks `Cool`-tier by default; reads stay
direct. Use `archive` only for backup-of-last-resort posture —
restores then need a rehydrate step measured in hours.

### 3. Sovereign cloud (US Government, China, Germany)

```bash
pg_hardstorage repo init 'azblob://acmebackups.blob.core.usgovcloudapi.net/prod'
```

The dotted host is passed through; the SDK's authentication
endpoint follows the cloud automatically when
`AZURE_AUTHORITY_HOST` is set (or when running on a sovereign-
cloud VM that advertises it).

### 4. CI / service principal

```bash
export AZURE_TENANT_ID=...
export AZURE_CLIENT_ID=...
export AZURE_CLIENT_SECRET=...
pg_hardstorage repo init 'azblob://acmebackups/ci-test'
```

The default credential chain picks up the env triplet and
authenticates without a config file.

### 5. Local development against Azurite

```bash
pg_hardstorage repo init \
    'azblob://devstoreaccount1/test?endpoint=http://127.0.0.1:10000/devstoreaccount1&account_key=Eby8...Mg=='
```

This is the canonical Azurite shape — an `account_key` overrides
the default chain so the dev container needs no Azure identity.

## Auth chain order

`DefaultAzureCredential` tries, in order:

1. Environment variables (`AZURE_*`)
2. Workload Identity (federated tokens, common on AKS + GitHub
   Actions OIDC)
3. Managed Identity (system- or user-assigned)
4. Azure CLI (`az login`)
5. IDE / `Azure Developer CLI`

Earlier providers win; supply credentials at the matching layer
of your environment.

## Immutable storage / WORM

```bash
pg_hardstorage repo init 'azblob://acmebackups/prod' \
    --worm-mode compliance \
    --worm-retention 7y
```

WORM relies on Azure's per-blob immutability policies. The
container must have **version-level immutability enabled at
container creation time**; without it the service rejects the
retention metadata with a clear error.

## Troubleshooting

**`AuthorizationPermissionMismatch`** — the identity has
`Reader` rather than `Blob Data Contributor`. Add the role at
the container scope (preferred) or storage-account scope.

**`ContainerNotFound`** — the path component you passed isn't
a real container. Run `az storage container list --account-name
acmebackups` to confirm.

**`endpoint` is not allowed** in air-gap strict mode — the
endpoint must resolve to an RFC1918 / loopback address or be
listed in `airgap.allowlist`. Same constraint applies to
private-endpoint URLs.

## Next steps

- [Add a deployment](deployment.md) wired to this repo
- [Pin residency](../operating/data-residency.md) — Azure
  reports the account region via the SDK
- [Add Azure Key Vault as the KMS provider](kms-azure.md)
- [`repo init` CLI reference](../../reference/cli/pg_hardstorage_repo_init.md)

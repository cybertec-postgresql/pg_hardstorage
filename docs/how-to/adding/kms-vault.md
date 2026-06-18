---
title: Add HashiCorp Vault Transit
description: KEKRef format, AppRole authentication, and the
              "deletion_allowed" requirement for the Vault
              Transit KMS provider.
tags:
  - kms
  - vault
  - encryption
---

# Add HashiCorp Vault Transit

> Vault Transit is the most common self-hosted KMS in production,
> especially in cloud-agnostic and on-prem deployments. The
> Transit key lives inside Vault's storage backend and never
> leaves; pg_hardstorage only ever sees ciphertext blobs that
> look like `vault:v1:<base64>`.

## KEKRef format

```text
vault-transit://<host>[:port]/<mount>/<key-name>
```

| Component | Meaning |
| --- | --- |
| `<host>` | Vault address (with port if non-default). |
| `<mount>` | Transit engine mount path. Vault permits multiple Transit instances on different mounts (`transit/`, `secrets-eu/transit/`, …). |
| `<key-name>` | The Transit key itself. |

Examples:

```text
vault-transit://vault.acme.example.com:8200/transit/db-kek
vault-transit://10.0.0.5:8200/secrets-eu/db-prod-kek
```

## What you need

- A reachable Vault server with the Transit engine mounted.
- A Transit key created with
  `vault write -f transit/keys/db-kek`. Use
  `type=aes256-gcm96` (the default) unless you have a strong
  reason for another type.
- A Vault auth method that resolves to a token with
  `transit/encrypt/<key>` and `transit/decrypt/<key>`
  capabilities.
- For `kms shred`, the key must also have
  `deletion_allowed=true`:

  ```bash
  vault write transit/keys/db-kek/config deletion_allowed=true
  ```

  This is a deliberate safety belt — operators have to
  explicitly opt the key into deletability.

## Steps

### 1. Create a Vault policy

```hcl
# pg_hardstorage policy
path "transit/encrypt/db-kek" { capabilities = ["update"] }
path "transit/decrypt/db-kek" { capabilities = ["update"] }
path "transit/keys/db-kek"     { capabilities = ["read"] }
```

```bash
vault policy write pg-hardstorage pg-hardstorage.hcl
```

For `kms shred`, an additional, more privileged policy:

```hcl
path "transit/keys/db-kek" {
  capabilities = ["read", "update", "delete"]
}
```

### 2. Bind an auth method

For AppRole (most common pattern):

```bash
vault write auth/approle/role/pg-hardstorage \
    token_policies=pg-hardstorage \
    token_ttl=1h token_max_ttl=24h

ROLE_ID=$(vault read -field=role_id auth/approle/role/pg-hardstorage/role-id)
SECRET_ID=$(vault write -force -field=secret_id auth/approle/role/pg-hardstorage/secret-id)
```

### 3. Configure the provider in `pg_hardstorage.yaml`

```yaml
kms:
  providers:
    - kek_ref: vault-transit://vault.acme.example.com:8200/transit/db-kek
      config:
        role_id:   <ROLE_ID>
        secret_id: <SECRET_ID>
        # Or, if a sidecar (vault-agent) materialises a token file:
        # token_file: /var/run/secrets/vault/token
```

The provider exchanges `role_id` + `secret_id` for a token at
Open. Tokens get auto-renewed for the configured TTL.

### 4. Reference the KEK from the deployment

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: file:///srv/pg_hardstorage/repo
    kek_ref: vault-transit://vault.acme.example.com:8200/transit/db-kek
```

### 5. Take the first encrypted backup

```bash
pg_hardstorage backup db1 --encrypt
```

### 6. Verify

```bash
pg_hardstorage kms verify --repo file:///srv/pg_hardstorage/repo
```

## Key versioning

Vault Transit handles versioning internally. Decrypt picks the
version from the ciphertext prefix; Encrypt uses the latest
version unless `key_version` is supplied. Operators rotating
keys do:

```bash
vault write -f transit/keys/db-kek/rotate
```

Old ciphertexts continue to decrypt under the old version
until trimmed
(`vault write transit/keys/db-kek/config min_decryption_version=N`).

For pg_hardstorage's [KEK rotation](../operating/rotate-kek.md)
flow, the manifests get rewrapped under a new KEK reference;
within a single Vault Transit key, in-place rotation is
transparent and doesn't require pg_hardstorage involvement.

## Auth method alternatives

`role_id` + `secret_id` is the simplest robust pattern. Other
options that are out of scope for the in-binary v1.0 surface:

- **Kubernetes auth:** run `vault agent` as a sidecar that
  authenticates via the projected service account token and
  materialises a `VAULT_TOKEN`-bearing file. Reference that
  file via `token_file:` in the provider config.
- **AWS IAM auth:** same pattern — `vault agent` does the
  exchange and writes a file the provider reads.
- **OIDC / JWT:** delegated to the operator's identity broker.

## FIPS posture

HashiCorp Vault Enterprise has FIPS 140-2 Level 1 and Level 2
validated builds (`vault-fips`). Set
`use_fips_mode: true` in the provider config — the SDK doesn't
expose the server-side build flavour, so the declaration is a
documentation handshake.

## Crypto-shred

```bash
vault delete transit/keys/db-kek

pg_hardstorage audit append kms.shred --repo s3://acme-backups/ \
    --reason "GDPR Art 17 #4421; deleted vault transit/keys/db-kek"
```

`kms shred` destroys the *local* keyring KEK; a Vault-transit-wrapped
backup is crypto-shredded in Vault. `vault delete transit/keys/db-kek`
issues `DELETE transit/keys/db-kek`. Vault refuses
unless `deletion_allowed=true` is set — see *What you need*.
On refusal you get a clear error pointing at the remediation
step.

See [Crypto-shred](../operating/crypto-shred.md) for the full
flow.

## Troubleshooting

**`permission denied`** — the policy is short. Check the path
exactly matches the key name and the auth method's
`token_policies`.

**`deletion is not allowed for this key`** — set
`deletion_allowed=true` on the key (see step in *What you need*).

**Connection refused** — air-gap policy may be denying the host.
Add `vault.acme.example.com:8200` to `airgap.allowlist`.

## Next steps

- [Rotate the KEK](../operating/rotate-kek.md)
- [Crypto-shred](../operating/crypto-shred.md)
- [`kms` CLI reference](../../reference/cli/pg_hardstorage_kms.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)

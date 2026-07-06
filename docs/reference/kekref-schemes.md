<!-- AUTO-GEN candidate: emit from kms.DefaultRegistry.Schemes() and per-provider parseKEKRef godoc; per docs/DOC_PLAN.md auto-generation map. -->
---
title: KEKRef schemes
description: The six KEK-provider URL schemes — URL form, auth chain, FIPS posture, and Shred semantics.
tags:
  - reference
  - kms
  - encryption
  - fips
---

# KEKRef schemes

A **KEKRef** is the manifest-stamped pointer to the
Key-Encryption Key that wraps a backup's per-backup DEK.
The first segment of the KEKRef selects how the DEK is
unwrapped. `local:` is resolved inline by the keystore
([`internal/backup/keystore/unwrap.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/backup/keystore/unwrap.go)),
*not* via `kms.DefaultRegistry`. Every other (cloud / HSM)
scheme — `aws-kms`, `gcp-kms`, `azure-kv`, `vault-transit`,
`pkcs11` — selects a `kms.Provider` implementation in
`internal/kms` and is dispatched by
[`kms.DefaultRegistry`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/kms/kms.go).

| Scheme | Provider | Built in | FIPS posture | Source |
| --- | --- | --- | --- | --- |
| `local:` | On-disk keystore | yes (default builds) | host-OS crypto only | `internal/backup/keystore/kek.go` |
| `aws-kms://` | AWS KMS | yes | FIPS 140-2 L3 in FIPS regions | `internal/plugin/kms/awskms/` |
| `gcp-kms://` | GCP KMS | yes | FIPS 140-2 L3 (HSM protection level) | `internal/plugin/kms/gcpkms/` |
| `azure-kv://` | Azure Key Vault | yes | FIPS 140-2 L2 (Standard) / L3 (Premium / Managed HSM) | `internal/plugin/kms/azurekv/` |
| `vault-transit://` | HashiCorp Vault Transit | yes | FIPS 140-2 L1 / L2 (Vault Enterprise FIPS builds) | `internal/plugin/kms/vaulttransit/` |
| `pkcs11://` | PKCS#11 / HSM | gated by `-tags pkcs11` | module-validated; typically FIPS 140-2 / 140-3 | `internal/plugin/kms/pkcs11/` |

The 24-month back-compat commitment that applies to every
v1 schema also covers the on-disk KEKRef strings: a backup
manifest written today decrypts unchanged through the v1.x
release line.

---

## `local:`

```
local:default
```

The KEK lives in the per-deployment keystore at
`<keyring>/kek.bin` (`internal/backup/keystore`).
`local:default` is the only segment v0.1+ accepts; the trailing
identifier is reserved for future named-keystore selection.

| Field | Value |
| --- | --- |
| **Auth** | filesystem ACLs on the keystore directory |
| **FIPS** | follows the host's `crypto/aes` posture; FIPS only when the binary is `pg_hardstorage-fips` |
| **Shred** | `kms shred` typed-confirms, then erases `kek.bin`; immediately and irrevocably destroys all backups bound to the key |
| **Air-gap** | local file; no network |

`local:` is the default for `pg_hardstorage init` when no
cloud KMS is configured.  Operators upgrading to a
cloud-KEK do `pg_hardstorage kms migrate` to re-wrap each
manifest's DEK under the new KEK.

---

## `aws-kms://`

```
aws-kms://<key-id-or-arn>
aws-kms://alias/<alias-name>
```

Examples:

```
aws-kms://arn:aws:kms:us-east-1:123456789012:key/abcd1234-…
aws-kms://alias/pg-hardstorage-prod
aws-kms://12345678-1234-1234-1234-123456789012
```

The host part is parsed off and handed verbatim to the
SDK's `KeyId` parameter — AWS accepts ARNs, key-IDs, and
alias references in the same field.  The KEK never leaves
AWS KMS; we only ever see `Encrypt` / `Decrypt` ciphertext.

| Field | Value |
| --- | --- |
| **Auth** | AWS SDK v2 default credential chain (env vars → IRSA → EC2 IAM role → profile → SSO) |
| **FIPS** | `FIPSMode()` true when the operator points at a FIPS endpoint (`aws_use_fips_endpoint`) or a FIPS region (`us-gov-west-1` / `us-gov-east-1` / `us-east-1` / `us-west-2`) |
| **Shred** | `kms:ScheduleKeyDeletion` with a 7-30 day pending window (default 30) |
| **Air-gap** | works through a VPC endpoint (private IP); the routable-private-IP allowlist accepts it |

---

## `gcp-kms://`

```
gcp-kms://projects/<proj>/locations/<loc>/keyRings/<ring>/cryptoKeys/<key>
gcp-kms://projects/<proj>/locations/<loc>/keyRings/<ring>/cryptoKeys/<key>/cryptoKeyVersions/<v>
```

The version-bearing form is **required** for `Shred` —
GCP destroys versions, not keys.  Wrap and Unwrap accept
either form.

| Field | Value |
| --- | --- |
| **Auth** | Application Default Credentials (env vars → metadata service → `gcloud auth`) |
| **FIPS** | operator-declared via `WithFIPSMode`; HSM-protection-level keys are FIPS 140-2 L3 |
| **Shred** | `DestroyCryptoKeyVersion` on the version in the KEKRef; key material is destroyed after the parent key's `destroy_scheduled_duration` (default 24h) |
| **Air-gap** | reachable via Private Google Access; `endpoint=` URL parameter routes through the VPC |

---

## `azure-kv://`

```
azure-kv://<vault-name>/<key-name>
azure-kv://<vault-name>/<key-name>/<version>
```

Sovereign clouds (US Gov, Azure China) supply a dotted host:

```
azure-kv://acmevault.vault.azure.cn/db-backup-kek
```

The version-pinned form is required for `Shred`.  Bare
account name implies `.vault.azure.net`; a dotted name is
taken literally.

| Field | Value |
| --- | --- |
| **Auth** | `azidentity.NewDefaultAzureCredential` (env → managed identity → Azure CLI → IDE auth) |
| **FIPS** | Standard tier = FIPS 140-2 L2; Premium / Managed HSM = L3; not reported by the SDK, declared by the operator |
| **Shred** | `DeleteKey` (soft-delete; recovery window 7-90 days, default 90).  `pg_hardstorage` never issues `purge` directly — operators do that out-of-band |
| **Air-gap** | private endpoint via Azure Private Link |

---

## `vault-transit://`

```
vault-transit://<host[:port]>/<mount>/<key-name>
```

Examples:

```
vault-transit://vault.acme.example.com:8200/transit/db-kek
vault-transit://10.0.0.5:8200/secrets-eu/transit/db-kek
vault-transit://http+vault.internal:8200/transit/db-kek   # plaintext
```

Multi-segment mounts are honoured; the **last** path
segment is always the key name, everything before is the
mount.  The default scheme is HTTPS; prefix the host with
`http+` to force plaintext (in-cluster Vault).

| Field | Value |
| --- | --- |
| **Auth** | `VAULT_TOKEN` env by default; `role_id` + `secret_id` in the `cfg` map for AppRole |
| **FIPS** | server-side; reported by the operator (Vault Enterprise `vault-fips` build) |
| **Shred** | `DELETE transit/keys/<name>` — refused unless the key was previously configured with `deletion_allowed=true` |
| **Air-gap** | typically self-hosted; routable-private-IP allowlist accepts it |

Versioning is internal to Vault: `Decrypt` picks the
version from the ciphertext prefix; `Encrypt` uses the
latest unless `key_version` is supplied.

---

## `pkcs11://`

```
pkcs11://<token-label>/<key-label>?module=<path>&pin=<pin>&mech=<mechanism>&slot=<id>
```

Example:

```
pkcs11://prod-token/db-kek?module=/usr/lib/softhsm/libsofthsm2.so&pin_source=/etc/pg_hardstorage/pin
```

Query parameters:

| Parameter | Meaning |
| --- | --- |
| `module` | Absolute path to the `.so`/`.dll`; falls back to `$PKCS11_MODULE_PATH` |
| `pin` | PIN inline (test convenience) |
| `pin_source` | Path to a mode-`0600` file holding the PIN — the production posture |
| `mech` | `aes-gcm` (default) or `rsa-oaep` |
| `slot` | Numeric slot id; default auto-resolved from the token label |

| Field | Value |
| --- | --- |
| **Auth** | PIN-bound C_Login against the token; `pin_source` file owns the secret |
| **FIPS** | module-validated; the device's certificate sets the level |
| **Shred** | `C_DestroyObject` on the key handle.  Real HSMs may gate the destroy behind operator-card / quorum policies; refusal surfaces wrapped in `kms.ErrShredFailed` |
| **Build** | requires `make build-pkcs11` (CGO + `-tags pkcs11`); default builds keep the scheme registered but every operation returns "binary built without `-tags pkcs11`" |

The two wrap mechanisms differ in envelope format:

- `aes-gcm` — symmetric `CKK_AES` KEK; envelope is
  `[12-byte IV | ciphertext | 16-byte GCM tag]`.
- `rsa-oaep` — RSA key pair; envelope is opaque RSA
  ciphertext, length = modulus size.

## See also

- [Build flavours](build-flavours.md) — which binary
  carries which provider.
- [Plugins → Encryption (KMS) contract](plugins/encryption-contract.md) —
  the full Provider interface.
- [How-to → Crypto-shred a backup](../how-to/operating/crypto-shred.md)
  — operator workflow for the Shred verb.
- [How-to → Rotate the KEK](../how-to/operating/rotate-kek.md)
  — re-wrapping every manifest's DEK under a new KEK.
- [Explanation → Envelope encryption](../explanation/envelope-encryption.md)
  — why DEK / KEK separation, and what `Shred` actually destroys.

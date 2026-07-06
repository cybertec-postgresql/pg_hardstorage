---
title: Add a PKCS#11 / HSM KMS
description: KEKRef format, the -tags pkcs11 build flavour, and
              token / key labelling for hardware HSMs.
tags:
  - kms
  - pkcs11
  - hsm
  - fips
---

# Add a PKCS#11 / HSM KMS

> The PKCS#11 provider talks to an external HSM module — Thales
> nCipher, Utimaco, AWS CloudHSM, YubiHSM2, or SoftHSM2 for
> testing. The KEK never leaves the HSM; every wrap / unwrap
> round-trips through the device.

## KEKRef format

```text
pkcs11://<token-label>/<key-label>?module=<path>&pin=<pin>&mech=<mechanism>&slot=<id>
```

| Component | Meaning |
| --- | --- |
| `<token-label>` | PKCS#11 token label (`CKA_LABEL` on the token). |
| `<key-label>` | `CKA_LABEL` on the key object inside that token. |

Two label strings — no slot-ID memorisation. The plugin
auto-resolves slot from the token label (override with
`?slot=<id>` for the rare case where one slot exposes multiple
tokens with the same label).

Query parameters:

| Key | Meaning |
| --- | --- |
| `module` | Absolute path to the `.so` / `.dll`. Falls back to `$PKCS11_MODULE_PATH`. |
| `pin` | PIN inline. Test only — leaks through process listings. |
| `pin_source` | Path to a `mode 0600` file holding the PIN. Production path. |
| `mech` | `aes-gcm` (default) or `rsa-oaep`. |
| `slot` | Numeric slot id; default auto-resolved from token label. |

## Build flavour

PKCS#11 requires CGo to call `libpkcs11`. The standard release
build (`CGO_ENABLED=0`) includes this scheme in the registry
but every operation returns a structured
`binary built without -tags pkcs11` error. Two ways to get the
real backend:

```bash
# Build from source with the HSM tag
go build -tags pkcs11 ./cmd/pg_hardstorage
```

Or pull the official `pg-hardstorage-fips` artifact — built
with `-tags pkcs11` by default since FIPS-validated HSM-backed
envelopes are the canonical FIPS posture. See
[the FIPS variant build how-to](../packaging/fips-variant.md)
for the full build recipe.

## What you need

- An HSM module with a `.so` / `.dll` library installed.
- A token initialised with a PIN.
- A key created with `CKA_LABEL` and `CKA_WRAP=true` /
  `CKA_UNWRAP=true`. Symmetric AES-256 for `aes-gcm`; RSA
  keypair for `rsa-oaep`.
- A `pg_hardstorage` binary built with `-tags pkcs11`.

## Steps

### 1. Smoke-test with SoftHSM2 (dev)

```bash
softhsm2-util --init-token --slot 0 --label test-token \
    --pin 1234 --so-pin 5678
pkcs11-tool \
    --module /usr/lib/softhsm/libsofthsm2.so \
    --token-label test-token --pin 1234 \
    --keygen --key-type AES:32 --label db-kek
```

```bash
pg_hardstorage kms verify \
    --repo file:///srv/pg_hardstorage/repo \
    --kek-ref 'pkcs11://test-token/db-kek?module=/usr/lib/softhsm/libsofthsm2.so&pin=1234'
```

`kms verify --repo … --kek-ref` reads the key's metadata only —
confirms the binary has the PKCS#11 backend and the URL resolves.
No mutation.

### 2. Production: file-based PIN

Write the PIN to a `0600` file owned by the agent user:

```bash
install -m 0600 -o pg_hardstorage <(echo -n 1234) /etc/pg_hardstorage/keys/hsm.pin
```

Reference it from the KEKRef:

```yaml
deployments:
  db1:
    kek_ref: 'pkcs11://prod-token/db-kek?module=/opt/nfast/libcknfast.so&pin_source=/etc/pg_hardstorage/keys/hsm.pin'
```

The KEKRef is stored verbatim in every backup manifest, so the
file path stays the same across the agent's lifecycle.

### 3. RSA-OAEP mechanism

For RSA-keyed HSMs (Thales nCipher with RSA-only policy, etc.):

```yaml
deployments:
  db1:
    kek_ref: 'pkcs11://prod-token/db-kek-rsa?mech=rsa-oaep&module=/opt/nfast/libcknfast.so&pin_source=/etc/pg_hardstorage/keys/hsm.pin'
```

The `Wrap` operation calls `C_Encrypt` under the public half;
`Unwrap` calls `C_Decrypt` under the private half. The wrapped
form is opaque ciphertext (length = RSA modulus).

### 4. Take the first encrypted backup

```bash
pg_hardstorage backup db1 --encrypt
```

### 5. Verify the envelope

```bash
pg_hardstorage kms verify --repo file:///srv/pg_hardstorage/repo
```

## Wrap mechanisms

| Mechanism | KEK type | Wrap call | Wrapped form |
| --- | --- | --- | --- |
| `aes-gcm` (default) | `CKK_AES` | `C_Encrypt` with `CKM_AES_GCM`, fresh 12-byte IV | `[12-byte IV | ciphertext | 16-byte GCM tag]` |
| `rsa-oaep` | RSA keypair | `C_Encrypt` with `CKM_RSA_PKCS_OAEP` | opaque ciphertext (RSA modulus length) |

## FIPS posture

HSMs are typically FIPS 140-2 / 140-3 validated; the module
itself is the validated cryptographic boundary. Set
`use_fips_mode: true` in the provider config — the PKCS#11
standard exposes no portable "am I FIPS?" predicate, so the
declaration is a documentation handshake.

## Crypto-shred

Shred calls `C_DestroyObject` on the key handle. Real HSMs
frequently have additional policy gates (operator-card
threshold, ACS quorum) that may refuse the destroy; those
errors propagate as `ErrShredFailed`. See
[Crypto-shred](../operating/crypto-shred.md).

## Troubleshooting

**`binary built without -tags pkcs11`** — you're on the
standard build. Rebuild with the tag, or switch to
`pg-hardstorage-fips`.

**`module not found`** — the path is wrong, or the module's
shared dependencies aren't installed. `ldd /opt/nfast/libcknfast.so`
on Linux to spot missing libraries.

**`CKR_PIN_INCORRECT`** — wrong PIN, or PIN locked. Many HSMs
lock the user PIN after 3 wrong tries; recovery typically
involves the SO PIN.

**`CKR_KEY_HANDLE_INVALID`** — the `<key-label>` doesn't
resolve to an object. Confirm with `pkcs11-tool --list-objects`.

## Next steps

- [Rotate the KEK](../operating/rotate-kek.md)
- [Crypto-shred](../operating/crypto-shred.md)
- [`kms` CLI reference](../../reference/cli/pg_hardstorage_kms.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)

---
title: Rotate the KEK
description: Re-wrap every committed manifest's DEK under a new
              KEK without re-encrypting chunks.
tags:
  - kms
  - rotation
  - encryption
---

# Rotate the KEK

> KEK rotation re-wraps every committed manifest's DEK under
> a new KEK. **Chunks are not re-encrypted** — per-chunk keys
> are derived via HKDF from the (unchanged) BDEK, so rotation
> is O(manifest count), not O(chunk count).

## What you need

- The **old** KEK bytes (32 raw bytes) — typically read from
  the keyring's `kek.bin`. Keep a copy of the keyring backed
  up before rotation; without it, old manifests can't be
  unwrapped.
- The **new** KEK bytes (32 raw bytes). Generate with:

  ```bash
  head -c 32 /dev/urandom > /tmp/new-kek.bin
  chmod 0600 /tmp/new-kek.bin
  ```

- The new KEK reference (the `kek_ref` you'll record in
  rotated manifests).

## Steps

### 1. Preview (dry-run)

```bash
pg_hardstorage kms rotate \
    --repo file:///srv/pg_hardstorage/repo \
    --old-kek-file /etc/pg_hardstorage/keys/kek.bin \
    --old-kek-ref local://main \
    --new-kek-file /tmp/new-kek.bin \
    --new-kek-ref  local://main-2026-q2
```

```console
rotation preview
  manifests scanned:        1247
  matched (old kek_ref):    1184
  already_rotated:             0
  would_rewrite:            1184
  unrelated_kek_ref:          63
```

`unrelated_kek_ref` is the multi-tenant safety: manifests
wrapped under a different KEK ref are skipped, not failed.

### 2. Apply

```bash
pg_hardstorage kms rotate \
    --repo file:///srv/pg_hardstorage/repo \
    --old-kek-file /etc/pg_hardstorage/keys/kek.bin \
    --old-kek-ref local://main \
    --new-kek-file /tmp/new-kek.bin \
    --new-kek-ref  local://main-2026-q2 \
    --apply
```

```console
rotation applied
  manifests scanned:        1247
  rewritten:                1184
  already_rotated:             0
  unrelated_kek_ref:          63
  duration:                 47.2s
```

The command rewrites each matching manifest atomically: decrypt
the wrapped DEK with the old bytes, re-wrap with the new bytes,
mutate the `encryption.kek_ref` and `encryption.wrapped_dek`
fields, re-sign with the operator's signing keypair (unchanged),
and atomically replace the manifest at its repo key. The replica
copy is kept in sync.

### 3. Swap the active KEK

The rotation rewrote manifests; future backups should also
use the new KEK. Update the keyring:

```bash
mv /etc/pg_hardstorage/keys/kek.bin /etc/pg_hardstorage/keys/kek.bin.old
cp /tmp/new-kek.bin              /etc/pg_hardstorage/keys/kek.bin
chmod 0600                        /etc/pg_hardstorage/keys/kek.bin
chown pg_hardstorage:             /etc/pg_hardstorage/keys/kek.bin
```

Restart the agent so the next backup picks up the new KEK:

```bash
systemctl restart pg_hardstorage-agent
```

### 4. Verify

```bash
pg_hardstorage kms verify --repo file:///srv/pg_hardstorage/repo
```

Walks every committed manifest and confirms the wrapped DEK
unwraps under the resolved KEK. Mismatches return exit 9.

## Multi-tenant safety

Only manifests with `--old-kek-ref` are touched. Manifests
wrapped under different KEK refs (other tenants) are
**skipped**, not failed. The result body's
`unrelated_kek_ref` count tracks them so a 0 there means
"every manifest in the repo got reconsidered."

Operators rotating per-tenant KEKs run the command once per
tenant, with each run's `--old-kek-ref` / `--new-kek-ref`
matching that tenant's references.

## Resumability

A rotation interrupted partway through is safely re-runnable
with the same args. Manifests already rotated (their
`kek_ref == --new-kek-ref`) are counted as `already_rotated`
and skipped. Loop until `would_rewrite == 0`:

```bash
while pg_hardstorage kms rotate ... --apply --quiet \
    | jq '.result.body.would_rewrite' | grep -qv '^0$'; do :; done
```

## Cloud-KMS rotation

When the KEK lives in a cloud KMS (AWS / GCP / Azure / Vault
Transit / PKCS#11), there are two flavours of rotation:

- **In-place version rotation** (most common). The provider's
  native rotation command bumps the version; old ciphertexts
  continue to decrypt under the old version automatically.
  pg_hardstorage doesn't need to be involved. See:
  - [`vault write -f transit/keys/<name>/rotate`](../adding/kms-vault.md#key-versioning)
  - [GCP KMS key rotation](https://cloud.google.com/kms/docs/rotate-key)
  - [Azure Key Vault key rotation](https://learn.microsoft.com/en-us/azure/key-vault/keys/how-to-configure-key-rotation)
  - [AWS KMS automatic key rotation](https://docs.aws.amazon.com/kms/latest/developerguide/rotate-keys.html)

- **Reference rotation** (rare). The KEK reference itself
  changes — for instance, when migrating from
  `aws-kms://alias/old` to `aws-kms://alias/new`. Use
  `pg_hardstorage kms rotate` with the old and new
  `kek_ref` values; the provider handles the underlying
  unwrap/wrap.

## Audit

With `--apply`, one `kms.rotate` event is emitted to the
[audit chain](../../operations/operator-guide.md#8-audit-log)
per rotated manifest. The chain remains consistent across
rotation; nothing about the audit log changes.

## Troubleshooting

**`kms.rotate.old_kek_mismatch`** — the bytes at
`--old-kek-file` don't unwrap a manifest's `wrapped_dek`. Did
you point at the wrong keyring? Did the keyring change between
backup and rotate? Compare against `pg_hardstorage kms inspect`.

**`unrelated_kek_ref` is non-zero and unexpected** — the repo
contains manifests from a tenant you didn't expect. Confirm
with `pg_hardstorage list --tenant`.

**Replica copy out of sync** — `repo check` flags it; re-run
the rotation against the replica with the same args to bring
both repos back to parity.

## Next steps

- [Crypto-shred](crypto-shred.md) — the GDPR Art. 17 path
- [Verify backups](verify-fast-vs-full.md)
- [`kms rotate` CLI reference](../../reference/cli/pg_hardstorage_kms_rotate.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)

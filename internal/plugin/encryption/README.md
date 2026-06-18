# plugin/encryption/

The encryption tier: symmetric per-chunk cipher implementations. Today it's
AES-256-GCM; the interface keeps the door open for sibling ciphers.

## What lives here

The `Encryptor` interface, the HKDF-based per-chunk key derivation, and the only
shipping implementation (`aesgcm`). Encryption runs **after** compression and
**before** the storage plugin; the KMS tier owns the KEK that wraps each chunk's
DEK.

## Encryptor interface

`Seal(ctx, dek, plaintext) (ciphertext, nonce, tag)`, `Open(ctx, dek,
ciphertext, nonce, tag) plaintext`, `KeySize`, `NonceSize`, `Overhead`.

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `aesgcm` | AES-256-GCM with HKDF-SHA256 per-chunk subkey derivation | real |

## Key files

- `encryption.go` — `Encryptor` interface, registry, mode constants
- `encryption_test.go` — interface-conformance tests
- `keywrap.go` / `keywrap_test.go` — RFC 5649 AES key-wrap used by KMS plugins
  to wrap DEKs
- `aesgcm/` — the shipping cipher

## Threat model notes

- Per-chunk DEK derived via HKDF from a manifest-level seed; no nonce reuse
  across chunks.
- Authentication tag covers (chunk header || ciphertext || AAD-from-manifest);
  tampering with the manifest invalidates the tag.
- FIPS build (`-tags fips`) routes AES-GCM through `crypto/internal/boring`.

## Read next

- `../kms/README.md` — where the KEK lives
- `../../backup/keystore/` — manifest-side DEK handling
- `docs/explanation/encryption.md` — the threat model in full

## Don't put X here

- KEK material — that's the KMS tier.
- Asymmetric crypto — manifests are signed (Ed25519) in
  `internal/backup/sign.go`, not here.
- Compression — separate tier; chunker decides order.

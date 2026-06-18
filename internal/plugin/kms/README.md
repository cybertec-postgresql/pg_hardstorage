# plugin/kms/

The KMS tier: KEK (key-encryption-key) custody. Generates DEKs, wraps them for
at-rest storage, unwraps them on restore, rotates KEKs, and shreds keys on
retention expiry.

## What lives here

Implementations of the `EncryptionPlugin` interface. The KMS plugin never sees a
chunk's plaintext — it only handles 32-byte DEK material wrapped for the
underlying provider. Manifests record which KEK ID wrapped each DEK so unwrap is
deterministic at restore time.

## EncryptionPlugin interface

`GenerateDEK(ctx) (dek, wrapped, keyID)`, `UnwrapDEK(ctx, wrapped, keyID) dek`,
`RotateKEK(ctx, oldID) newID` (rewraps existing DEKs without re-encrypting
chunks), `Shred(ctx, keyID)` (destroy KEK; the DEKs become unrecoverable).

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `awskms` | AWS KMS (CMKs, multi-region keys, grants) | real |
| `azurekv` | Azure Key Vault (Software + HSM SKUs) | real |
| `gcpkms` | Google Cloud KMS (software + Cloud HSM) | real |
| `vaulttransit` | HashiCorp Vault Transit secrets engine | real |
| `pkcs11` | PKCS#11 HSMs (SoftHSM, YubiHSM, Luna, CloudHSM) | build-tagged |
| `local-aesgcm` | Passphrase-derived KEK for dev / lab clusters | real |

## Key files

- `awskms/`, `azurekv/`, `gcpkms/`, `vaulttransit/` — cloud KMS clients
- `pkcs11/` — PKCS#11 binding; requires the `pkcs11` build tag because it
  needs cgo + a vendor library
- (`local-aesgcm` lives under `../encryption/` for code-share; registered as a
  KMS factory)

## Read next

- `../encryption/README.md` — what the unwrapped DEK feeds into
- `../../backup/keystore/` — manifest-side KEK ID tracking + rewrap logic
- `docs/how-to/configure-kms.md` — provider-by-provider setup

## Don't put X here

- Chunk encryption — KMS handles keys, not data.
- Manifest signing — that's Ed25519, separate trust root, in
  `internal/backup/sign.go`.
- Passphrase prompting — UI lives in `internal/cli`; KMS plugins receive
  material via config.

# kms/

KEK-provider abstraction: resolves a manifest's `KEKRef` to a 32-byte
Key-Encryption Key.

## What lives here

A thin dispatcher that maps a `KEKRef` scheme (`local:`, `aws-kms://`,
`gcp-kms://`, `azure-kv://`, `vault-transit://`, `pkcs11:`) to a concrete
provider. The provider unwraps the per-backup DEK that lives on the manifest,
which is what lets `restore` decrypt chunks. This package is the glue; the
actual cloud / HSM clients live in `internal/plugin/kms/`.

## Key files

- `kms.go` — `Provider` interface, `DefaultRegistry`, scheme dispatch
- `kms_test.go` — registry + scheme-parsing coverage

## Read next

- `../plugin/kms/README.md` if present — concrete adapters: `awskms/`,
  `azurekv/`, `gcpkms/`, `pkcs11/`, `vaulttransit/`
- `../backup/keystore/` — local-KEK custody on disk (`local:` scheme)
- `../threshold/README.md` — multi-party authorization for KEK rotation /
  shred
- `../../docs/reference/kekref-schemes.md` — user-facing scheme reference
- `../README.md` — parent index

## Don't put X here

- Concrete cloud-KMS client code — that's `internal/plugin/kms/<provider>/`.
- Symmetric chunk encryption — that's `internal/plugin/encryption/aesgcm/`.
- DEK wrap/unwrap arithmetic — that's `internal/plugin/encryption/keywrap.go`.

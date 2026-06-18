---
title: Build the PKCS#11 variant
description: Build a `pg_hardstorage-pkcs11` binary that talks to
              an HSM via PKCS#11.
tags:
  - packaging
  - pkcs11
  - hsm
  - kms
---

# Build the PKCS#11 variant

> Compile a `pg_hardstorage-pkcs11` binary that activates
> the cgo-backed PKCS#11 KMS provider via
> `github.com/miekg/pkcs11`. The binding wraps a libpkcs11
> module (Yubico, OpenSC, AWS CloudHSM, SoftHSM, Thales,
> …) at runtime through `dlopen`; the binding itself is C,
> hence CGO.

## What you need

- A C toolchain reachable to cgo
  (`build-essential` on Debian).
- `CGO_ENABLED=1` (the build target sets this).
- One-time: vendor the PKCS#11 binding into your fork's
  `go.mod` (the file is gated behind a build tag so it
  isn't in the default `go.mod`):

  ```bash
  go get github.com/miekg/pkcs11@v1.1.1
  ```

  This is a one-shot per fork. Subsequent builds run
  without it.

- At runtime: a PKCS#11 module library on the host
  (e.g. `/usr/lib/softhsm/libsofthsm2.so`,
  `/usr/lib/x86_64-linux-gnu/opensc-pkcs11.so`,
  `/opt/yubihsm/libyubihsm_pkcs11.so`).

## Steps

### 1. Vendor the binding (one-time per fork)

```bash
go get github.com/miekg/pkcs11@v1.1.1
```

CI's tag-build smoke job runs this on a throwaway checkout
to validate the file compiles; production packagers carry
the dep in their fork.

### 2. Build the binary

```bash
make build-pkcs11
```

Equivalent direct invocation:

```bash
CGO_ENABLED=1 go build -tags pkcs11 \
    -trimpath \
    -ldflags '-s -w
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=$(VERSION)
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=$(COMMIT)
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=$(DATE)' \
    -o bin/pg_hardstorage-pkcs11 \
    ./cmd/pg_hardstorage
```

### 3. Configure a KEK against your HSM

```yaml
# /etc/pg_hardstorage/pg_hardstorage.yaml
kms:
  default:
    type: pkcs11
    pkcs11:
      module: /usr/lib/softhsm/libsofthsm2.so
      slot: 0
      pin_file: /etc/pg_hardstorage/keyring/hsm-pin
      key_label: pg_hardstorage_master_kek
```

The `module:` path is whatever your HSM vendor ships;
`slot:` and `key_label:` come from your HSM admin console.
`pin_file` is mode-0600, owner `pgbackup`, one-line file
containing the PIN — the same posture as the rest of the
keyring directory.

### 4. Run `kms verify`

```bash
pg_hardstorage-pkcs11 kms verify --kek-ref pkcs11://default
```

This is the post-install smoke check: round-trip a tiny
DEK through wrap / unwrap to prove the HSM is reachable
and the credentials work.

### 5. Confirm `doctor` reports the KMS posture

```bash
pg_hardstorage-pkcs11 doctor
```

The system section lists every configured KEK and reports
"reachable / unreachable" for each.

## What just happened

The `pkcs11` build tag activates `internal/plugin/encryption/pkcs11`,
which holds the `EncryptionPlugin` implementation backed by
`miekg/pkcs11`. At runtime the provider:

1. `C.dlopen`s the configured `module:` path.
2. `C_Initialize`s the module.
3. Authenticates against the configured `slot:` with the
   PIN from `pin_file`.
4. For each `GenerateDEK` call: emits a session key locally,
   wraps it on the HSM via `C_WrapKey` against the
   `key_label` master KEK, returns `(plaintext_dek,
   wrapped_dek)`.
5. For each `UnwrapDEK` call: passes the wrapped DEK back
   to the HSM, gets the plaintext DEK out.

The plaintext DEK lives in process memory only as long as
the chunk it encrypts is being processed; the wrapped DEK
is what gets persisted in the manifest's `encryption.wrapped_dek`
field. The master KEK never leaves the HSM.

## What this gives you

| Property | Why it matters |
| --- | --- |
| KEK never leaves the HSM | Compromise of the agent host doesn't expose the master key. |
| Hardware-rooted key custody | Audit / compliance: the master KEK is FIPS-approved, hardware-bound. |
| Per-tenant DEKs unchanged | Same envelope shape as the file-keyring path; switching providers is a config flip. |

## Troubleshooting

### `cgo: C compiler not found`

Install a C toolchain
(`build-essential` on Debian, `Development Tools` on
Fedora). The PKCS#11 binding is C; cgo is mandatory.

### `dlopen failed: cannot open shared object file`

The `module:` path is wrong, or the module isn't installed
on the agent host. List candidates:

```bash
find / -name 'libsofthsm2.so' -o -name 'opensc-pkcs11.so' 2>/dev/null
```

Try the path explicitly with `pkcs11-tool` first:

```bash
pkcs11-tool --module /usr/lib/softhsm/libsofthsm2.so --list-slots
```

If `pkcs11-tool` works against the module, the binary will
too.

### `CKR_PIN_INCORRECT`

PIN file mismatch. Confirm `pin_file` is mode-0600, owned
by `pgbackup`, and contains exactly the PIN with no
trailing whitespace beyond a single optional newline.

### Module reports `CKR_TOKEN_NOT_PRESENT`

Slot isn't initialised. SoftHSM example:

```bash
softhsm2-util --init-token --slot 0 --label pg_hardstorage \
    --pin <pin> --so-pin <so-pin>
```

Or use the vendor's HSM admin console; the binary doesn't
provision tokens itself.

### Binary works on build host but not on a different one

PKCS#11 modules are typically not portable — the binding
works against any module, but the module itself is host-
specific. Install the module package on the runtime host
(`softhsm2`, `opensc-pkcs11`, the vendor's package).

## Next steps

- [Build from source](build-from-source.md) — the default
  build.
- [Build the FIPS variant](fips-variant.md) — combine FIPS
  + PKCS#11 for the most regulated environments.
- [`kms verify` CLI reference](../../reference/cli/pg_hardstorage_kms_verify.md)
  — the smoke-test command this page calls.

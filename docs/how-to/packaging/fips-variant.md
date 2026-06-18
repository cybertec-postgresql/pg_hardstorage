---
title: Build the FIPS variant
description: Build `pg_hardstorage` against Go's BoringCrypto
              (FIPS 140-2 validated module).
tags:
  - packaging
  - fips
  - boringcrypto
  - compliance
---

# Build the FIPS variant

> Compile a `pg_hardstorage-fips` binary that routes every
> `crypto/tls`, `crypto/aes`, `crypto/sha256`, … call
> through Google's FIPS 140-2 validated BoringCrypto module.
> CGO required; Linux/amd64 only.

## What you need

- Go 1.19 or later. `GOEXPERIMENT=boringcrypto` is built
  into the upstream Go toolchain on `linux/amd64`; on
  every other GOOS/GOARCH the experiment fails with a
  clear error.
- A C toolchain reachable to cgo. On Debian:

  ```bash
  sudo apt install build-essential
  ```

- Linux/amd64 build host (BoringSSL doesn't have a
  validated build for ARM or other arches).

## Steps

### 1. Build the FIPS binary

```bash
make build-fips
```

Equivalent direct invocation:

```bash
GOEXPERIMENT=boringcrypto CGO_ENABLED=1 \
    go build -tags fips \
        -trimpath \
        -ldflags '-s -w
            -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=$(VERSION)
            -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=$(COMMIT)
            -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=$(DATE)' \
        -o bin/pg_hardstorage-fips \
        ./cmd/pg_hardstorage
```

The `fips` build tag is the runtime selector:
`internal/fips.Enabled()` returns `true` in this flavour;
`pg_hardstorage doctor` surfaces it under the system
section so operators see at a glance which variant is
running.

### 2. Confirm BoringCrypto symbols are linked in

```bash
go tool nm bin/pg_hardstorage-fips | grep -i goboringcrypto | head -5
```

```console
0000000000abc123 T crypto/internal/boring._cgo_4ad7b8e0f3c5_Cfunc__goboringcrypto_AES_set_decrypt_key
0000000000abc456 T crypto/internal/boring._cgo_4ad7b8e0f3c5_Cfunc__goboringcrypto_AES_set_encrypt_key
...
```

The symbols' presence is your "yes, this binary actually
routes through BoringCrypto" check. Empty output means the
build silently fell back to Go's default crypto stack —
verify `GOEXPERIMENT=boringcrypto` was set during the build.

### 3. Confirm the runtime flag

```bash
./bin/pg_hardstorage-fips doctor
```

The system section reports `fips: true`. The `version`
subcommand also surfaces the flag:

```bash
./bin/pg_hardstorage-fips version
```

```console
pg_hardstorage v0.9.x
  commit: abcdef1
  ...
  fips:   true
  build:  fips
```

## What just happened

`GOEXPERIMENT=boringcrypto` swaps Go's standard crypto
implementations for BoringCrypto-backed equivalents at
compile time. The substitution is automatic and exhaustive
within the standard library: every `crypto/tls`,
`crypto/aes`, `crypto/sha256`, `crypto/hmac`, `crypto/rsa`,
`crypto/ecdsa` call routes through validated code.
Algorithms BoringCrypto doesn't validate (e.g. `chacha20poly1305`
in older versions) refuse at runtime under
`internal/fips.Enabled()` rather than silently fall back.

The `fips` build tag activates the project's own FIPS
posture: refuses non-FIPS encryption providers, refuses
unsigned manifests in compliance mode, and surfaces the
flag in `doctor` output.

## What FIPS mode actually changes for the operator

| Surface | Behaviour |
| --- | --- |
| Storage envelope | AES-256-GCM-SIV via BoringCrypto. |
| TLS to control plane / repos | Only FIPS-approved ciphersuites. |
| KMS providers | Refuses providers that aren't FIPS-approved. |
| `doctor` | Reports `fips: true` and a "FIPS mode active" event. |
| Unit / integration tests | Same suite, run with the FIPS build. |

Operators who don't need FIPS should stay on the default
build (`make build`). The FIPS variant is for regulated
environments where the validated-module requirement is the
deciding factor.

## Distribution

The packaging story is roadmap from the SPEC:

| Lands | Posture |
| --- | --- |
| v0.5 | `pg-hardstorage-fips` `.deb` / `.rpm` shipped with
  goreleaser; `Conflicts: pg-hardstorage` on Debian so the
  two binaries don't co-install. |
| v0.5+ | `ghcr.io/cybertec-postgresql/pg_hardstorage-fips:<ver>`
  distroless container image, cosign-signed, SBOM via syft. |

For v0.1 the path is "build it yourself with the
instructions on this page."

## Troubleshooting

### `GOEXPERIMENT boringcrypto: not supported on …`

You're on a non-`linux/amd64` host. The experiment is only
available on `linux/amd64`; cross-build to that arch from
elsewhere or run the build on a real Linux/amd64 box.

### `cgo: C compiler not found`

Install a C toolchain (`build-essential` on Debian,
`Development Tools` on Fedora). FIPS requires cgo because
BoringSSL is C; the wrapper links it in.

### Symbols missing in `go tool nm`

The build fell back to Go's default crypto stack. Causes:

- `GOEXPERIMENT` env var didn't reach the build (check
  with `go env GOEXPERIMENT` from the same shell).
- `CGO_ENABLED=0` overrode cgo. The Makefile target sets
  `CGO_ENABLED=1` explicitly; if you're calling `go build`
  directly, set it yourself.

### Algorithms not in BoringCrypto refused at runtime

Expected behaviour. The FIPS variant is a closed set of
validated algorithms; the project refuses anything outside
that set rather than silently falling back. If your
deployment depends on a non-FIPS algorithm you can't run on
the FIPS variant.

## Next steps

- [Build from source](build-from-source.md) — the default
  build the FIPS variant deviates from.
- [Build the PKCS#11 variant](pkcs11-variant.md) — the
  other CGO-using flavour.
- [Compliance mapping](../../compliance/index.md) — how
  the FIPS posture maps to control frameworks (SOC2, ISO,
  HIPAA, PCI, FedRAMP).

<!-- AUTO-GEN candidate: emit from Makefile build* targets and `//go:build` tag inventory; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Build flavours
description: The four pg_hardstorage binaries — what each tag activates, dependency posture, and FIPS / CGO claims.
tags:
  - reference
  - build
  - fips
  - pkcs11
  - firecracker
---

# Build flavours

`pg_hardstorage` ships from one source tree as four
related binaries.  All four implement the same v1
contract; they differ in which optional providers /
sandbox backends are linked in and what cryptographic
posture the binary claims.

| Binary | Build target | Build tag(s) | CGO | FIPS claim | Use when |
| --- | --- | --- | --- | --- | --- |
| `pg_hardstorage` | `make build` | (none) | optional | no | every-day default; runs anywhere |
| `pg_hardstorage-fips` | `make build-fips` | `fips` | required | yes (BoringCrypto) | regulated / FIPS 140-2 environments; linux/amd64 only |
| `pg_hardstorage-pkcs11` | `make build-pkcs11` | `pkcs11` | required | per-HSM | HSM-backed KEK over PKCS#11 |
| `pg_hardstorage-firecracker` | `make build-firecracker` | `firecracker` | not required by SDK | no | microVM verifier sandbox (linux + KVM) |

Common flags every target inherits from the
[`Makefile`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/Makefile):

```
GOFLAGS := -trimpath
LDFLAGS := -s -w \
           -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.date=$(BUILD_DATE)
```

Output lands in `./bin/`.

---

## `pg_hardstorage` (default)

```bash
make build
```

| Field | Value |
| --- | --- |
| **CGO** | optional; `CGO_ENABLED=$(CGO_ENABLED)` honours the operator's env (defaults to `1` on macOS, `0` on linux for static-build CI) |
| **Crypto** | Go's standard `crypto/...` packages; pure-Go fallback |
| **FIPS** | `fips.Enabled() == false`; `pg_hardstorage doctor` reports `variant: default` |
| **KMS schemes activated** | `local`, `aws-kms`, `gcp-kms`, `azure-kv`, `vault-transit`. The `pkcs11` scheme is registered but every operation returns "binary built without `-tags pkcs11`" |
| **Sandbox** | Docker backend only (`internal/verify/sandbox/backend_docker.go`) |

Runs on every supported OS / arch the Go toolchain
targets.  This is the binary `go install`,
`brew install pg_hardstorage`, and the default container
image carry.

---

## `pg_hardstorage-fips`

```bash
make build-fips
# emits: bin/pg_hardstorage-fips
```

```
GOEXPERIMENT=boringcrypto CGO_ENABLED=1 \
go build -tags fips -ldflags '$(LDFLAGS)' \
    -o bin/pg_hardstorage-fips ./cmd/pg_hardstorage
```

| Field | Value |
| --- | --- |
| **CGO** | required (BoringSSL is C) |
| **Crypto** | every `crypto/aes`, `crypto/sha256`, `crypto/ecdsa`, `crypto/rsa`, `crypto/tls` call routes through Google's BoringCrypto — the FIPS 140-2 validated module |
| **FIPS** | `fips.Enabled() == true`; `--fips-strict` accepted; doctor reports `variant: fips` |
| **OS / arch** | linux/amd64 only (BoringCrypto's supported set) |
| **Verification** | `go tool nm bin/pg_hardstorage-fips \| grep -i goboringcrypto \| head -5` |

The `fips` build tag flips the constant in
[`internal/fips/enabled_fips.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/fips/enabled_fips.go);
the audit log stamps every backup taken by this binary with
`fips: true` for compliance auditors.

There is no official `pg-hardstorage-fips` distribution
artifact today — the FIPS `.deb`/`.rpm` (which will bake in
`-tags pkcs11` so HSM-backed envelopes, the canonical FIPS
posture, work out of the box) is roadmap; see the FIPS
variant how-to. Until it ships, build FIPS yourself and add
the tag explicitly: `go build -tags 'fips pkcs11' …`.

---

## `pg_hardstorage-pkcs11`

```bash
go get github.com/miekg/pkcs11@v1.1.1   # one-time, into operator's fork
make build-pkcs11
# emits: bin/pg_hardstorage-pkcs11
```

```
CGO_ENABLED=1 go build -tags pkcs11 -ldflags '$(LDFLAGS)' \
    -o bin/pg_hardstorage-pkcs11 ./cmd/pg_hardstorage
```

| Field | Value |
| --- | --- |
| **CGO** | required (`miekg/pkcs11` wraps `libpkcs11`) |
| **Activates** | the cgo-backed PKCS#11 KMS provider in `internal/plugin/kms/pkcs11/realclient_cgo.go` |
| **HSM compat** | Thales nCipher, Utimaco, AWS CloudHSM, YubiHSM2, SoftHSM2 (any module that exposes a PKCS#11 v2.20+ interface) |
| **Loads via** | `dlopen(<module>)` at runtime — PKCS#11 module is operator-selected, not baked in |
| **FIPS** | the device's certificate sets the level; `FIPSMode()` is operator-declared via `WithFIPSMode` |

The `miekg/pkcs11` dependency is **not** in the default
`go.mod` — the file is gated behind `//go:build pkcs11`.
Operators wanting HSM vendor the dep into their fork's
`go.mod` once and rebuild from there.  CI's tag-build
smoke job runs `go get` on a throwaway checkout to validate
the file compiles.

---

## `pg_hardstorage-firecracker`

```bash
go get github.com/firecracker-microvm/firecracker-go-sdk
make build-firecracker
# emits: bin/pg_hardstorage-firecracker
```

```
CGO_ENABLED=$(CGO_ENABLED) go build -tags firecracker -ldflags '$(LDFLAGS)' \
    -o bin/pg_hardstorage-firecracker ./cmd/pg_hardstorage
```

| Field | Value |
| --- | --- |
| **CGO** | not required by the SDK; the `firecracker` process itself (which the agent execs as a subprocess) is C |
| **Activates** | the Firecracker microVM verifier-sandbox backend in `internal/verify/sandbox/backend_firecracker_real.go` |
| **Use when** | you want microVM isolation for `verify --full` instead of the default Docker backend |
| **OS** | linux + KVM only |
| **Default builds** | the same Go file `backend_firecracker_stub.go` is compiled — every microVM call returns "binary built without `-tags firecracker`" |

Same `go.mod` posture as `pkcs11`: SDK dep is not in the
default file; operators vendor it into their fork once.

## Posture cheat-sheet

```
default    →  no FIPS, no HSM, Docker sandbox
fips       →  FIPS, no HSM, Docker sandbox      (linux/amd64)
fips+pkcs11→  FIPS, HSM, Docker sandbox         (linux/amd64; the planned official FIPS artifact)
firecracker→  no FIPS, no HSM, microVM sandbox  (linux + KVM)
```

Operators wanting both FIPS and microVM verification today
ship two binaries side-by-side and select per command.
A combined `fips+pkcs11+firecracker` build is supported but
not part of CI's official artifact set.

## See also

- [KEKRef schemes](kekref-schemes.md) — which scheme each
  build flavour activates.
- [Plugins → Tier-1 vs Tier-2](plugins/tier1-vs-tier2.md) —
  why optional-deps live behind build tags.
- [How-to → Packaging the FIPS variant](../how-to/packaging/fips-variant.md) —
  building, signing, and shipping the FIPS binary.
- [Compliance → SLSA L3 provenance](../compliance/slsa-l3-provenance.md) —
  the artifact-signing chain for each flavour.

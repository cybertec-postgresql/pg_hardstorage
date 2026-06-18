---
title: Packaging how-to guides
description: Build the binary, plus its FIPS / PKCS#11 /
              Firecracker variants and Debian / RPM packages.
---

# Packaging how-to guides

Build `pg_hardstorage` from source and produce installable
artefacts. The default build is pure Go (`CGO_ENABLED=0`)
and produces one static binary; build-tagged variants add
specific capabilities.

## Pages

- [Build from source](build-from-source.md) — the default
  binary, with version stamping.
- [Build .deb and .rpm packages](debian-rpm.md) —
  `dpkg-buildpackage` / `rpmbuild` with `lintian` /
  `rpmlint` as a hard gate.
- [Build the FIPS variant](fips-variant.md) —
  BoringCrypto, Linux/amd64 only, CGO required.
- [Build the PKCS#11 variant](pkcs11-variant.md) — HSM-
  backed KEK custody via `miekg/pkcs11`.
- [Build the Firecracker variant](firecracker-variant.md)
  — microVM verifier-sandbox backend.

## Build-tag matrix

| Variant       | Build tag      | CGO | Platform        | Make target            |
|---------------|----------------|-----|-----------------|------------------------|
| Default       | _none_         | off | any supported   | `make build`           |
| FIPS          | `fips`         | on  | linux/amd64     | `make build-fips`      |
| PKCS#11 / HSM | `pkcs11`       | on  | any supported   | `make build-pkcs11`    |
| Firecracker   | `firecracker`  | off | linux + KVM     | `make build-firecracker` |

The variants stack: a `build-fips-pkcs11` target combining
both is on the v0.5 roadmap.

## Distribution

For v0.1, `make build` plus the Debian / RPM skeletons
under `debian/` are the supported paths. Container images,
Homebrew tap, Scoop, cosign attestations, and per-PG-major
extension packages all land with v0.5 alongside the
control plane.

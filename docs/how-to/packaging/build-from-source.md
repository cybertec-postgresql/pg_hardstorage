---
title: Build from source
description: Compile the default `pg_hardstorage` and testkit
              binaries with proper version stamping.
tags:
  - packaging
  - build
  - source
---

# Build from source

> Produce a stamped, reproducible `pg_hardstorage` binary
> from a clean checkout. Pure Go, `CGO_ENABLED=0` by
> default, single static binary. No external build-time
> dependencies beyond the Go toolchain.

## What you need

- Go 1.26 or later (the `go.mod` toolchain pin).
- Git — used for version stamping; not required at runtime.
- `make` (every target is a thin wrapper over the Go
  toolchain; you can run the underlying `go build` directly
  if you prefer).

## Steps

### 1. Build the production binary

```bash
make build
```

This writes `bin/pg_hardstorage`. The Makefile equivalents:

```bash
mkdir -p bin
CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags '-s -w
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=$(VERSION)
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=$(COMMIT)
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=$(DATE)' \
    -o bin/pg_hardstorage \
    ./cmd/pg_hardstorage
```

`VERSION` defaults to `git describe --tags --always --dirty`
falling back to `dev`. `COMMIT` is the short SHA. `DATE` is
the build timestamp (UTC, RFC3339).

### 2. Build the testkit binary

```bash
make build-testkit
```

Writes `bin/pg_hardstorage_testkit`. Same flags; different
entry point under `cmd/pg_hardstorage_testkit`.

### 3. Build all binaries at once

```bash
make all-binaries
```

This runs `build`, `build-testkit`, and `build-simple` —
three binaries: `bin/pg_hardstorage`,
`bin/pg_hardstorage_testkit`, and `bin/pg_hardstorage_simple`.

### 4. Confirm the build is stamped

```bash
./bin/pg_hardstorage version
```

```console
pg_hardstorage v1.0.x (abcdef1, built 2026-05-04T08:13:42Z)
```

The output is a single line: `pg_hardstorage <version>
[FIPS] (<commit>, built <date>)`. The `[FIPS]` marker only
appears on the FIPS variant. For machine-readable output run
`version --output json`; the JSON carries `version`,
`commit`, `date`, `variant` (`"default"` or `"fips"`), and
`fips` fields.

A clean checkout that's not on a tagged commit produces
`v1.0.x-N-gabcdef1` — which is what you want, because that's
the unique identifier of the build that's running.

## What just happened

The Go compiler statically links every dependency. With
`CGO_ENABLED=0` the result is a position-independent
executable that runs on any glibc / musl Linux of the same
arch — no `libc` version match required. The
`-trimpath` flag strips workspace paths from the binary so
two builds of the same commit on different machines produce
byte-identical output (one input to reproducible builds).

`-ldflags -s -w` strips the symbol table and DWARF info,
trimming roughly 20% off the binary's size. The version
metadata is injected via `-X` linker variables read by
`internal/version`.

## Cross-compiling

```bash
GOOS=linux   GOARCH=arm64 make build
GOOS=darwin  GOARCH=arm64 make build
GOOS=windows GOARCH=amd64 make build       # CLI-only
```

Windows is CLI-only; the agent's signal handling and
systemd integration are Linux-first. macOS is supported as
an operator workstation target — `pg_hardstorage` runs as
a CLI, not as a long-running agent (no launchd plist
ships).

## Reproducible builds

The default flags (`-trimpath`, `-ldflags -s -w`) plus a
pinned `go.mod` toolchain produce reproducible artefacts.
Two builds of the same commit on the same arch with the
same `VERSION`/`COMMIT`/`DATE` produce identical bytes.
Override the date via the env var when you need bit-exact
reproducibility:

```bash
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) \
DATE=$(date -u -r $SOURCE_DATE_EPOCH +%Y-%m-%dT%H:%M:%SZ) \
make build
```

CI runs `reprotest` over the Debian artefacts; the goal is
the hand-rolled `make build` matches what the packaged
binary contains.

## Building the helper variants

These three flavours each get their own how-to:

- [FIPS variant](fips-variant.md) — `make build-fips`,
  BoringCrypto, CGO required, Linux/amd64 only.
- [PKCS#11 / HSM variant](pkcs11-variant.md) —
  `make build-pkcs11`, CGO required.
- [Firecracker variant](firecracker-variant.md) —
  `make build-firecracker`, microVM verify-sandbox backend.

The default `make build` is the right starting point for
every other deployment.

## Troubleshooting

### `go: errors downloading modules`

Module proxy unreachable (air-gapped network). Either
configure `GOPROXY` to your mirror, or vendor the
dependencies once on a connected host with `go mod vendor`
and ship the vendored tree.

### `version.Version` is empty in `pg_hardstorage version`

The `-X` linker flag didn't apply. Run via `make build`
rather than calling `go build` directly, or pass the flag
yourself:

```bash
go build -ldflags '-X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=v1.0.0-dev' ...
```

### Binary works on the build host but not on a target

The most common culprit is glibc on the build host vs
musl on the target. With `CGO_ENABLED=0` the binary should
be fully self-contained; verify with `file bin/pg_hardstorage`
that it doesn't show as dynamically linked.

## Next steps

- [Build the FIPS variant](fips-variant.md).
- [Build the PKCS#11 variant](pkcs11-variant.md).
- [Build the Firecracker variant](firecracker-variant.md).
- [Debian / RPM packaging](debian-rpm.md) — wrap the
  binary into a `.deb` / `.rpm`.

---
title: Build the Firecracker variant
description: Build a `pg_hardstorage-firecracker` binary that
              runs the verifier sandbox in a microVM.
tags:
  - packaging
  - firecracker
  - sandbox
  - verify
---

# Build the Firecracker variant

> Compile a `pg_hardstorage-firecracker` binary that
> activates the firecracker-go-sdk-backed sandbox backend.
> Pure Go (no CGO required); only the `firecracker` process
> the agent execs as a subprocess is C.  Linux + KVM only.

## What you need

- A Linux build host with Go 1.26 or later.
- One-time: vendor the firecracker-go-sdk into your fork's
  `go.mod`:

  ```bash
  go get github.com/firecracker-microvm/firecracker-go-sdk
  ```

  Same posture as the [PKCS#11 variant](pkcs11-variant.md):
  the SDK is gated behind a build tag so it isn't in the
  default `go.mod`.

- At runtime: a Linux host with `/dev/kvm` accessible by
  the agent's user, the `firecracker` binary on `$PATH`, a
  `vmlinux` kernel image, and a contract-honouring rootfs
  (see [Firecracker sandbox](../verify/firecracker-sandbox.md)).

## Steps

### 1. Vendor the SDK (one-time per fork)

```bash
go get github.com/firecracker-microvm/firecracker-go-sdk
```

### 2. Build the binary

```bash
make build-firecracker
```

Equivalent direct invocation:

```bash
go build -tags firecracker \
    -trimpath \
    -ldflags '-s -w
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=$(VERSION)
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=$(COMMIT)
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=$(DATE)' \
    -o bin/pg_hardstorage-firecracker \
    ./cmd/pg_hardstorage
```

`CGO_ENABLED=0` works for this variant — the SDK is pure
Go. The Makefile inherits the project default `CGO_ENABLED=0`.

### 3. Confirm the firecracker backend is compiled in

The `firecracker` build tag swaps the default stub backend
(`backend_firecracker_stub.go`, which refuses with a clear
"this build doesn't include the firecracker backend"
message) for the real implementation
(`backend_firecracker_real.go`). There is no `doctor` output
that enumerates sandbox backends; the confirmation is simply
that a firecracker-backed verify succeeds instead of
returning the stub's refusal.

### 4. Run a verify with the new backend

The sandbox backend and its Firecracker kernel / rootfs
paths are configured in `pg_hardstorage.yaml`, not via CLI
flags — there is no `--sandbox-backend`, `--firecracker-kernel`,
or `--firecracker-rootfs` flag. Run the verify as usual:

```bash
pg_hardstorage-firecracker verify db1 latest \
    --repo s3://acme-pg-backups \
    --full
```

See [Verify with the Firecracker sandbox](../verify/firecracker-sandbox.md)
for the rootfs contract and PGDATA-as-image details.

## What just happened

The `firecracker` build tag activates two things:

1. The Firecracker backend in
   `internal/verify/sandbox/backend_firecracker_real.go`
   (the default build ships a stub that refuses with a
   clear "this build doesn't include the firecracker
   backend" message).
2. `FirecrackerBuilt() = true` — the runtime predicate
   `doctor` and the audit-event stream surface, so
   operators can confirm the variant matches what they
   expect.

Both flavours share the always-built parser / validator
helpers in `firecracker_common.go`, so the unit-test surface
is exercised on every CI run regardless of the build tag.
That keeps the rootfs-contract parser honest without
needing KVM in CI.

## Why pick this variant

| Need | Default build | Firecracker build |
| --- | --- | --- |
| Verify backups | Docker sandbox | Docker + microVM sandbox |
| No Docker daemon allowed on the host | Won't work | Works (microVM doesn't need dockerd) |
| Stronger isolation for verify | testcontainers | Separate kernel, no shared kernel with agent |
| Portable across non-Linux operator workstations | Yes | No (Linux + KVM only) |

If both Docker and Firecracker are available on a host,
operators select the backend through the sandbox
configuration in `pg_hardstorage.yaml` (there is no
`--sandbox-backend` CLI flag).

## Distribution

The Firecracker variant ships through the same goreleaser
artefact channel as the FIPS / PKCS#11 variants — packaged
binaries land in v0.5+ alongside the operator integration
work. For v0.1 the path is "build it yourself with the
instructions on this page."

## Troubleshooting

### `unknown backend "firecracker"`

You're running the default build, not the firecracker
build. Check `pg_hardstorage doctor`'s sandbox backend
list, or rebuild with `make build-firecracker`.

### Build fails with `cannot find module providing package github.com/firecracker-microvm/firecracker-go-sdk`

You haven't vendored the SDK into your fork's `go.mod`.
Run `go get github.com/firecracker-microvm/firecracker-go-sdk`
once.

### Binary runs but `firecracker` exec fails

The runtime host needs the `firecracker` binary on `$PATH`,
not the build host. Install firecracker on the runtime
host:

```bash
# Debian / Ubuntu
sudo apt install firecracker

# Manual: download from the upstream release page
```

### Permission denied on /dev/kvm

The agent's user must be in the `kvm` group:

```bash
sudo usermod -aG kvm pgbackup
```

Restart the agent after the group change for it to take
effect.

## Next steps

- [Verify a backup with the Firecracker sandbox](../verify/firecracker-sandbox.md)
  — operate the backend.
- [Verify with the Docker sandbox](../verify/docker-sandbox.md)
  — the default that ships in every build.
- [Build from source](build-from-source.md) — the default
  build the firecracker variant deviates from.

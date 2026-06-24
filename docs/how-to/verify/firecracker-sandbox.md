---
title: Verify a backup with the Firecracker microVM sandbox
description: Run `pg_verifybackup` inside a Firecracker microVM —
              no shared kernel, no Docker daemon.
tags:
  - verify
  - sandbox
  - firecracker
  - airgapped
---

# Verify a backup with the Firecracker microVM sandbox

> Boot a stripped Linux kernel + an operator-supplied rootfs,
> attach the restored PGDATA as a read-only block image, and
> let `pg_verifybackup` run inside. Strongest isolation
> posture the verifier ships: no shared kernel with the
> agent, no Docker daemon to attack, no container escape
> surface.  Linux + KVM only.

## What you need

- The Firecracker-flavour binary
  (`pg_hardstorage-firecracker` from
  [Build the Firecracker variant](../packaging/firecracker-variant.md)).
- A Linux host with `/dev/kvm` accessible by the agent's
  user (group `kvm` on most distros).
- The `firecracker` binary on `$PATH` (or a path passed via
  `Options.FirecrackerBin`).
- A kernel image (typically `vmlinux`) and a rootfs `ext4`
  image that honour the magic-line contract below.
- The PGDATA directory pre-built into a block image, since
  Firecracker has no directory-bind primitive.

## Steps

### 1. Prepare a rootfs that honours the contract

The microVM boots, mounts the PGDATA disk read-only at
`/mnt/pgdata`, runs `pg_verifybackup`, and signals the
verdict on `/dev/console` with a frozen magic prefix:

```text
__PG_HARDSTORAGE_VERIFY__:OK
__PG_HARDSTORAGE_VERIFY__:FAIL <stderr>
__PG_HARDSTORAGE_VERIFY__:SKIPPED <reason>
```

A reference rootfs build script ships under
`scripts/firecracker-rootfs.sh`. Operators with custom
security baselines roll their own rootfs and keep the magic
prefix exact — the prefix is part of the v1.0 schema-
compatibility commitment.

The init script must halt after printing (`reboot -f` is
fine; the kernel cmdline carries `panic=1 reboot=k` so any
exit path halts the microVM).

### 2. Build the PGDATA block image

Firecracker attaches block devices, not directories.
Convert the restored PGDATA into an ext4 image once per
verify run:

```bash
mkfs.ext4 -d /var/restore/db1.full.20260427T093017Z \
          /tmp/pgdata.img \
          $((SIZE_GB * 1024))M
```

The Firecracker backend refuses with a clear remediation if
`DataDir` points at a directory rather than a block image.

### 3. Run the verify

```bash
pg_hardstorage-firecracker verify db1 latest \
    --repo s3://acme-pg-backups \
    --full
# The sandbox backend and the kernel / rootfs paths are configured in
# pg_hardstorage.yaml (see step 3), not via CLI flags.
```

(For agent-driven verify the same fields go in
`/etc/pg_hardstorage/pg_hardstorage.yaml` under the verify
schedule's sandbox config.)

### 4. Inspect the result

```console
✓ verify --full passed (pg_verifybackup, firecracker microVM)
  Deployment:  db1
  Backup:      db1.full.20260427T093017Z
  Duration:    91204 ms
```

The `Result.Backend` field is `firecracker`; the rest of the
schema is identical to the Docker backend
(`pg_hardstorage.verify.sandbox.v1`).

## What just happened

The agent forked `firecracker`, gave it a unix-domain
socket, attached the kernel image, attached the rootfs
read-only, attached `pgdata.img` read-only as a second
drive, and captured the serial console. The init script
inside the rootfs ran `pg_verifybackup /mnt/pgdata`, printed
the magic line, and halted. The agent parsed the magic
verdict and emitted the `Result`.

A 30-minute wallclock cap covers the case where the rootfs
hangs; the agent kills the microVM and surfaces a structured
error. v0.1 ships this cap as a constant; v1.0 will expose
it through `Options`.

## Why pick this over Docker

- **No shared kernel.** A bug in `pg_verifybackup` cannot
  pivot to host kernel exploits; the microVM kernel is
  separate from the agent's.
- **No Docker daemon.** Eliminates a privileged daemon as
  attack surface. Suitable for hosts that aren't allowed to
  run dockerd at all.
- **Reproducible boot.** Each run starts from a fresh
  microVM with a known-good rootfs; no image-pull, no layer
  cache to trust.
- **Air-gap friendly.** The kernel + rootfs sit on local
  disk; nothing in the verify step needs network egress
  (matches the [air-gap policy](../air-gapped/enable-policy.md)
  posture).

## Troubleshooting

### `sandbox/firecracker: FirecrackerKernel is required`

The `Options.FirecrackerKernel` and
`Options.FirecrackerRootfs` paths are mandatory for the
Firecracker backend. The pre-flight gate refuses early
rather than letting the microVM panic 200 ms in.

### `DataDir is a directory; the firecracker backend needs a block image`

Build a block image first:

```bash
mkfs.ext4 -d <pgdata-dir> pgdata.img <size>
```

Auto-image-creation is a v1.5 polish item. For v1.0 the
agent (or a `pg_hardstorage verify prepare-image` helper) is
expected to produce the image before calling Verify.

### `rootfs did not emit __PG_HARDSTORAGE_VERIFY__ line`

The rootfs's init script never reached the magic-line
print. Causes:

- Init crashed before running `pg_verifybackup`. Boot the
  rootfs interactively (drop `panic=1 reboot=k` and add a
  shell on `tty1`) to see what's happening.
- Init printed the line on stdout instead of `/dev/console`.
  Firecracker's serial passthrough is `/dev/console`; ensure
  the script writes there.
- pg_verifybackup is not on `PATH`. The reference rootfs
  installs it at `/usr/bin/pg_verifybackup`.

### KVM not available

The agent's user must be in the `kvm` group, and `/dev/kvm`
must exist. On bare metal that means the host firmware
exposes virtualisation extensions and the kernel was built
with KVM. On nested-virt cloud VMs check the provider's
documentation; Firecracker requires hardware-virt at the
hypervisor level.

## Rootfs contract (frozen for 24 months)

| Field | Constraint |
| --- | --- |
| Mount of `/dev/vdb` | Read-only at `/mnt/pgdata`. |
| Verifier invocation | `pg_verifybackup /mnt/pgdata`. |
| Magic prefix | `__PG_HARDSTORAGE_VERIFY__:` on `/dev/console`. |
| Verdicts | `OK`, `PASS`, `FAIL <detail>`, `SKIPPED <reason>`. |
| Halt | Any exit path that the kernel's `panic=1 reboot=k` will catch. |

Ship the contract once, reuse the rootfs across runs.

## Next steps

- [Docker sandbox](docker-sandbox.md) — the default backend.
- [Build the Firecracker variant](../packaging/firecracker-variant.md)
  — what `make build-firecracker` produces.
- [Air-gap policy](../air-gapped/enable-policy.md) — the
  posture this backend complements.
- [`verify` CLI reference](../../reference/cli/pg_hardstorage_verify.md).

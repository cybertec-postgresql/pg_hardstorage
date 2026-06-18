---
title: Verifier sandbox how-to guides
description: Recipe pages for the two sandbox backends —
              Docker (default) and Firecracker (microVM).
---

# Verifier sandbox how-to guides

`pg_hardstorage verify --full` runs `pg_verifybackup` against
a freshly-restored copy of a backup inside a disposable
sandbox. Two backends ship; both produce the same
schema-stable `Result` shape so callers stay backend-
agnostic.

## Pages

- [Use the Docker sandbox](docker-sandbox.md) — default
  backend, always built. `postgres:<major>` container via
  testcontainers-go, requires a Docker socket on the agent
  host.
- [Use the Firecracker microVM sandbox](firecracker-sandbox.md)
  — strongest isolation. Build-tagged
  (`-tags firecracker`); Linux + KVM only. No shared kernel
  with the agent, no Docker daemon.

## Picking a backend

| Constraint                           | Pick |
|--------------------------------------|------|
| Default; one host, Docker available  | [Docker sandbox](docker-sandbox.md) |
| No Docker daemon allowed on the host | [Firecracker sandbox](firecracker-sandbox.md) |
| Strongest isolation posture          | [Firecracker sandbox](firecracker-sandbox.md) |
| macOS / non-Linux operator workstation | [Docker sandbox](docker-sandbox.md) |
| Air-gapped, no host-side dockerd     | [Firecracker sandbox](firecracker-sandbox.md) |

For non-Linux workstations only the Docker backend is
available; Firecracker is Linux-KVM-only.

## Related

- [Build the Firecracker variant](../packaging/firecracker-variant.md)
  — what `make build-firecracker` produces.
- [Verifier subsystem (architecture tour)](../../explanation/architecture-tour.md)
  — design context for fast vs full verify.
- [`verify` CLI reference](../../reference/cli/pg_hardstorage_verify.md)
  — every flag.

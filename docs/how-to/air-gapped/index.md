---
title: Air-gapped operation how-to guides
description: Recipe pages for running pg_hardstorage in a
              network with no path to the public internet.
---

# Air-gapped operation how-to guides

`pg_hardstorage` is designed to run end-to-end inside an
air-gapped network: regulated finance, classified, strict
data-residency. The binary itself never phones home — no
telemetry, no auto-update checks, no Rekor lookups by
default. The pages below cover the two pieces that matter
in this posture: locking the policy gate and moving
backups across the air gap.

## Pages

- [Enable air-gap policy](enable-policy.md) — flip the
  binary into strict mode and configure the endpoint
  allowlist.
- [Export a repo bundle](repo-bundle-export.md) — bundle
  manifests + chunks + optional WAL into one tar for
  transport.
- [Import a repo bundle](transport-bundle-import.md) —
  receive a bundle on the destination side; idempotent.

## Verifying inside an air gap

The Firecracker verifier sandbox is the natural fit for
air-gapped operation: kernel + rootfs sit on local disk,
no Docker daemon, no outbound calls during verify. See
[Verify with the Firecracker sandbox](../verify/firecracker-sandbox.md).

## Architecture context

The full architectural rationale (resolution precedence,
allowed schemes, why DNS is deliberately not consulted)
lives on [Enable air-gap policy](enable-policy.md). For
the design "why" behind the policy gate's posture, see
the architecture tour.

---
title: Reference
description: Exhaustive technical specs — auto-generated where possible.
---

# Reference

Reference pages are **exhaustive and machine-comparable**.
Half are auto-generated from the source of truth (Cobra
command tree, OpenAPI spec, .proto files); the rest are
hand-maintained schemas.

The auto-generated pages land via `make docs-regen`; CI
fails on drift between the source of truth and the
committed page.

---

## CLI

- [CLI reference](cli/index.md) — one page per
  `pg_hardstorage` subcommand, auto-generated from the
  Cobra command tree.  216 pages today.

## REST API

- [REST API overview](api/index.md) — the v1 contract,
  auth, conventions, exit codes.

## Plugins

How to write a plugin against the pg_hardstorage interfaces.

- [Overview](plugins/index.md) — Tier-1 vs Tier-2, the seven
  plugin types, registration patterns, trust posture.
- [Storage contract](plugins/storage-contract.md)
- [Source contract](plugins/source-contract.md)
- [Encryption (KMS) contract](plugins/encryption-contract.md)
- [Compression contract](plugins/compression-contract.md)
- [Renderer contract](plugins/renderer-contract.md)
- [Sink contract](plugins/sink-contract.md)
- [LLM provider contract](plugins/llm-provider-contract.md)
- [Tier-2 protocol (`go-plugin`)](plugins/tier2-go-plugin-protocol.md)
- [Tier-1 vs Tier-2 decision matrix](plugins/tier1-vs-tier2.md)

## Schemas and catalogues

Hand-written for v1.0; will be auto-generated as the
reflectors land (see [`DOC_PLAN.md`](../DOC_PLAN.md)
"Auto-generation map").

- [Exit codes](exit-codes.md) — the 0-10 contract and the
  namespace → exit-code mapping.
- [Error codes](error-codes.md) — catalogue of structured
  error codes, grouped by domain.
- [KEKRef schemes](kekref-schemes.md) — `local:`,
  `aws-kms://`, `gcp-kms://`, `azure-kv://`,
  `vault-transit://`, `pkcs11://`.
- [Storage URL schemes](storage-url-schemes.md) —
  `file://`, `s3://`, `azblob://`, `gcs://`, `sftp://`,
  `scp://`.
- [Build flavours](build-flavours.md) — default, FIPS,
  PKCS#11, Firecracker.
- [Manifest schema](manifest-schema.md) — on-disk backup
  manifest fields and canonicalisation rule.
- [Output event schema](output-event-schema.md) — `Event`,
  `Result`, and `Error` wire format.
- [Skill schema](skill-schema.md) — LLM skill YAML format
  and tool allowlist.
- [Metric catalogue](metric-catalogue.md) — Prometheus
  metric names; the catalogue is largely **live** (~20 metric
  families registered), only the Reserved families are still
  outstanding (see SPEC drift).

## Runbooks

[Runbooks](runbooks/index.md) for the seven named
disaster scenarios + control-plane setup.

---

## Coming in later doc passes

These reference pages will land as the auto-generators wire
up (the [DOC_PLAN.md](../DOC_PLAN.md) "Auto-generation map"
tracks what's outstanding):

- gRPC reference (auto-generated from `proto/**/*.proto`
  via `protoc-gen-doc`)
- Configuration schema (auto-generated from
  `internal/config.Schema()` reflection)
- Audit-event schema (struct-reflection emitter)
- Kubernetes CRDs (auto-generated from `api/crd/`)
- Filesystem layout, compatibility matrix

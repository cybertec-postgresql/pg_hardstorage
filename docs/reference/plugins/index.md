---
title: Plugin reference
description: Authoring contracts for the seven pg_hardstorage plugin tiers — Storage, Source, Encryption, Compression, Renderer, Sink, LLMProvider.
tags:
  - plugins
  - reference
---

# Plugin reference

`pg_hardstorage` is a small core wrapped in seven plugin
contracts.  This section is the **authoring reference**:
each page below is the formal interface, the per-method
contract, the error sentinels, and a pointer to a
reference implementation already in-tree.

| Tier | Concern | Interface | Reference impl |
| --- | --- | --- | --- |
| **Storage** | Object store the repo lives on (FS, S3, Azure, GCS, SFTP) | `internal/plugin/storage.StoragePlugin` | `internal/plugin/storage/{fs,s3}` |
| **Source** | What pg_hardstorage backs *up from* (streaming, pgincr17, snapshot) | `SourcePlugin` (forward-looking, see below) | not yet in-tree |
| **Encryption** (KEK) | KEK custody — wraps and unwraps the per-backup DEK | `internal/kms.Provider` | `internal/plugin/kms/{awskms,vaulttransit}` |
| **Compression** | Per-chunk codec (zstd today, lz4 / brotli / etc. plug-points) | `internal/plugin/compression.Compressor` | `internal/plugin/compression/zstd` |
| **Renderer** | Synchronous CLI output (text, json, ndjson, junit, …) | `internal/output.Renderer` | `internal/plugin/renderer/{text,ndjson}` |
| **Sink** | Asynchronous fan-out to external systems (Slack, Jira, syslog, OTel) | `internal/output.Sink` | `internal/plugin/sink/slack` |
| **LLMProvider** | Chat completion backend for the assistant (OpenAI-compatible) | `internal/plugin/llmprovider.Provider` | `internal/plugin/llmprovider/openai.go` |

Per-tier contract pages:

- [Storage contract](storage-contract.md)
- [Source contract](source-contract.md)
- [Encryption contract (kms.Provider)](encryption-contract.md)
- [Compression contract](compression-contract.md)
- [Renderer contract](renderer-contract.md)
- [Sink contract](sink-contract.md)
- [LLM provider contract](llm-provider-contract.md)

Cross-cutting:

- [Tier-2 plugin protocol](tier2-go-plugin-protocol.md)
- [Tier-1 vs Tier-2: choosing a plugin tier](tier1-vs-tier2.md)

---

## Tier 1 vs Tier 2 in one paragraph

**Tier 1** plugins are Go packages compiled into the
`pg_hardstorage` binary.  They register themselves at
`init()` time against a per-tier `DefaultRegistry` and are
selected by name (sink plugin), URL scheme (storage,
KEKRef), or `AlgorithmID` (compression, encryption).  A
single signed binary; one supply chain; FIPS-buildable in
one shot.

**Tier 2** plugins are *separate executables* discovered on
`$HSPLUGIN_PATH` and invoked via stdio JSON-RPC (`v1`) —
the protocol contract lives in
`internal/plugin/external/protocol.go` and the gRPC-shaped
contract that v1.1 will move to lives in
`proto/plugin/v1/plugin.proto`.  Crash-isolated,
language-agnostic, but a separate trust decision per
binary.

See [Tier-1 vs Tier-2](tier1-vs-tier2.md) for the
selection matrix.

## Registration: how plugins get into the binary

Tier-1 plugins use one of two patterns depending on the
tier; both are init-side-effect imports from `cmd/`:

**1. Self-registering init().**  Storage, Compression,
Encryption, Sink, LLMProvider, KMS providers all expose
their concrete package's `init()` function which calls
`Register(...)` on the per-tier default registry:

```go
// internal/plugin/sink/slack/slack.go
func init() {
    output.DefaultSinkRegistry.Register("slack", NewFromSpec)
}
```

The `cmd/pg_hardstorage/main.go` (or a build-flavour-
specific `cmd/pg_hardstorage_fips/main.go`) imports the
concrete plugin packages with `_ "…/internal/plugin/sink/slack"`
to trigger that side-effect.  Drop the import; lose the
plugin.

**2. Constructor-call wiring.**  Renderers don't
self-register; the dispatcher's constructor takes one
explicit `Renderer` argument and the CLI's `--output`
resolution chooses which one to construct.  Same effect,
different idiom — used where exactly one impl is active
per process invocation.

Tier-2 plugins are **discovered**, not registered.  At
startup the binary walks every directory in
`$HSPLUGIN_PATH`, invokes each `pg-hardstorage-plugin-*`
executable with `--probe`, and collects the `ProbeResponse`
into an in-memory `external.Registry`.  See
[Tier-2 plugin protocol](tier2-go-plugin-protocol.md).

## Trust posture

| Posture | Tier-1 | Tier-2 |
| --- | --- | --- |
| Supply chain | One signed `pg_hardstorage` binary | Each plugin is a separate binary the operator must trust |
| FIPS build | Inherited from the host binary | Plugin must declare its own FIPS posture; mixed-mode is refused under `--fips-strict` |
| Crash blast radius | Plugin panic = process exit (we don't recover) | Plugin panic = subprocess exit; host marks plugin failed and surfaces a `plugin.crashed` event |
| Discovery | Compile-time (`_ "…/internal/plugin/x/y"` in `cmd/`) | Runtime (`$HSPLUGIN_PATH` walk + `--probe`) |
| Versioning | Locked to `pg_hardstorage` SemVer | Plugin declares its own SemVer + protocol version; mismatched protocol = refusal at handshake |
| Auditing | Linked binary set fixed at build | `pg_hardstorage doctor` lists every loaded plugin with name, version, path, signature |

Operators in regulated environments typically pin to Tier-1
exclusively; the SPEC's compliance posture (`--fips-strict`,
SLSA L3 build attestation) covers only the Tier-1 subset.
Tier-2 is the integration story for vendors and customers
shipping bespoke logic against the public protocol.

## Cross-references

- The corresponding **how-to** lives at
  `tutorials/build-a-storage-plugin.md` (a worked example)
  — once Phase 6's tutorial slice lands.
- The CLI verbs that surface plugin state:
  `pg_hardstorage doctor`, `pg_hardstorage plugin list`,
  `pg_hardstorage repo capabilities`.
- The audit-event types the host emits about plugin
  lifecycle: `plugin.discovered`, `plugin.handshake.ok`,
  `plugin.handshake.refused`, `plugin.crashed`,
  `plugin.unloaded` — see the audit-event-schema reference.

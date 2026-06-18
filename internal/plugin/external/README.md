# plugin/external/

The Tier-2 out-of-process plugin host — **v1.0 target; currently scaffolded;
the `hashicorp/go-plugin` host is not wired**.

## What lives here

The contract that will let third-party plugins ship as separate binaries, signed
independently, communicating over gRPC with `pg_hardstorage` as the host
process. Today this directory holds the wire protocol stubs only — no host
runtime, no handshake, no plugin discovery. The Tier-1 in-tree plugins remain
the only supported extension mechanism until v1.0.

## Why Tier-2 is deliberately deferred

- Tier-1 covers every plugin we ship.
- Out-of-process plugins enlarge the trust boundary (signing, sandboxing,
  capability scoping) — design needs to land before code.
- FIPS posture: a separately built plugin must declare its FIPS mode and be
  refused if it doesn't match the host.

## Key files

- `protocol.go` — gRPC service definitions and version stamps that the
  eventual host will negotiate against
- `protocol_test.go` — schema stability tests so the wire shape doesn't drift
  before v1.0

## v1.0 plan (tracking)

1. `hashicorp/go-plugin` host with magic-cookie handshake.
2. Per-tier gRPC service per Tier-1 interface (storage, sink, renderer first).
3. Plugin manifest: `pg_hardstorage_plugin.json` declaring name, tier,
   signature, capabilities, FIPS posture.
4. Plugin discovery directory: `<paths.Lib>/plugins/<tier>/<name>/`.
5. Capability scoping per `internal/approval` so out-of-process code can't
   sidestep destructive-action gates.

## Read next

- `../README.md` — the eight tiers and how Tier-1 plugins are added today
- `docs/explanation/plugin-architecture.md` — design rationale
- Tracking issue in the repo's milestones board

## Don't put X here

- Tier-1 plugins — they go under their tier's subdir, statically linked.
- Plugin binaries themselves — this is the host scaffolding only.
- Anything that lets a plugin bypass the dispatcher / approval / audit layers.

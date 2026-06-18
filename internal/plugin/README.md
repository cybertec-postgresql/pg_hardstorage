# plugin/

The plugin index: eight tiers that every extension point in `pg_hardstorage`
slots into.

## What lives here

The directory tree that organises every pluggable surface. Each subdir is a
tier: a Go interface, a factory registry, and N implementations. All eight tiers
ship as **Tier-1** today — in-tree, statically linked, FIPS-buildable, audited
together. **Tier-2** out-of-process plugins via `hashicorp/go-plugin` are
scaffolded under `external/` for v1.0.

## The eight tiers

| Tier | Purpose | Plugins shipped |
| --- | --- | --- |
| `storage/` | Object/file backends for chunks and manifests | 9 |
| `encryption/` | Per-chunk symmetric encryption | 1 (`aesgcm`) |
| `kms/` | KEK custody / DEK wrap-unwrap | 6 |
| `sink/` | Outbound event delivery | 15 |
| `renderer/` | Event → bytes for stdout, files, HTTP | 11 |
| `compression/` | Pre-encryption codec | 2 |
| `llmprovider/` | LLM chat backends | 2 |
| `external/` | Tier-2 out-of-process host (v1.0, currently scaffolded) | 0 |

## Adding a new plugin

1. Drop a Go package under the right tier subdir (e.g. `storage/foo/`).
2. Implement the tier's interface (see `<tier>/<tier>.go` or
   `<tier>/contract/`).
3. Register a factory in the tier's `registry.go` (or `init()` for the simpler
   tiers).
4. Add a config block under `share/pg_hardstorage.sample.yaml`.
5. Cover with unit tests and at least one integration test (`//go:build
   integration`).

## Read next

- `../config/README.md` — how plugins are selected from config
- Each `<tier>/README.md` — full plugin table for the tier
- `docs/explanation/plugin-architecture.md` — design rationale

## Don't put X here

- Plugins that mutate cluster state outside their tier interface — wrap a
  workflow instead.
- Cross-tier helpers — every tier is independent; share through `internal/`
  libs, not sideways.

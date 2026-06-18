# proto

Protobuf source of truth for every wire contract pg_hardstorage exposes — the
control-plane gRPC surface and the external-plugin protocol.

## What lives here

Hand-written `.proto` files versioned by package (`v1`, future `v2`). Generated
Go stubs land under `../internal/` and are committed to the repo so consumers
never need `protoc` installed. Breaking changes require a new version directory;
never mutate a frozen `v1` message.

## Key files / subdirs

- `pg_hardstorage/v1/common.proto` — shared scalar / enum types used by both
  services
- `pg_hardstorage/v1/services.proto` — control-plane RPCs (agents, jobs,
  backup, restore, verify)
- `plugin/v1/plugin.proto` — host ↔ external-plugin handshake, capability
  negotiation, RPC envelope

## Read next

- `../internal/plugin/external/protocol.go` — Go side of the plugin transport
  that wraps `plugin/v1`
- `../internal/server/routes.go` — REST handlers that mirror most of the
  `services.proto` RPCs
- `../api/openapi.yaml` — the REST projection of the same surface for non-gRPC
  clients

## Don't put X here

- Generated `*.pb.go` files — they live next to their consumers under
  `../internal/`.
- Server-side business logic — `.proto` is contract only.
- Experimental / unstable messages — promote to `v1` only when the shape is
  frozen.

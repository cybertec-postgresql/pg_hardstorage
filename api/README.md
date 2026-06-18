# api

External-facing API contracts in their canonical, hand-maintained form. Today:
the OpenAPI 3.1 spec for the REST control plane. Future: Kubernetes CRDs (v0.5
roadmap, not yet built).

## What lives here

The OpenAPI document is the spec, not a generated artefact — it is
hand-maintained and CI-asserted to match the Go handlers in
`../internal/server/routes.go`. Drift between this spec and the handlers is
treated as a release-blocking bug; see `../docs/SPEC_DRIFT.md`.

## Key files / subdirs

- `openapi.yaml` — OpenAPI 3.1 spec for the v1 REST API; mirrors
  `proto/pg_hardstorage/v1/services.proto`

## Read next

- `../internal/server/routes.go` — the handlers this spec describes (must stay
  in sync)
- `../proto/pg_hardstorage/v1/services.proto` — gRPC projection of the same
  surface
- `../docs/reference/api/` — the rendered, user-facing API docs derived from
  this spec
- `../docs/SPEC_DRIFT.md` — the policy that governs spec ↔ implementation
  drift

## Don't put X here

- Kubernetes CRDs — slated for the v0.5 roadmap; they will land here as YAML
  manifests once the controller story is decided.
- Generated SDKs — consumers can generate from `openapi.yaml`; we don't ship
  them.
- Internal RPC shapes — those belong in `../proto/` or stay package-private
  under `../internal/`.

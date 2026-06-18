# charts

Helm charts for the two Kubernetes deployment shapes pg_hardstorage supports.
Each chart is self-contained and versioned independently of the binary.

## What lives here

Standard Helm v3 chart layouts (`Chart.yaml`, `values.yaml`, `templates/`).
Charts pin to a published container image — they do not build from source.
Per-chart `README.md` files document values and upgrade notes; this top-level
README only routes between them.

## Key files / subdirs

- `pg-hardstorage-server/` — central control-plane deployment (REST API, jobs,
  sweeper); read `pg-hardstorage-server/README.md`
- `pg-hardstorage-sidecar/` — per-cluster sidecar pattern (one agent next to
  each PostgreSQL pod); read `pg-hardstorage-sidecar/README.md`

## Read next

- `../docs/how-to/kubernetes/` — operator-facing install / upgrade / DR
  walkthroughs
- `../test/k8s/README.md` — the CNPG-backed end-to-end test that exercises
  both charts
- `../dockerfiles/k8s/` — the operator-shim images these charts pull when
  running under CNPG, Crunchy, or Spilo

## Don't put X here

- CRD definitions — those will live under `../api/` once the v0.5 roadmap
  lands.
- Operator code (controllers, reconcilers) — out of scope; pg_hardstorage is
  shipped as a controller-less workload.
- Non-Helm Kubernetes manifests — use `../test/k8s/` for fixtures and
  `../docs/how-to/kubernetes/` for samples.

# test

End-to-end test corpus — scenario YAMLs, load profiles, fleet topologies, and
Kubernetes fixtures consumed by the testkit. Go unit and integration tests live
next to the code they cover under `../internal/`; this tree holds the
data-driven scenarios that exercise the system as a whole.

## What lives here

Declarative YAML inputs to the scenario runner (`../internal/testkit/runner/`)
and the load generator (`../internal/testkit/load/`). Scenarios are tiered
L1–L8 by cost and surface area — pick the lowest tier that exercises the bug
you're catching. Fixtures and topology files are shared across scenarios;
one-off data belongs inline in the scenario.

## Key files / subdirs

- `scenarios/` — the L1–L8 test corpus; one `.scenario.yaml` per case, named
  `L<tier>_<area>_<verb>.scenario.yaml`
  - L1 — smoke (CLI plumbing, LLM plumbing, misc plumbing)
  - L2 — feature-level happy-path (approval, audit chain, backup compare,
    compat shims, db extension, deployment lifecycle, DSA, gameday, hold,
    incremental, insider, integrity, JIT, ...)
  - L3 — multi-step / midchain / retention / tablespace remap
  - L4 — stress / disk pressure / toxiproxy / multi-PG-major
  - L5–L8 — longer-horizon and adversarial tiers
- `load/` — load profiles consumed alongside scenarios
  - `oltp_smoke.load.yaml` — light OLTP workload
  - `failover_loop.load.yaml` — failover-storm profile
- `fleets/multi-sink.fleet.yaml` — fleet topology fixture (multi-cluster
  scenarios)
- `soak/faults_new_primitives_2h.yaml` — 2-hour fault-injection soak profile
- `k8s/` — Kubernetes scenario fixtures (`cnpg-cluster.yaml`,
  `cnpg-verify.sh`); see `k8s/README.md`
- `llm-scenarios/` — LLM-evaluation scenarios with their own runner contract
  (`*.llm-test.yaml`) and the latest comprehensive report
- `fixtures/operator-argv/` — captured operator argv for the CNPG / Crunchy /
  Spilo shim regression tests

## Read next

- `../internal/testkit/runner/` — runner that consumes `scenarios/` and
  `load/`
- `../internal/testkit/scenario/` — scenario schema
- `../internal/testkit/load/` — load-profile schema
- `../dockerfiles/testbed/` — distro-matrix images scheduled by these
  scenarios

## Don't put X here

- Go unit / integration tests — they live next to the code under
  `../internal/<pkg>/*_test.go`.
- Production manifests — `../charts/` and `../deploy/`.
- One-shot debug scratch — keep it out of the repo.

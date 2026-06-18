# Kubernetes drop-in quality-assurance run, locally

This page is for the operator who wants to check whether
pg_hardstorage works as a drop-in replacement for the backup
tool of an existing PostgreSQL operator (CloudNativePG,
Crunchy PGO, or Zalando postgres-operator) on their own
machine, end-to-end, in under ten minutes.

The script you'll run is `./run_k8s_testing.sh` from the repo
root.  It builds the shim images, brings up a disposable
minikube profile, installs the operator, creates a cluster
that uses the shim image, drives WAL archiving through the
operator's own backup machinery, and asserts that genuine
pg_hardstorage objects (`HSREPO`, `chunks/sha256/...`,
WAL manifests) land in the in-cluster S3 sink.

## Prereqs

| tool | minimum | install |
|---|---|---|
| `docker` | engine able to run `minikube --driver=docker` | https://docs.docker.com/engine/install/ |
| `minikube` | v1.30+ | https://minikube.sigs.k8s.io/docs/start/ |
| `kubectl` | v1.28+ | https://kubernetes.io/docs/tasks/tools/ |
| `helm` | v3.14+ | https://helm.sh/docs/intro/install/ |
| host RAM | ≥ 8 GiB free per concurrent cell | — |

You can verify everything is in place with:

```sh
./bin/pg_hardstorage_testkit k8s prereqs
```

The testkit binary itself comes from `make build-testkit`,
which the driver runs automatically the first time.

## What's tested today (and what isn't)

| operator | drop-in works | scenarios |
|---|---|---|
| **Zalando** (Spilo + WAL-G) | ✅ | `operator_backup_smoke`, `operator_failover_no_gap` |
| **CloudNativePG** (barman-cloud-*) | ✅ | `operator_backup_smoke` |
| **Crunchy PGO** (pgbackrest TLS-server-mode) | ❌ blocked on protocol shim | — |

The Crunchy block is architectural, not a bug — PGO v5+ uses
pgbackrest's TLS RPC server (not the standalone CLI), and our
shim doesn't yet implement that protocol.  Crunchy operators
who want pg_hardstorage today should deploy the
`charts/pg-hardstorage-sidecar` Helm chart alongside their
PostgresCluster instead of attempting the binary-overlay drop-in.

CNPG note: this works against the 1.29.x series via the
in-image `barman-cloud-*` family.  CNPG 1.30+ deprecates
that path in favour of a Plugin sidecar; pg_hardstorage's
plugin implementation is a separate workstream.  The
scenarios pin a 1.29.x chart range.

## The single-cell run

```sh
export PATH="$HOME/bin:$PATH"   # if you installed minikube/helm to ~/bin
./run_k8s_testing.sh --operator zalando
```

To run all working operators × scenarios:

```sh
./run_k8s_testing.sh --operator cnpg,zalando \
                     --scenario operator_backup_smoke,operator_failover_no_gap
```

The `operator_failover_no_gap` scenario kills the primary
pod, waits for Patroni to promote the replica, then asserts
the WAL archive remained continuous (no gap, no orphan
`.partial` files).  Runtime ~10–15 min per cell because it
includes a real failover wait.

What happens:

1. **build** — `make build` + `make build-compat` produce the
   agent + shim binaries; `docker build` produces
   `pg-hardstorage/zalando-shim:pg17-<binhash>`.
2. **prereqs** — verifies `minikube`, `kubectl`, `helm`,
   `docker` are on PATH.
3. **cluster up** — `minikube start --profile
   pgvalidate-k8s-zalando-pg17-backup-smoke`, ~60 s.
4. **scenario** —
   `test/scenarios/k8s/zalando_operator_backup_smoke.sh`:
   installs Zalando postgres-operator (helm), applies a
   namespace + MinIO + bucket-bootstrap Job + a
   `postgresql.acid.zalan.do/v1` cluster CRD using the shim
   image, waits for `Running`, drives a WAL switch,
   asserts `pg_stat_archiver.archived_count > 0` and
   `HSREPO` + `chunks/sha256/*` objects in MinIO.
5. **teardown** — `minikube delete -p ...`, unless
   `--keep-on-failure` and the cell failed.

Total wall-clock: ~5–10 min on a 4-core / 8 GiB host.

Output:

```
✓ summary at test-runs/k8s-<ts>/summary.md
  1 pass / 0 fail / 0 skip / 0 dry-run (of 1)
```

The per-cell artefacts under
`test-runs/k8s-<ts>/zalando-pg17-operator_backup_smoke/`
include `run.log`, `pg_stat_archiver.txt`, and a full MinIO
listing.

## Useful flags

| flag | use |
|---|---|
| `--no-build` | reuse the binaries + shim image in `bin/` from a prior run; skips ~30 s |
| `--no-up` | assume a profile is already up and the operator already installed; useful for iterating on a scenario |
| `--keep-on-failure` | preserve cluster on failure for `kubectl describe` / log forensics |
| `--dry-run` | print what would happen, no actual cluster work — useful to validate flag plumbing without burning resources |
| `--parallel N` | run N cells concurrently, each in its own profile (needs ~8 GiB RAM per slot) |
| `--seed N` | shuffle cell order; combine with `--parallel` so a small N doesn't always pick the same heavy cells first |
| `--kube-driver <name>` | minikube backend — `docker` (default), `kvm2`, `hyperkit`, `virtualbox`, `podman` |
| `--keep-on-failure` plus `kubectl --context pgvalidate-k8s-...` | post-mortem against the live cluster |

## Troubleshooting

### "missing prerequisites"

The driver detected one of `minikube`, `kubectl`, `helm`,
`docker` not on PATH.  Install per the table above; the
driver re-checks after install.

### "minikube up failed: kubelet is not healthy"

Most often the kubelet's cgroup driver doesn't match the
docker daemon's.  The driver passes
`--extra-config=kubelet.cgroup-driver=systemd` already; if
you run minikube manually, do the same.

### Scenario FAILs with "no HSREPO file in MinIO"

Check the per-cell `run.log` and `minio-listing.txt`.  Real
causes we've seen:

- Pod still booting when the assertion ran (auto-init had
  to retry, the assertion ran during the gap).  Reproduce
  with `--keep-on-failure`, manually inspect the pod's
  `pg_stat_archiver` + `mc ls -r m/spilo-backups/`.
- The MinIO base image has no `grep`.  This is fixed in
  the scenario script; if you wrote a new scenario, pipe
  `mc ls` output to host-side grep, NOT to a grep inside
  the MinIO container.

### "docker driver is not supported on linux/amd64"

Older minikube took `--driver minikube` literally; modern
minikube wants `--driver docker`.  The driver's default is
already `docker`; if you forced `--kube-driver minikube`,
drop the flag.

## Adding a new scenario

A scenario is a single bash script under
`test/scenarios/k8s/<operator>_<scenario_name>.sh` that
gets these env vars from the driver:

| env | what |
|---|---|
| `KUBECTX` | minikube profile / kube context for this cell |
| `SHIM_IMAGE` | local docker image tag for the shim |
| `CELL_DIR` | per-cell artefact dir (write `result.json`, logs here) |
| `TESTKIT_BIN` | path to `pg_hardstorage_testkit` binary |

The script's responsibility is to install the operator,
create a cluster using `$SHIM_IMAGE`, drive whatever
exercise the scenario tests, and `exit 0` on PASS or
non-zero on FAIL.  The driver handles cluster bring-up,
teardown, retries, and report aggregation.

Reference implementation:
[`test/scenarios/k8s/zalando_operator_backup_smoke.sh`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/test/scenarios/k8s/zalando_operator_backup_smoke.sh).

## Reference: the captured operator argv

When we built this infra we ran each operator with the
`argv-recorder` shim in place of the real backup tool, then
captured the argv as a fixture.  Those fixtures live at:

- `test/fixtures/operator-argv/cnpg/argv-fixtures.ndjson` (5 unique invocations)
- `test/fixtures/operator-argv/zalando/argv-fixtures.ndjson` (10 unique invocations)
- `test/fixtures/operator-argv/crunchy/argv-fixtures.ndjson` (4 unique invocations)

Each fixture's README documents what the operator does and
what our shim handles.  These are the load-bearing reference
for any future shim-translation work.

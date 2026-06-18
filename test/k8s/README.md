# Kubernetes / CloudNativePG verification

This directory holds an **executable, re-runnable** verification that
`pg_hardstorage` backs up and restores a CloudNativePG (CNPG) cluster
correctly.

## Why this is a script and not a testkit scenario

The testkit (`internal/testkit`) understands `kind` and `k8s-remote`
topology providers *by name*, but both are still **stubs** — see
`internal/testkit/topology/topology.go`. A `.scenario.yaml` pointing
at `provider: kind` fails fast with a structured "topology lands"
error rather than running. So a CNPG scenario cannot run inside the
testkit harness yet.

Until the k8s topology provider is implemented, `cnpg-verify.sh` is
the proof behind every "CloudNativePG: verified" claim — on the
website, in `docs/how-to/k8s-quickstart.md`, and in the compatibility
matrix. It is real, it runs, and it fails loudly if the round-trip
is not byte-equal.

## Files

| File | Purpose |
|------|---------|
| `cnpg-cluster.yaml` | A minimal single-instance CNPG `Cluster` CR. |
| `cnpg-verify.sh`    | End-to-end backup → verify → restore → byte-compare. |

## Running it

Requires `kubectl` (pointed at a running cluster, e.g. minikube),
`docker`, `psql`, and a built `./pg_hardstorage` binary in the repo
root.

```sh
minikube start --driver=docker --memory=4096 --cpus=2
go build -o pg_hardstorage ./cmd/pg_hardstorage
./test/k8s/cnpg-verify.sh
```

Exit 0 means the table checksum read from the restored, recovered
data directory is identical to the checksum on the live CNPG
primary. Any other exit code is a failure — the script prints the
PostgreSQL logs on a recovery failure and the two checksums on a
mismatch.

## What it proves — and what it does not

**Proven:** `pg_hardstorage` connects to a CNPG primary over the
PostgreSQL replication protocol, takes a `BASE_BACKUP`, stores it in
a content-addressed repository, verifies the manifest signature and
chunk hashes, restores the data directory, and the restored cluster
recovers to a consistent state with byte-identical data.

This works because CNPG runs **real PostgreSQL** and exposes the
replication protocol — `pg_hardstorage` talks to it exactly as it
would to a self-managed server. CNPG operates in its **native**
mode; no Barman-compatibility shim is involved.

**Not proven / not provided:** there is no CNPG-I backup plugin, no
Helm chart, and no operator integration. `pg_hardstorage` runs as
its own workload (CronJob / Deployment) alongside the cluster — see
`docs/how-to/k8s-quickstart.md`. A `REPLICATION`-capable role is
required, which is why fully managed DBaaS offerings (RDS, Cloud
SQL, …) that do not expose the replication protocol are unsupported.

## Last verified

2026-05-18 — CNPG operator 1.25.1, PostgreSQL 17, on minikube.
Backup → verify (700/700 chunks, signature valid) → restore (1274
files) → restored checksum byte-equal to the live cluster.

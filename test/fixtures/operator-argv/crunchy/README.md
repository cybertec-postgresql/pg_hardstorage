# Crunchy (PGO v5) pgbackrest argv fixtures

Captured by running `crunchy-argv-recorder:17` (an overlay of
`registry.developers.crunchydata.com/crunchydata/crunchy-postgres:ubi9-17.5-2520`
that swaps in our `argv-recorder` binary in place of
`pgbackrest`) inside Crunchy PGO v6.0.1 with a single-instance
`PostgresCluster` CRD pointing at in-cluster MinIO.

## Files

- `argv-fixtures.ndjson` — 4 unique pgbackrest invocation
  shapes seen during cluster bring-up + a forced WAL switch.
- `manifests/discovery.yaml` — Namespace + MinIO + bucket
  bootstrap Job + `PostgresCluster` CRD using the recorder
  image.

## What Crunchy actually invokes — the bombshell

| verb | shape | our shim handles? |
|---|---|---|
| `pgbackrest server` | (no args) — long-running TLS RPC server | **NO** |
| `pgbackrest server-ping` | (no args) — health probe | **NO** |
| `pgbackrest stanza-create --stanza=db` | repo init equivalent | yes (stanza_create.go) |
| `pgbackrest --stanza=db archive-push pg_wal/...` | WAL archive | yes (archive.go) |

**Crunchy PGO v5 uses pgbackrest in TLS-server mode.** Every
pgbackrest invocation between the postgres pod and the
backup-runner pod is mediated by a long-running `pgbackrest
server` process on each side, with mTLS-authenticated RPCs in
between (see Crunchy's "TLS replication" architecture docs,
PGO v5.0+).

This is fundamentally different from the standalone-pgbackrest
model our shim was designed for (where pgbackrest is a
short-lived CLI invoked by cron / archive_command / a backup
script).  Our shim's verb dispatcher would refuse `server` /
`server-ping` and the cluster would fail to bootstrap.

## Implication for the K8s drop-in claim

**Standalone pgBackRest deployments**: drop-in works (our
existing shim covers `backup`, `restore`, `archive-push`,
`archive-get`, `info`, `check`, `verify`, `stanza-create`).

**Crunchy PGO**: drop-in does NOT work today.  To make it work
we'd need to implement either:

1. The pgbackrest TLS-server protocol — a meaningful RPC stack
   built on libpgbackrest's wire format.  Order-of-weeks work.
2. A "translation server" sidecar that speaks pgbackrest's
   server protocol on one side and dispatches to native
   pg_hardstorage CLI on the other.  Same ~weeks of work but
   neater architectural fit.

Neither is in v1 of the K8s drop-in plan.  Crunchy users who
want pg_hardstorage today should run pg_hardstorage as a
**sidecar via the existing Helm chart**
(`charts/pg-hardstorage-sidecar`) alongside the Crunchy cluster,
not as a binary overlay on Crunchy's image.

## Status

- ✅ argv discovery complete — fixtures captured for all 4
  verbs Crunchy fires during normal operation.
- ❌ Drop-in via image overlay does NOT work — `pgbackrest
  server` mode unsupported.
- 📋 Tracked as a follow-up task; out of scope for the v1
  K8s test infra.

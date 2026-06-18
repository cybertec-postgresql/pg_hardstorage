# API Reference

The control plane exposes a REST API and (post-v0.5) a gRPC service.
Both speak the same `pg_hardstorage.v1` schema as the CLI's JSON
output. The control plane itself ships in v0.5 — what's documented
here is the contract that v0.1 binaries are built to honour.

OpenAPI 3.1 spec: `api/openapi.yaml` (will be filled in fully in
v0.5; the contracts below are stable).

---

## Versioning

All routes live under `/v1/`. The `v1` major version commits to a
**24-month backward-compatibility window**: a script written
against `v1` in 2026 keeps working through 2028. New fields may
appear; existing fields don't change shape; existing routes don't
change semantics.

When a breaking change is unavoidable, `/v2/` is added alongside
`/v1/`; clients migrate at their own pace.

---

## Authentication

Two mechanisms, composable:

- **mTLS.** Client presents a certificate signed by the control
  plane's CA. Identity = the certificate Subject's `CN`.
- **Bearer tokens.** `Authorization: Bearer <token>`. Tokens are
  issued by the control plane (or a configured OIDC provider) and
  scoped to RBAC verbs.

Default-deny: a request that authenticates but lacks the required
verb returns `403 Forbidden` with a structured `error.code` of
`authz.denied`.

RBAC verbs include `backup:create`, `backup:read`,
`restore:execute`, `kms:rotate`, `kms:shred`, `audit:read`,
`admin:*`. Verbs are tenant-scoped.

---

## Conventions

All responses are wrapped:

```json
{
  "schema": "pg_hardstorage.v1",
  "command": "backups.list",
  "generated_at": "2026-04-29T14:21:08Z",
  "result": {
    "body": { "...route-specific..." }
  }
}
```

Errors use the same wrapper with `error` instead of `result`:

```json
{
  "schema": "pg_hardstorage.v1",
  "error": {
    "code":    "wal.slot_missing",
    "message": "Replication slot 'pg_hardstorage_db1' not present.",
    "subject": { "deployment": "db1" },
    "suggestion": {
      "human":   "Recreate the slot.",
      "command": "pg_hardstorage wal repair db1",
      "doc_url": "https://docs.pghardstorage.org/runbooks/wal-slot-missing"
    }
  }
}
```

HTTP status codes map to the CLI exit-code contract:

| HTTP | CLI exit | Meaning              |
| ---- | -------- | -------------------- |
| 200  | 0        | Success              |
| 400  | 2        | Misuse / bad request |
| 401  | 3        | Auth required        |
| 403  | 3        | Auth denied          |
| 404  | 6        | Not found            |
| 409  | 7        | Conflict             |
| 412  | 4        | Pre-flight failed    |
| 422  | 9        | Verify failure       |
| 503  | 8        | Storage / KMS unreachable |
| 500  | 1        | Generic error        |

---

## Routes

### Health and metrics

```
GET  /v1/healthz                     # liveness
GET  /v1/readyz                      # KMS reachable + repo reachable + leader-elected
GET  /metrics                        # Prometheus exposition (not under /v1/; matches Prometheus convention)
```

### Deployments

```
GET    /v1/deployments               # list
POST   /v1/deployments               # create (idempotent on name)
GET    /v1/deployments/{d}           # show
PATCH  /v1/deployments/{d}           # update fields
DELETE /v1/deployments/{d}           # remove (preserves backups)
GET    /v1/deployments/{d}/health    # doctor for one deployment
```

### Backups

```
GET    /v1/deployments/{d}/backups                  # list
POST   /v1/deployments/{d}/backups                  # take a backup; streams NDJSON progress
GET    /v1/deployments/{d}/backups/{id}             # show one (full manifest)
DELETE /v1/deployments/{d}/backups/{id}             # tombstone (soft-delete)
POST   /v1/deployments/{d}/backups/{id}/verify      # fast-verify; streams NDJSON
POST   /v1/deployments/{d}/backups/{id}/hold        # legal hold
DELETE /v1/deployments/{d}/backups/{id}/hold        # release hold
```

`POST /backups` returns a streaming NDJSON body — one event per
line, same schema the CLI emits with `-o ndjson`. Final event is
`backup_completed` (or an error frame).

### Restores

```
POST /v1/deployments/{d}/restores       # initiate; streams NDJSON progress
GET  /v1/deployments/{d}/restores/{id}  # status
```

Request body for `POST /restores`:

```json
{
  "backup_id": "db1.full.20260427T093017Z",
  "target":    "/var/lib/postgresql/restored",
  "to":        "5 minutes ago",            // or "to_lsn": "0/3000028", "to_name": "..."
  "verify":    "auto",                      // auto | skip | require
  "force":     false
}
```

The pre-flight refusals (target non-empty, KMS unreachable, Patroni
primary, etc.) return `412 Precondition Failed` with the same
`Suggestion` shape as the CLI.

### WAL

```
GET   /v1/deployments/{d}/wal                        # segments + gaps
POST  /v1/deployments/{d}/wal/{seg}/fetch            # fetch one segment (used by restore_command shim)
POST  /v1/deployments/{d}/wal/repair                 # recreate slot, resync
```

### Repository

```
GET    /v1/repos/{r}                                 # HSREPO + tenants (v0.5+)
POST   /v1/repos/{r}/check                           # composite health pass (v0.5+)
POST   /v1/repos/{r}/gc                              # routine orphan sweep (v0.5+)
GET    /v1/repos/{r}/usage                           # bytes by category (v0.5+)
POST   /v1/repos/{r}/scrub                           # full SHA round-trip (v0.5+)
```

`POST /gc` and `POST /scrub` accept `?apply=true`; both stream
NDJSON progress.

### KMS

```
POST /v1/kms/rotate                                  # walk manifests, rewrap DEKs (v0.5+)
POST /v1/kms/shred                                   # destroy KEK, write audit (v0.5+)
GET  /v1/kms/inspect                                 # keyring summary (v0.5+)
```

### Audit

```
GET  /v1/audit                                       # search; filter by since/action/deployment (v0.5+)
POST /v1/audit/verify-chain                          # walk Merkle chain (v0.5+)
```

### Doctor

```
GET /v1/doctor                                       # full report (v0.5+)
GET /v1/doctor/{deployment}                          # one deployment (v0.5+)
```

### Fleet

```
GET /v1/agents                                       # registered agents (v0.5+)
GET /v1/search?q=<expr>                              # fleet-wide backup search (v0.5+)
```

---

## Streaming endpoints

Backup, restore, verify, and WAL stream return chunked NDJSON. Each
line is a typed `Event`:

```json
{"schema":"pg_hardstorage.v1","severity_name":"info","op":"backup_started","subject":{"deployment":"db1","backup_id":"..."}}
{"schema":"pg_hardstorage.v1","severity_name":"info","op":"progress","body":{"bytes_logical":4194304000,"bytes_physical":1342177280,"dedup_ratio":3.12,"throughput_mb_s":620}}
{"schema":"pg_hardstorage.v1","severity_name":"warning","op":"chunker_paused","body":{"reason":"backpressure","stage":"storage_put"}}
{"schema":"pg_hardstorage.v1","severity_name":"notice","op":"backup_completed","body":{"verified":true,"duration_seconds":847}}
```

The same payload reaches every configured Sink concurrently — your
Slack webhook, your Jira board, and the API consumer see the same
event.

A streaming endpoint that fails mid-stream emits a final error
frame and closes the connection without a trailing newline; clients
should handle `EOF` followed by an error frame as a normal failure.

---

## gRPC (v0.5+)

Defined in `proto/pg_hardstorage/v1/`. Services:

- `BackupService` — `Take`, `List`, `Show`, `Delete`, `Verify`
- `RestoreService` — `Initiate`, `Status`, `Cancel`
- `WALService` — `List`, `Fetch`, `Repair`
- `RepoService` — `Init`, `Check`, `GC`, `Usage`, `Scrub`
- `KMSService` — `Rotate`, `Shred`, `Inspect`
- `AuditService` — `Search`, `VerifyChain`
- `DoctorService` — `Check`
- `AdminService` — `Health`, `Ready`, `Version`

Streaming RPCs use the same `Event` payload as the REST NDJSON
endpoints.

---

## OpenAPI

The full schema lives at `api/openapi.yaml` — generated from the Go
types in `internal/api/rest/`. The v0.1 file is a stub committing
to the routes above; v0.5 fleshes out every body schema.

A live control plane exposes `/v1/openapi.yaml` so clients can pin
to the version they're talking to.

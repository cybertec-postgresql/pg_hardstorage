<!-- AUTO-GEN candidate: emit from storage.Schemes() and per-plugin URL parser godoc; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Storage URL schemes
description: The five repo-URL schemes — URL form, query parameters, auth chain, and capability matrix.
tags:
  - reference
  - storage
  - repo
---

# Storage URL schemes

A repo URL is parsed by `storage.Open` (in
[`internal/plugin/storage/registry.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/plugin/storage/registry.go))
and dispatched on the URL scheme.  Every Tier-1 backend
self-registers via `init()`; Tier-2 plugins register at
runtime.  Six schemes ship in the binary today.

## At a glance

| Scheme | Backend | `IfNotExists` | Native Rename | WORM | Region-aware | Free-space probe |
| --- | --- | --- | --- | --- | --- | --- |
| `file://` | local filesystem | atomic via `O_CREATE\|O_EXCL` | atomic via `link(2)+unlink(2)` | no | no | yes (`statfs`) |
| `s3://` | S3 / MinIO / R2 / B2 | atomic via `If-None-Match: *` | emulated | yes (Object Lock) | yes | no |
| `azblob://` | Azure Blob Storage | atomic via `If-None-Match: *` | emulated | yes (Immutable Storage) | no | no |
| `gcs://` | Google Cloud Storage | atomic via `Conditions{DoesNotExist:true}` | emulated | yes (Object Lock, 2024+) | no | no |
| `sftp://` | SSH/SFTP | emulated (stat→tmp→rename) | atomic when server advertises `posix-rename@openssh.com`; else best-effort | no | no | no |
| `scp://`  | SSH (shell-exec) | emulated (stat→tmp→`mv -T`) | atomic via `rename(2)` (same fs) | no | no | no |

`Capabilities()` is the runtime predicate — backends with
WORM advertise it.  Hosts that need a capability MUST gate
on `Capabilities()` (see [storage contract](plugins/storage-contract.md)).

---

## `file://`

```
file:///absolute/path
```

The host must be empty (or `localhost`).  The path is the
absolute repository root; relative paths are rejected
rather than silently rebased.

| Field | Value |
| --- | --- |
| **Parsed by** | [`internal/plugin/storage/fs/fs.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/plugin/storage/fs/fs.go) |
| **Auth** | filesystem ACLs (chunks `0640`, dirs `0750`) |
| **Atomicity** | `O_CREATE\|O_EXCL` for `IfNotExists`; `<key>.tmp` + `fsync` + `rename(2)` for plain Put; `link(2)` + `unlink(2)` for `RenameIfNotExists` |
| **Capabilities** | `ConditionalPut` only |

The fs plugin is the only backend that implements
`FreeSpaceAware`; capacity preflight against object stores
silently passes (the operator's quota is set out-of-band).

---

## `s3://`

```
s3://<bucket>/<optional-prefix>?region=...&endpoint=...&path_style=true&storage_class=...
```

Examples:

```
s3://acme-pg-backups
s3://acme-pg-backups/prod/db1
s3://minio-bucket/test?endpoint=https://minio.local:9000&path_style=true
s3://acme-pg-backups?region=eu-west-1&storage_class=STANDARD_IA
```

| Query | Meaning |
| --- | --- |
| `region` | explicit AWS region; otherwise picked up from env / profile |
| `endpoint` | S3-compatible endpoint URL (MinIO, Cloudflare R2, B2). When set, `path_style` is forced on |
| `path_style` | `true` to force path-style addressing (bucket in path, not host) |
| `storage_class` | default `StorageClass` for `Put` when not set per-request (`STANDARD`, `STANDARD_IA`, `GLACIER`, …) |
| `checksum` | SDK checksum posture. `when_required` (default) — only attach checksums to operations that explicitly need them. Required for Ceph-RGW services (Hetzner Object Storage, some MinIO configurations, Backblaze B2 strict mode) that reject the v1.36+ default CRC32 header with `400 InvalidRequest`. `when_supported` opts back into the SDK default for real AWS where the duplicate integrity check is harmless. The CAS layer already content-addresses every chunk by SHA-256, so the SDK-level CRC32 is duplicative regardless of the setting. |

| Field | Value |
| --- | --- |
| **Auth** | AWS SDK v2 default credential chain (env vars → IRSA → IAM role → profile → SSO). Inline URL credentials are not accepted |
| **Atomicity** | `If-None-Match: *` for `IfNotExists`; rename emulated via copy+delete |
| **WORM** | S3 Object Lock — `SetRetention` maps `WORMCompliance` / `WORMGovernance` onto Object Lock modes |
| **HTTP timeout** | per-request timeout = 5 min (`DefaultHTTPTimeout`); operators wanting tighter limits pass a `ctx` deadline |
| **Capabilities** | `WORM`, `ConditionalPut`, `Multipart`, `ServerSideEncryption`, `CrossRegionReplicate`, `StorageClassSelectable` |

---

## `azblob://`

```
azblob://<account>/<container>[/<prefix>][?option=...]
```

Examples:

```
azblob://acmebackups/prod
azblob://acmebackups/prod/db1?access_tier=cool
azblob://acmebackups.blob.core.usgovcloudapi.net/prod   # sovereign cloud
```

A bare account name implies `.blob.core.windows.net`; a
dotted account name is taken literally so US Government
Cloud / Azure China / custom-domain accounts work without
a config flag.

| Query | Meaning |
| --- | --- |
| `access_tier` | `Hot`, `Cool`, `Cold`, `Archive` |
| `endpoint` | full service-URL override (private endpoint, sovereign cloud) |
| `account_key` | base64 storage account key for shared-key auth (test convenience) |

| Field | Value |
| --- | --- |
| **Auth** | `azidentity.NewDefaultAzureCredential` (env → managed identity → Azure CLI → IDE) by default; shared-key when `account_key` is set |
| **Atomicity** | `If-None-Match: *` via `ModifiedAccessConditions{IfNoneMatch: ETagAny}` |
| **WORM** | per-blob immutability policy (`Locked` / `Unlocked`); requires version-level immutability at container creation |
| **Capabilities** | `ConditionalPut` |

---

## `gcs://`

```
gcs://<bucket>[/<prefix>][?option=...]
```

Examples:

```
gcs://acme-pg-backups
gcs://acme-pg-backups/prod/db1
gcs://acme-pg-backups?storage_class=NEARLINE
```

| Query | Meaning |
| --- | --- |
| `storage_class` | `STANDARD`, `NEARLINE`, `COLDLINE`, `ARCHIVE` |
| `endpoint` | private endpoint URL (Private Google Access) |
| `credentials_file` | path to a service-account JSON file (otherwise ADC) |

| Field | Value |
| --- | --- |
| **Auth** | Application Default Credentials (env → metadata service → `gcloud auth`) |
| **Atomicity** | `If: storage.Conditions{DoesNotExist: true}` |
| **WORM** | per-object `Retention` field (Object Lock for GCS, 2024+).  Older buckets without retention configured return `ErrUnsupported` from `SetRetention` |
| **Capabilities** | `ConditionalPut` |

---

## `sftp://`

```
sftp://[user@]host[:port]/<absolute-path>
```

Examples:

```
sftp://backup@nas.example.com/srv/pg-hardstorage
sftp://nas.example.com:2222/data/backups
```

Configuration **extras** (under `repo.extras` in
`pg_hardstorage.yaml`, never in the URL):

| Extras key | Meaning |
| --- | --- |
| `identity_file` | path to a private key (recommended) |
| `identity_passphrase` | optional, for an encrypted key |
| `known_hosts` | path to a known_hosts file — **required** |
| `password` | discouraged; only auth fallback when no key is set |

The plugin **rejects** `StrictHostKeyChecking=no`;
`known_hosts` is mandatory.  Silently trusting unknown hosts
would let a network attacker MITM the entire backup
pipeline; this is the single most common SFTP
misconfiguration in audited environments.

| Field | Value |
| --- | --- |
| **Auth** | SSH public key (default) or password |
| **Atomicity** | `IfNotExists` is stat-then-write-then-rename (TOCTOU window inherent to SFTP).  Rename uses `posix-rename@openssh.com` when the server advertises it; falls back to plain `Rename` otherwise |
| **WORM** | not supported; `SetRetention` returns `ErrUnsupported` |
| **Capabilities** | `ConditionalPut` (emulated) |

CAS chunks are content-addressed so a duplicate write is
harmless; manifests commit through `RenameIfNotExists`
which catches the read-after-write case.

---

## `scp://`

```
scp://[user@]host[:port]/<absolute-path>
```

Examples:

```
scp://backup@nas.example.com/srv/pg-hardstorage
scp://nas.example.com:2222/data/backups
```

Same `extras` shape as `sftp://` (`identity_file`,
`identity_passphrase`, `known_hosts`, `password`); same
hostkey posture (`known_hosts` is mandatory; the plugin
refuses `StrictHostKeyChecking=no`).

### Why a separate scheme when sftp:// exists?

Some hardened SSH deployments disable the SFTP subsystem
(`Subsystem sftp` commented out in sshd_config) but still
permit ssh-exec; some embedded / appliance SSH servers don't
implement SFTP at all.  The `scp://` backend talks to those
servers using the same set of remote-shell primitives `scp`
itself uses — `cat`, `stat`, `find`, `mv`, `rm`, `mkdir`.
No SFTP subsystem required.

### What this is NOT

The `scp://` backend does **not** speak the legacy SCP wire
protocol (`C0644 size name` framing).  The scp wire format
has a documented security history (CVE-2018-20685,
CVE-2019-6111, CVE-2019-6109) and is being deprecated by
OpenSSH itself.  Instead, the plugin uses ssh-exec with
stdin / stdout streaming for data (`cat > path` / `cat path`)
and shell commands for filesystem ops — the same posture
`paramiko-scp` and Ansible's `synchronize` module ship with.

### Path safety

Every key flows through a single shell-quoting helper
(`'…'` with embedded-quote escape via `'\''`).  POSIX-portable;
no shell expansion on the remote.  The repo's keys are
always content-addressed paths (`chunks/sha256/aa/bb/...`)
or manifest paths so the surface is structurally narrow,
but the quoting defends against future schema changes.

| Field | Value |
| --- | --- |
| **Auth** | SSH public key (default) or password |
| **Atomicity** | `IfNotExists` is stat-then-write-then-`mv -T` (atomic via `rename(2)` on the same fs).  TOCTOU window same as the SFTP backend; CAS dedup absorbs duplicate writes. |
| **WORM** | not supported; `SetRetention` returns `ErrUnsupported` |
| **Capabilities** | `ConditionalPut` (emulated) |

### Picking `scp://` vs `sftp://`

Both ride SSH.  Default to `sftp://` unless your server
forbids it: SFTP is a stateful protocol (one session, many
ops) and avoids forking a shell per operation, so it's
faster for big repos.  `scp://` is the right choice when:

- The remote SSH server has the SFTP subsystem disabled.
- The remote is an embedded / appliance device that doesn't
  implement SFTP.
- Compliance review insists on the smallest SSH surface
  enabled for backups.

## Air-gap

All four cloud schemes consult `airgap.Default()` on
`Open` and reject endpoints that resolve to a public IP
when `PG_HARDSTORAGE_AIRGAPPED=1`.  Operators in air-gap
mode point at a VPC / private endpoint that resolves to
RFC1918; the routable-private-IP allowlist accepts it.

## See also

- [Plugins → Storage contract](plugins/storage-contract.md) —
  the `StoragePlugin` interface every backend implements.
- [How-to → Add an S3 repository](../how-to/adding/repository-s3.md)
  — operator workflow.
- [Explanation → Content-addressed storage](../explanation/content-addressed-storage.md) —
  why CAS absorbs duplicate writes.

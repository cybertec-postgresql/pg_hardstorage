# Operator Guide

This is the reference for day-2 operations: running backups, restoring
under time pressure, applying retention, verifying restorability,
managing the repository, ferrying WAL across the network, custody of
encryption keys, and knowing what each command tells you when
something goes wrong.

Audience: a DBA who is at a terminal, possibly at 3am, and wants
exact commands. New to the tool? Start at
[getting-started](../tutorials/getting-started.md).

---

## 1. Daily operations

### Backup

```sh
pg_hardstorage backup db1
```

Reads `pg_hardstorage.yaml` for the deployment's PG connection and
repo URL. To override at the call site:

```sh
pg_hardstorage backup db1 \
    --pg-connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo file:///srv/backups
```

The pipeline is `BASE_BACKUP` over libpq → tar parser → FastCDC
chunker → CAS PUTs → signed manifest. Default compression is
zstd `SpeedBetterCompression`; default encryption is on if a KEK is
present at the keyring path. Force one or the other with
`--encrypt` / `--no-encrypt`. Backup IDs follow
the shape `db1.full.20260427T093017Z`.

For NDJSON progress (one event per line) suitable for piping to `jq`:

```sh
pg_hardstorage backup db1 -o ndjson
```

### Status, list, show

```sh
pg_hardstorage status            # one line per deployment
pg_hardstorage status db1        # detail for one
pg_hardstorage list db1          # backups for a deployment
pg_hardstorage show db1 <backup-id>
```

`status` returns RPO (now − latest backup completion), WAL lag, and
the next scheduled run. `show` dumps the full manifest including
LSN range, timeline, dedup ratio, encryption envelope details, and
the verification record if one has been written.

### Doctor

```sh
pg_hardstorage doctor
pg_hardstorage doctor db1
pg_hardstorage doctor --exit-on-issues   # exit 10 if anything is wrong
```

Validates: paths resolution, config loaded and well-formed, keystore
presence (signing key + KEK), deployment reachability, repo
writability, slot health. Each finding prints `Suggested fix:` with
the exact command to run.

---

## 2. Restore

### Interactive

```sh
pg_hardstorage restore db1
```

With no positional argument the command lists backups, prompts for
selection, runs pre-flight checks, and asks for confirmation. The
answer is `y` to proceed, anything else aborts with exit 5
(operator-aborted).

### Latest, with one confirmation

```sh
pg_hardstorage restore db1 latest --target /var/lib/postgresql/restored
```

### PITR

Three forms, mutually exclusive:

```sh
--to "5 minutes ago"            # natural-language time
--to "2026-04-27 09:42:00 UTC"  # RFC3339-ish
--to-lsn 0/3000028              # exact LSN
--to-name pre_release           # named restore point you created with pg_create_restore_point
```

Natural-language parsing supports `<n> minutes/hours/days ago`,
`yesterday`, `today HH:MM`, plain RFC3339, and the common
`YYYY-MM-DD HH:MM TZ` form. Anything else returns
`usage.bad_time` (exit 2).

### Preview

```sh
pg_hardstorage restore db1 latest \
    --target /var/lib/postgresql/restored \
    --to "5 minutes ago" \
    --preview
```

Prints what would happen — source backup, WAL replay range, RTO
estimate, target tablespace mapping, verification gate — and exits
without touching disk. Pair with `--force` to run the same
operation non-interactively after operator review.

### Refusals (pre-flight, exit 4)

The restore refuses up-front, before anything is written:

- target dir exists and is non-empty (override with `--force`, which
  is destructive and asks again)
- target dir contains a live `postmaster.pid`
- KMS key is unreachable (cannot decrypt)
- repo manifests fail signature verification
- requested LSN falls inside a WAL gap recorded by the gap auditor

Each refusal carries a `Suggestion`. None of them mutate state.

---

## 3. Retention

### Default policy: GFS

```yaml
deployments:
  db1:
    retention:
      policy: gfs
      keep_daily: 7
      keep_weekly: 4
      keep_monthly: 12
      keep_yearly: 5
```

`rotate` runs after every backup commit and as a scheduled job. Manual
invocation is dry-run by default:

```sh
pg_hardstorage rotate db1                # dry-run, prints decisions
pg_hardstorage rotate db1 --apply        # actually tombstone
```

### Other policies

```yaml
retention:
  policy: simple
  keep_for: 30d        # delete anything older than 30 days
```

```yaml
retention:
  policy: count
  keep_full_count: 14  # keep last N fulls; WAL kept while needed for PITR
```

### Soft-delete via tombstones

`rotate --apply` writes a `<manifest>.json.tombstone` marker beside the
live manifest. List operations filter tombstoned IDs; reads return
`ErrTombstoned`. The chunks themselves stay until `repo gc` sweeps
them. This makes retention reversible up until the next GC, and means
a misset flag never silently destroys data.

### Safety net

The newest manifest is **always** kept regardless of policy output.
A misset `--keep-for 1m` does not leave the deployment with zero
backups.

### Manual rotate

```sh
pg_hardstorage rotate db1 --policy simple --keep-for 7d --apply
pg_hardstorage rotate db1 --policy count --keep-fulls 5 --apply
pg_hardstorage rotate db1 --policy gfs \
    --keep-daily 7 --keep-weekly 4 --keep-monthly 12 --keep-yearly 5 \
    --apply
```

### Holds

A legal hold pins one backup against retention regardless of policy:

```sh
pg_hardstorage hold add db1 <backup-id> --repo ... \
    --holder ops@acme --reason "GDPR Art 17 #4421"
pg_hardstorage hold list --repo ...
pg_hardstorage hold remove db1 <backup-id> --repo ... --yes
```

`rotate --apply` filters held backups before SoftDelete and reports
them under `held` / `held_ids` in the result body.

---

## 4. Verification

Two tiers:

### Fast verify (signature + chunk SHA round-trip)

```sh
pg_hardstorage verify db1 latest --repo ...
pg_hardstorage verify db1 <backup-id> --repo ... --sample 1000
```

Validates the manifest's Ed25519 signature, then for each referenced
chunk: read from CAS, decrypt with the resolved KEK, verify the
plaintext SHA-256 against the manifest entry. `--sample N` caps the
chunk count for spot-checks. No PG, no restore, no WAL replay. Exit 9
on mismatch.

### Full verify (sandbox restore + pg_verifybackup)

The `verify` command's full mode is in v0.5; today, the
`pg_verifybackup` gate runs automatically after every `restore` —
that is the integration test. Skip it with `--verify=skip` only after
acknowledging that exit 9 is the contract.

`--verify=auto` (default) runs `pg_verifybackup` if the binary is on
`$PATH`. `--verify=require` returns `usage.no_pg_verifybackup` (exit 2)
when the binary is missing.

### Repo scrub

The full SHA round-trip across every chunk in the repo:

```sh
pg_hardstorage repair scrub <repo-url>
```

Mismatches surface as `verify.scrub_mismatch` (exit 9). Schedule this
weekly on hot repos; monthly is fine for cold archive repos.

---

## 5. Repository management

### init

```sh
pg_hardstorage repo init file:///srv/backups
pg_hardstorage repo init 's3://acme-backups/?region=us-east-1'
pg_hardstorage repo init 's3://minio/?endpoint=https://minio.acme.example.com&path_style=true'
```

Idempotent on URL: a second init returns `conflict.repo_exists`
(exit 7). Concurrent inits race-safely — exactly one wins.

### check

```sh
pg_hardstorage repo check file:///srv/backups
```

Composite health pass: HSREPO sanity, manifest signatures, chunk
reference completeness, tombstone hygiene. Missing-chunk findings
flagged `verify.missing_chunks` (exit 9).

### gc

```sh
pg_hardstorage repo gc file:///srv/backups            # dry-run
pg_hardstorage repo gc file:///srv/backups --apply    # delete orphans
```

Walks every manifest (including tombstoned), builds the live chunk
set, lists everything in `chunks/sha256/` and reports the difference.
Result body carries `bytes_reclaimable` (dry-run) or `bytes_reclaimed`
(applied).

### usage

```sh
pg_hardstorage repo usage file:///srv/backups
```

Bytes by category — chunks, primary manifests, replica manifests,
trash, WAL, audit. Useful for explaining the bill.

### scrub stub

`repo scrub` is the periodic auto-heal job (re-hash a percentage of
chunks per day, heal from replica region on mismatch). It is a stub
in v0.1; use `repair scrub` for the full pass today.

---

## 6. WAL transport

### Stream mode (the data plane)

```sh
pg_hardstorage wal stream db1 \
    --pg-connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo file:///srv/backups
```

Long-running. Connects with `replication=database`, runs
`START_REPLICATION SLOT pg_hardstorage_db1 PHYSICAL`, assembles
16 MiB WAL segments in memory, chunks each through the CAS, commits
a per-segment manifest atomically. Sends `Standby Status Update`
keepalives every 5 seconds.

The slot is created on first run (`CREATE_REPLICATION_SLOT
pg_hardstorage_db1 PHYSICAL RESERVE_WAL`). It is treated as
permanent — PG holds WAL on disk until the agent ACKs. That means
killing this process for a long time WILL bloat the primary's
`pg_wal/`. Either keep it running or `wal repair` to reset.

### Slot management

```sh
pg_hardstorage wal repair db1               # recreate the slot
pg_hardstorage repair slot db1              # alias for the same thing
```

`wal repair` drops and recreates the slot. The new slot's
`restart_lsn` is whatever PG has for the latest committed WAL; if
that is ahead of the agent's last confirmed LSN, the gap is
detected and reported as `wal_gap_detected`. The repo's WAL inventory
records the gap explicitly; PITR inside the gap window is refused.

### archive_command shim

If you also want belt-and-suspenders archive_command (some regulated
environments double-archive), the binary doubles as a shim:

```
archive_command = '/usr/bin/pg_hardstorage wal push db1 %p --repo file:///srv/backups'
```

`wal push` is a stub in v0.1 (lands in v0.5+); for now stick with
streaming as the only path.

### restore_command shim

Already wired into `restore`'s recovery file generation. PG runs:

```
restore_command = 'pg_hardstorage wal fetch <deployment> %f %p --repo <url>'
```

`wal fetch` exits 0 on success, 6 (`notfound.wal_segment`) on a
segment that isn't in the repo — which PG interprets as "no more WAL,
recovery is done."

---

## 7. Encryption + KMS

### How it works

Three layers:

1. **Local KEK** at `<keyring>/kek.bin` (mode 0600). Generated by
   `init` unless `--no-encrypt`.
2. **Per-backup DEK** (256-bit random), wrapped under the KEK and
   stored in the manifest's `encryption.wrapped_dek` field.
3. **Per-chunk key** derived `Kc = HKDF-SHA256(BDEK, info=chunk_hash)`.
   Cipher: AES-256-GCM. The on-disk envelope is
   `[version=0x02][compression-algo][encryption-algo][12-byte nonce][payload]`
   so each chunk is self-describing.

The chunk **key** is the plaintext SHA-256, so dedup-within-key still
works across compression posture, encryption setting, and re-runs.

### KEK rotation

```sh
pg_hardstorage kms rotate     # v0.5 — walks all manifests, rewraps DEKs
```

`kms rotate` is deferred to v0.5. For v0.1, the practical path is to
run a final backup under the old KEK, then move the keyring path to
the new KEK and start fresh — old backups are still readable as long
as the old keyring is preserved.

### Crypto-shred

```sh
pg_hardstorage kms shred --confirm-keyring <keyring-dir> --reason "GDPR Art 17 #4421" --yes
```

`kms shred` is deferred to v0.5. The semantic is "destroy the KEK,
all backups become bit-for-bit unrecoverable, audit log entry is the
compliance artefact." Plan for it; do not rely on it shipping in
v0.1.

### Inspect the keyring

```sh
pg_hardstorage kms inspect
```

Read-only. Lists each file in the keyring: presence, mode, size,
mtime, public-key SHA-256 fingerprint. Private-key bytes and KEK
bytes are NEVER read. Surfaces a Warning on a private-key file with
mode more permissive than 0600 (the `cp -r` footgun).

---

## 8. Audit log

Append-only Merkle hash chain. Each event is a canonical-JSON record;
the hash links to the previous event's hash, so any tamper is
detectable.

```sh
pg_hardstorage audit append backup.completed --repo file:///srv/backups --reason "manual record"
pg_hardstorage audit search --deployment db1 --since 30d --action restore
pg_hardstorage audit verify-chain --repo file:///srv/backups
```

`verify-chain` walks the entire chain and surfaces two finding types:
hash mismatch (one event's hash field doesn't match the canonical
hash of its content) and chain break (an event's `prev` doesn't
match the actual hash of the previous event). Either fires
`verify.audit_chain_broken` (exit 9).

Files live at `audit/<yyyy>/<mm>/<dd>/<seq>-<id>.json` under the repo
root. Schema: `pg_hardstorage.audit.v1`.

---

## 9. Output modes

Default behaviour:

- TTY → `text` (human-readable, ANSI colour, ASCII tables)
- non-TTY → `json` (single object, schema `pg_hardstorage.v1`)

Override with `-o`:

```sh
-o text       # force text
-o json       # single object, or array for list commands
-o ndjson     # newline-delimited; mandatory for streaming commands
-o yaml       # same schema as JSON, YAML-encoded
-o template   # Go template via --template '{{.result.body.backup_id}}'
```

Or with the env var:

```sh
PG_HARDSTORAGE_OUTPUT=json pg_hardstorage status
```

`--quiet` suppresses non-essential progress lines. `--no-color`
disables ANSI in text mode. The text renderer also honours the
de-facto `NO_COLOR` and `CLICOLOR_FORCE` env vars.

The JSON wrapper is stable:

```json
{
  "schema": "pg_hardstorage.v1",
  "command": "backup",
  "generated_at": "2026-04-28T14:21:08Z",
  "result": {
    "body": { "...command-specific..." }
  }
}
```

24-month backward-compatibility commitment: scripts written against
v0.1 keep working through v1.0+.

Errors in JSON mode are JSON too:

```json
{
  "schema": "pg_hardstorage.v1",
  "error": {
    "code": "wal.slot_missing",
    "message": "Replication slot 'pg_hardstorage_db1' is not present.",
    "suggestion": {
      "human": "Recreate the slot.",
      "command": "pg_hardstorage wal repair db1"
    }
  }
}
```

Exit codes are stable: 0 ok, 1 generic error, 2 misuse, 3 auth,
4 pre-flight failed, 5 aborted by user, 6 not found, 7 conflict,
8 storage/KMS unreachable, 9 verify failure, 10 doctor issues.

---

## 10. Sinks

A sink is an asynchronous output plugin that fans events out to an
external system. Configured declaratively in `pg_hardstorage.yaml`:

```yaml
sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url: https://hooks.slack.com/services/T/B/X
    filter:
      min_severity: warning
      components: ["backup", "wal.stream", "verify", "kms"]

  - name: prod-syslog
    plugin: syslog
    config:
      protocol: tls               # tls | tcp | udp
      address: siem.example.com:6514
      facility: local6
    filter:
      min_severity: notice

  - name: ops-webhook
    plugin: webhook
    config:
      url: https://alerts.example.com/hooks/pg-hardstorage
      authorization: "Bearer kms-secret://ops/webhook-token"

  - name: ops-email
    plugin: email
    config:
      smtp_host: smtp.example.com
      smtp_port: 587
      tls_mode: starttls          # starttls | implicit | none
      auth_mode: plain            # plain | login | none
      username: pg-hardstorage
      password_secret: kms-secret://ops/smtp-password
      from: backups@example.com
      to: ["dba@example.com"]
      cc: ["ops@example.com"]
```

Sinks shipped in v0.1: `slack`, `webhook`, `syslog` (UDP/TCP/TLS,
RFC 5424, octet-counted RFC 6587 framing on stream transports),
`email` (plain SMTP, three TLS modes, three auth modes), `jira`,
`opsgenie`, `pagerduty`. Severity floor is RFC 5424
(emergency=0 … debug=7). Sinks emit when an event's severity is
≤ the floor. Component allow/deny filters compose with severity.

A sink that panics is recovered; siblings still receive the event;
a diagnostic line goes to stderr.

---

## 11. Configuration

`pg_hardstorage.yaml` resolved in order:

1. `--config <path>` (explicit override)
2. `$PG_HARDSTORAGE_CONFIG_DIR/pg_hardstorage.yaml`
3. XDG: `~/.config/pg_hardstorage/pg_hardstorage.yaml`
4. FHS: `/etc/pg_hardstorage/pg_hardstorage.yaml`

Drop-ins under `<config>/conf.d/*.yaml` merge in lex order; later
wins for scalars, sinks append, deployments overlay by name.

Realistic example:

```yaml
schema: pg_hardstorage.config.v1

deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: file:///var/lib/pg_hardstorage/repo
    retention:
      policy: gfs
      keep_daily: 7
      keep_weekly: 4
      keep_monthly: 12
      keep_yearly: 5
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
    classification: confidential

  db2:
    pg_connection: postgres://pgbackup@db2.example.com/postgres
    repo: 's3://acme-backups/?region=eu-central-1'
    retention:
      policy: simple
      keep_for: 30d
    schedule:
      backup: { daily_at: "02:00" }

sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url: https://hooks.slack.com/services/T/B/X
    filter:
      min_severity: warning
```

`pg_hardstorage doctor` validates the resolved config and prints
the path it loaded from. Useful when drop-ins surprise you.

---

## 12. Troubleshooting

For symptom-keyed diagnoses see [troubleshooting](troubleshooting.md).
For full-incident playbooks see
[runbooks/](../reference/runbooks/index.md). For the JSON schema and
exit-code contract see [api](../reference/api/index.md) and the
manpage at `man/man1/pg_hardstorage.1`.

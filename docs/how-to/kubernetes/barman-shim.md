---
title: Run as a Barman shim (host-managed PG)
description: Drop-in replacement for the `barman` and
              `barman-wal-archive` binaries in pod-side
              configurations that wrap a host-managed PG.
tags:
  - kubernetes
  - barman
  - integration
---

# Run as a Barman shim (in-pod)

> Use the **`pg-hardstorage-barman`** and
> **`pg-hardstorage-barman-wal-archive`** binaries as
> drop-in replacements for `barman` and `barman-wal-archive`
> in any pod that today wraps a host-managed PostgreSQL via
> Barman.

The shim ships in pg_hardstorage v1.1 and follows the
[`compat/barman` architecture](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/compat/barman):
the binaries parse Barman flags + INI config, build
synthetic argv for the native CLI, and dispatch via the
public command surface (`pg_hardstorage backup`,
`pg_hardstorage restore`, `pg_hardstorage wal push`).

## When this applies

There is no widely-deployed Kubernetes operator that drives
backups specifically through Barman the way Crunchy PGO
drives pgBackRest or Zalando drives WAL-G.  This doc covers
two real-world patterns:

1. **Sidecar pod with `barman` installed** — a custom
   workload that runs `barman backup <server>` against a
   host-managed PG via SSH / network connection, on a cron
   or controller schedule.
2. **Init / restore pod with `barman recover`** — a
   one-shot Job that recovers a managed PG cluster from an
   existing Barman repository.

Both patterns benefit from the shim: the wrapper script,
job spec, and on-disk config stay identical, but the bytes
that land in the repository are pg_hardstorage-flavour.

If you're not running such a pod, the
[host-package](../packaging/debian-rpm.md) install with
the Debian or RPM `pg-hardstorage-compat-barman` subpackage
plus a `/usr/local/bin/barman` symlink is the simpler path.

## What you need

- A pod or container image that today exec's `barman` /
  `barman-wal-archive`.
- The pg_hardstorage v1.1 image carrying both shim
  binaries.
- A repository URL accessible from the pod.

## Steps

### 1. Build the image with the shim

```dockerfile
FROM your-existing-barman-image:tag

COPY --from=ghcr.io/cybertec-postgresql/pg_hardstorage:v1.1 \
    /usr/bin/pg-hardstorage-barman /usr/bin/pg-hardstorage-barman
COPY --from=ghcr.io/cybertec-postgresql/pg_hardstorage:v1.1 \
    /usr/bin/pg-hardstorage-barman-wal-archive /usr/bin/pg-hardstorage-barman-wal-archive

# Drop-in: barman invocations land on our binary.
RUN ln -sf /usr/bin/pg-hardstorage-barman /usr/local/bin/barman \
 && ln -sf /usr/bin/pg-hardstorage-barman-wal-archive /usr/local/bin/barman-wal-archive
```

`/usr/local/bin` precedes `/usr/bin` on every common
distro's PATH, so any subsequent `which barman` resolves to
the shim — your wrapper script doesn't need to change.

### 2. Translate the Barman config (one-shot, optional)

If you'd like an explicit `pg_hardstorage.yaml` that mirrors
your existing `barman.conf`, run the translator inside the
pod (or on the host) once:

```bash
pg_hardstorage compat translate --from barman \
    /etc/barman.conf \
    --output /etc/pg_hardstorage/pg_hardstorage.yaml
```

Multi-server `barman.conf` files (one `[server]` section
per managed PG) produce one deployment entry per section.
Every Barman setting that doesn't have a direct semantic
equivalent emits as a YAML comment + a stderr summary.

### 3. Repo + KMS configuration

The shim consumes the same Barman config the operator-
facing wrapper supplied; what gets translated:

| Barman config              | pg_hardstorage equivalent     |
|----------------------------|-------------------------------|
| `backup_directory`         | `--repo file:///…`            |
| `backup_method`            | streaming-base / snapshot     |
| `compression`              | `compression:` config (zstd default) |
| `encryption`               | KMS envelope                  |
| `archiver`                 | `barman-wal-archive` binary in archive_command |
| `streaming_archiver`       | native WAL streaming via slot |
| `retention_policy`         | `retention:` config (count / GFS) |

The full mapping is in
[`compat/barman/flags.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/compat/barman/flags.go);
unmapped settings produce a `pg-hardstorage-barman: warn:`
line on stderr at runtime.

### 4. Confirm backups are landing in pg_hardstorage shape

```bash
pg_hardstorage list <deployment> --repo <repo-url>
```

Manifests with `<deployment>.full.<timestamp>` IDs and our
standard repo layout = the shim is active.

## Refusal contract

15 less-common Barman verbs (`cron`, `archive-wal`,
`switch-wal`, `diagnose`, `verify-backup`, `verify`,
`keep`, `receive-wal`, `replication-status`, `show-server`,
`list-server`, `lock-directory-cleanup`,
`rebuild-xlogdb`, `get-wal`, `put-wal`) and `--target-xid`
exit with code 2 and a one-line message of the form

```
pg-hardstorage-barman: <command>: not implemented in v1.1;
native equivalent: <suggestion>
```

— so wrapper scripts that test for "tool exited non-zero"
behave the same as they would against real Barman.

## What's NOT in the shim

- **Reading existing Barman repos.**  Old repos remain
  readable by `barman` only.  The
  [migration playbook](../migration/from-barman.md) covers
  dual-write + retention drain.
- **`barman-cli` interactive features.**  Out-of-band
  commands like the curses-based status viewer aren't
  shimmed; use `pg_hardstorage status` / `pg_hardstorage
  doctor` for the equivalent operational view.

## Why a shim, not a fork

- The Barman config + cron job model is well-understood by
  many operators; a shim lets them keep their muscle memory.
- We own the backup primitive: incremental, content-
  addressed, sandbox-verified, optionally KMS-wrapped.
- A shim inside one image is cheaper than maintaining a
  Barman fork.

## Alternative: native sidecar

If you'd rather not modify the existing Barman image, run
the [sidecar chart](helm-sidecar-chart.md) against the
managed PG's external endpoint.  It coexists with Barman's
existing backups; you get pg_hardstorage backups in your
own repo without touching the existing pod.

## Next steps

- [Migrate from Barman](../migration/from-barman.md) — for
  operators moving off Barman entirely (the v1.1 fast path
  is built around this same shim).
- [pgBackRest shim](pgbackrest-shim.md) — same drop-in
  pattern for Crunchy PGO.
- [WAL-G shim](walg-shim.md) — same drop-in pattern for
  Zalando.
- [Sidecar chart](helm-sidecar-chart.md) — the no-fork
  alternative.

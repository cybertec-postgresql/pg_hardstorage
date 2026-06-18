---
title: Configure pg_hardstorage with a file
description: Where pg_hardstorage looks for its config, what goes in
              the YAML, and how to override per invocation.
tags:
  - configuration
  - getting-started
---

# Configure pg_hardstorage with a file

You don't have to repeat the same `--repo`, `--pg-connection`, and
schedule flags on every invocation.  `pg_hardstorage` reads a YAML
config file at well-known FHS / XDG locations and honours every
deployment + global setting from there.

## Where pg_hardstorage looks

The lookup chain (later wins where there's overlap):

1. **`--config <path>`** on the command line — explicit override.
2. **`$XDG_CONFIG_HOME/pg_hardstorage/pg_hardstorage.yaml`** —
   user-mode default.  Falls back to `~/.config/pg_hardstorage/pg_hardstorage.yaml`
   when `XDG_CONFIG_HOME` is unset.  Use this for desktop / dev /
   per-user setups.
3. **`/etc/pg_hardstorage/pg_hardstorage.yaml`** — system-mode default
   (FHS).  Use this for systemd-managed deployments, containers,
   or any time the binary is run as a system user.
4. **`<config-dir>/conf.d/*.yaml`** drop-ins — applied after the
   main file, in lexicographic order.  Useful for splitting
   secrets out (`conf.d/00-base.yaml` checked in, `conf.d/99-secret.yaml`
   gitignored).

Confirm what your binary resolved at startup:

```bash
pg_hardstorage doctor 2>&1 | grep -i 'config'
```

## What the file looks like

A minimum viable config for one deployment:

```yaml
# /etc/pg_hardstorage/pg_hardstorage.yaml
schema: pg_hardstorage.config.v1

# Repository shared across deployments unless overridden per-deployment.
repo: s3://my-backups/prod

deployments:
  - name: prod
    connection: postgresql://backup_user@db.host:5432/postgres
    schedule:
      backup: "daily_at 02:00"
      rotate: "daily_at 04:00"
    retention:
      policy: simple
      keep_for: 30d
```

Once this file is in place, every invocation that operates on
`prod` uses these defaults — you can run

```bash
pg_hardstorage backup prod
```

instead of repeating the connection string, repo URL, and retention
flags on every call.

A full annotated example with every block (LLM, KMS, retention
policies, notify sinks, residency, SLO) ships at
[`share/pg_hardstorage.sample.yaml`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/share/pg_hardstorage.sample.yaml).
Copy it to your config path and trim the blocks you don't need.

## Operating modes — system vs user

`pg_hardstorage doctor` reports which mode it resolved:

```
config:        /etc/pg_hardstorage/pg_hardstorage.yaml (FHS)      ✓
state:         /var/lib/pg_hardstorage              (FHS)         ✓
keyring:       /etc/pg_hardstorage/keyring          (FHS, mode 0700)
```

- **FHS** — `pg_hardstorage` was invoked as a system user (or root,
  though `pg_hardstorage` refuses to run *as* root and uses
  `pgbackup`/equivalent instead).  Reads `/etc/pg_hardstorage`,
  writes state to `/var/lib/pg_hardstorage`, logs to
  `/var/log/pg_hardstorage`.
- **XDG** — `pg_hardstorage` was invoked by a desktop / interactive
  user.  Reads `~/.config/pg_hardstorage/...`, writes state to
  `~/.local/state/pg_hardstorage/`, logs to `~/.local/state/pg_hardstorage/logs/`.

Either mode supports the same YAML schema; the only difference is
where the file lives on disk.

## Overriding from the command line

Every config field has an equivalent flag.  Precedence is
**flag > env var > config file > default**.  Example:

```bash
# Use the config's defaults but override the repo just for one
# backup (e.g. take an ad-hoc backup to a secondary store):
pg_hardstorage backup prod --repo s3://emergency-backups/prod

# Same, via env var:
PG_HARDSTORAGE_REPO=s3://emergency-backups/prod pg_hardstorage backup prod
```

`pg_hardstorage --config <path>` lets you point at a fully
separate config file — useful for CI / migration scenarios where
the production config shouldn't be touched.

## Where the sample is

The full annotated sample with every block + comments lives in
the repo at `share/pg_hardstorage.sample.yaml`.  Install
recipes for packagers:

- **deb / rpm** packagers should drop `share/pg_hardstorage.sample.yaml`
  at `/usr/share/pg_hardstorage/pg_hardstorage.sample.yaml`; first-run
  scripts copy it to `/etc/pg_hardstorage/pg_hardstorage.yaml` only
  if no operator config already exists (don't clobber).
- **docker / k8s** users: bind-mount or `ConfigMap` your YAML to
  `/etc/pg_hardstorage/pg_hardstorage.yaml` inside the container —
  `pg_hardstorage` picks it up automatically.

## Windows

Windows isn't a supported production target today; the binary builds
and the FHS lookups don't apply.  Use `--config <path>` explicitly
and place the YAML wherever your workflow expects.

# systemd units for the pg_hardstorage agent

Drop-in units for running the `pg_hardstorage` host agent under
systemd. The Debian / RPM packages install these for you; the
files are here for hand-installs and for reference.

## Files

| File | Purpose |
| ---- | ------- |
| [`pg_hardstorage.service`](pg_hardstorage.service) | Single-instance unit. Enable this on a host that backs up one PostgreSQL deployment (or several declared in `/etc/pg_hardstorage/pg_hardstorage.yaml`). |
| [`pg_hardstorage@.service`](pg_hardstorage@.service) | Templated multi-instance unit — one systemd instance per logical deployment. `%i` selects the config under `/etc/pg_hardstorage/deployments/` and gives each instance an isolated state directory. Use this when several tenants share a host and must not share keyrings, audit logs, or backup runs. |
| [`pg-hardstorage.sysusers.conf`](pg-hardstorage.sysusers.conf) | Declares the unprivileged `pgbackup` system user the agent runs as. |
| [`pg-hardstorage.tmpfiles.conf`](pg-hardstorage.tmpfiles.conf) | Creates the state, cache, log, and runtime directories with the right ownership and modes. |

## Install (hand-install)

```sh
# system user + runtime dirs
sudo cp pg-hardstorage.sysusers.conf  /usr/lib/sysusers.d/
sudo cp pg-hardstorage.tmpfiles.conf  /usr/lib/tmpfiles.d/
sudo systemd-sysusers
sudo systemd-tmpfiles --create

# units
sudo cp pg_hardstorage.service pg_hardstorage@.service /etc/systemd/system/
sudo systemctl daemon-reload

# single-instance
sudo systemctl enable --now pg_hardstorage.service

# OR multi-instance, one per deployment config in
# /etc/pg_hardstorage/deployments/<name>.yaml
sudo systemctl enable --now pg_hardstorage@db1.service
```

## Related docs

- [Operations handbook](../../docs/operations/index.md)
- [Operator guide](../../docs/operations/operator-guide.md)
- [Debian / RPM packaging guide](../../docs/how-to/packaging/debian-rpm.md)

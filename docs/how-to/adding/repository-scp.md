---
title: Add an SCP repository
description: URL form, key-based authentication, and known_hosts
              setup for an SCP-backed pg_hardstorage repository.
tags:
  - repo
  - scp
  - ssh
  - on-prem
---

# Add an SCP repository

> The `scp://` scheme stores chunks on a remote Unix host
> over plain SSH command-exec — `cat`, `stat`, `find`, `mv`,
> `rm`, `mkdir`.  Useful when corporate policy disables the
> SFTP subsystem (`Subsystem sftp` commented out in
> sshd_config) but allows ssh-exec, or against embedded /
> appliance SSH servers that don't implement SFTP at all.

## When to pick `scp://` vs `sftp://`

Both ride SSH; both share the same auth + known_hosts model.
Default to [`sftp://`](repository-sftp.md) unless you can't,
because SFTP is a stateful protocol (one session, many ops)
and avoids forking a remote shell per operation.  Pick
`scp://` when:

- The remote SSH server has the SFTP subsystem disabled.
- The remote is an embedded / appliance device that doesn't
  implement SFTP.
- Compliance review insists on the smallest SSH surface
  enabled for backups.

## URL form

```text
scp://[user@]host[:port]/<absolute-path>
```

Examples:

```text
scp://backup@nas.example.com/srv/pg-hardstorage
scp://nas.example.com:2222/data/backups
```

If `user` is omitted the plugin uses `$USER`.  The path
component is the **absolute** root directory on the remote
side; the plugin will not create parent directories above
this point.

Authentication is configured via *extras* (per-repo settings
the StoragePlugin reads from the deployment's `extras` map),
not URL query parameters — secrets in URLs leak through
process listings and shell history.

| Extras key | Meaning |
| --- | --- |
| `identity_file` | Path to a private key (`ed25519`, `rsa`). |
| `identity_passphrase` | Passphrase for the private key. Empty for unencrypted keys. |
| `known_hosts` | Path to a `known_hosts` file. **Required.** |
| `password` | Password auth. Discouraged; prefer keys. |

## What you need

- An SSH-reachable host with an account dedicated to backups.
- An ed25519 keypair generated on the agent host
  (`ssh-keygen -t ed25519 -f /etc/pg_hardstorage/keys/scp_id_ed25519`).
- The remote host's host-key fingerprint captured into a
  `known_hosts` file.
- A directory that the backup user owns and can write to.
- The remote shell environment must have `cat`, `stat`,
  `find`, `mv`, `rm`, `mkdir` on PATH.  Every standard Unix
  shell + busybox satisfies this.

## Steps

### 1. Generate the keypair (agent side)

```bash
ssh-keygen -t ed25519 \
    -f /etc/pg_hardstorage/keys/scp_id_ed25519 \
    -N '' -C 'pg_hardstorage@$(hostname)'
chmod 600 /etc/pg_hardstorage/keys/scp_id_ed25519
```

### 2. Authorise the key on the remote host

Copy the public key onto the remote host and append to the
backup user's `~/.ssh/authorized_keys`:

```bash
ssh-copy-id -i /etc/pg_hardstorage/keys/scp_id_ed25519.pub \
    backup@nas.example.com
```

Lock down the account on the remote host: chroot the user to
the backup directory if your sshd supports it, disable shell
login features the backup doesn't need
(`AllowAgentForwarding no`, `PermitTTY no`,
`X11Forwarding no` for that user in
`/etc/ssh/sshd_config`).

### 3. Capture the host key

```bash
ssh-keyscan -t ed25519,rsa nas.example.com \
    > /etc/pg_hardstorage/keys/known_hosts
chmod 644 /etc/pg_hardstorage/keys/known_hosts
```

Verify the fingerprint out-of-band against the remote
server's `/etc/ssh/ssh_host_*_key.pub` files; an unverified
`ssh-keyscan` trusts whatever the network returned.

### 4. Initialise the repo

```bash
pg_hardstorage repo init scp://backup@nas.example.com/srv/pg-hardstorage
```

```console
repo: scp://backup@nas.example.com/srv/pg-hardstorage
mode: ok
```

The plugin reads `identity_file`, `identity_passphrase`, and
`known_hosts` from the deployment's `extras` map.  Configure
them in `pg_hardstorage.yaml`:

```yaml
deployments:
  db1:
    repo: scp://backup@nas.example.com/srv/pg-hardstorage
    extras:
      identity_file: /etc/pg_hardstorage/keys/scp_id_ed25519
      known_hosts:   /etc/pg_hardstorage/keys/known_hosts
```

### 5. Verify

```bash
pg_hardstorage repo check scp://backup@nas.example.com/srv/pg-hardstorage
```

## Why we refuse `StrictHostKeyChecking=no`

Same reason as the SFTP backend: silently trusting unknown
host keys is the single most common SSH misconfiguration in
audited environments — a network attacker who can MITM the
SSH handshake gets to MITM every backup write and read.  The
plugin requires a real `known_hosts` file.  CI sandboxes
that genuinely don't have a stable host key should write a
per-test `known_hosts` from the test harness.

## What this is NOT

The `scp://` backend does **not** speak the legacy SCP wire
protocol (the binary `C0644 size name` framing).  The scp
wire format has a documented security history
(CVE-2018-20685, CVE-2019-6111, CVE-2019-6109) and is being
deprecated by OpenSSH itself in favour of SFTP.  The `scp`
binary shipped on modern systems already uses SFTP under
the hood by default.

Instead, this plugin uses ssh-exec with stdin / stdout
streaming for data (`cat > path` / `cat path`) and shell
commands for filesystem ops — the same posture
`paramiko-scp`, Ansible's `synchronize` module, and ad-hoc
rsync wrappers all use.  It works against any SSH server
that allows command execution.

## Atomicity model

Same approach as the SFTP backend.  The plugin emulates
`IfNotExists` with stat → write-to-temp → `mv -T` (POSIX
`rename(2)`).  `mv -T` is atomic on the same filesystem on
every POSIX system, the standard guarantee for our
content-addressed chunk store.  The TOCTOU window between
the stat and the rename is the same as `sftp://`'s and is
absorbed by content-addressing: a duplicate write of an
already-present chunk is a no-op.  Manifest commits go
through `RenameIfNotExists` which has the same posture and
is the actual race winner.

## Path safety

Every key flows through a single shell-quoting helper —
single-quote-wrapped with embedded-quote escape via `'\''`.
POSIX-portable; the remote shell does no expansion on
quoted strings (`$vars`, backticks, glob, command
substitution all neutered).  The repo's keys are
content-addressed paths (`chunks/sha256/aa/bb/...`) plus
manifest paths so the surface is structurally narrow, but
the quoting defends against future schema changes.

## Troubleshooting

**`extras.known_hosts is required`** — the plugin refuses to
proceed without one.  See step 3.

**`ssh: handshake failed: ssh: unable to authenticate`** —
the key wasn't authorised on the remote side, or the
passphrase is wrong, or the remote `sshd` rejects ed25519
(very old hosts).  Try:

```bash
ssh -i /etc/pg_hardstorage/keys/scp_id_ed25519 backup@nas.example.com
```

**`ssh: handshake failed: knownhosts: key mismatch`** — the
remote host key changed (legitimate re-key, or attack).
Confirm out-of-band, then update `known_hosts` deliberately.

**`stat: command not found`** or similar — extremely rare,
but minimal embedded SSH servers may ship without coreutils.
The plugin needs `cat`, `stat`, `find`, `mv`, `rm`,
`mkdir`.  If those aren't available, you're on a setup the
backup tool can't drive over plain SSH; switch to
[`sftp://`](repository-sftp.md) or pull the data through a
sidecar that does have a normal toolchain.

**`Permission denied`** on the path — the backup user must
own the path and have `rwx` on it.  Check
`ls -ld /srv/pg-hardstorage` on the remote.

## Performance notes

`scp://` opens one SSH session per operation (per Stat,
per Get, per Put, etc.) versus `sftp://`'s single session
across the run.  For small chunk-by-chunk operations the
session-setup overhead is real (~5-15 ms each on a typical
LAN).  For repos with many small files (heavy CAS-dedup,
millions of chunks) prefer `sftp://`.  For repos with
fewer larger objects (snapshot-style backup, small WAL
volume), the difference is negligible.

## Next steps

- [Add a deployment](deployment.md) wired to this repo
- [Set retention](../operating/set-retention.md)
- [`repo init` CLI reference](../../reference/cli/pg_hardstorage_repo_init.md)
- [`sftp://` repository](repository-sftp.md) — the
  default-recommended SSH-based backend

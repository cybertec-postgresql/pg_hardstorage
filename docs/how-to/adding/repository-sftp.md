---
title: Add an SFTP repository
description: URL form, key-based authentication, and known_hosts
              setup for an SFTP-backed pg_hardstorage repository.
tags:
  - repo
  - sftp
  - on-prem
---

# Add an SFTP repository

> The `sftp://` scheme stores chunks on a remote Unix host over
> SSH. Useful when corporate policy mandates "files on a NAS"
> rather than object storage. The plugin **refuses** the
> `StrictHostKeyChecking=no` posture — a `known_hosts` file is
> required.

## URL form

```text
sftp://[user@]host[:port]/<absolute-path>
```

Examples:

```text
sftp://backup@nas.example.com/srv/pg-hardstorage
sftp://nas.example.com:2222/data/backups
```

If `user` is omitted the plugin uses `$USER`. The path component
is the **absolute** root directory on the remote side; the
plugin will not create parent directories above this point.

Authentication is configured via *extras* (per-repo settings the
StoragePlugin reads from the deployment's `extras` map), not URL
query parameters — secrets in URLs leak through process listings
and shell history.

| Extras key | Meaning |
| --- | --- |
| `identity_file` | Path to a private key (`ed25519`, `rsa`). |
| `identity_passphrase` | Passphrase for the private key. Empty for unencrypted keys. |
| `known_hosts` | Path to a `known_hosts` file. **Required.** |
| `password` | Password auth. Discouraged; prefer keys. |

## What you need

- An SSH-reachable host with an account dedicated to backups.
- An ed25519 keypair generated on the agent host
  (`ssh-keygen -t ed25519 -f /etc/pg_hardstorage/keys/sftp_id_ed25519`).
- The remote host's host-key fingerprint captured into a
  `known_hosts` file.
- A directory that the backup user owns and can write to.

## Steps

### 1. Generate the keypair (agent side)

```bash
ssh-keygen -t ed25519 \
    -f /etc/pg_hardstorage/keys/sftp_id_ed25519 \
    -N '' -C 'pg_hardstorage@$(hostname)'
chmod 600 /etc/pg_hardstorage/keys/sftp_id_ed25519
```

### 2. Authorise the key on the SFTP host

Copy the public key onto the SFTP host and append to the
backup user's `~/.ssh/authorized_keys`:

```bash
ssh-copy-id -i /etc/pg_hardstorage/keys/sftp_id_ed25519.pub \
    backup@nas.example.com
```

Lock down the account on the SFTP host: chroot the user to the
backup directory, disable shell login, set
`AllowAgentForwarding no` and `PermitTTY no` for that user in
`/etc/ssh/sshd_config`.

### 3. Capture the host key

```bash
ssh-keyscan -t ed25519,rsa nas.example.com \
    > /etc/pg_hardstorage/keys/known_hosts
chmod 644 /etc/pg_hardstorage/keys/known_hosts
```

Verify the fingerprint out-of-band against the SFTP server's
`/etc/ssh/ssh_host_*_key.pub` files; an unverified `ssh-keyscan`
trusts whatever the network returned.

### 4. Initialise the repo

```bash
pg_hardstorage repo init sftp://backup@nas.example.com/srv/pg-hardstorage
```

```console
✓ Repository initialised
  URL:    sftp://backup@nas.example.com/srv/pg-hardstorage
  ID:     6a0d2f4b8c1e3057a9b2d4f6c8e0a1b3
  Schema: pg_hardstorage.repo.v1
  Created: 2026-07-06T13:56:58Z
```

The plugin reads `identity_file`, `identity_passphrase`, and
`known_hosts` from the deployment's `extras` map. Configure them
in `pg_hardstorage.yaml`:

```yaml
deployments:
  db1:
    repo: sftp://backup@nas.example.com/srv/pg-hardstorage
    extras:
      identity_file: /etc/pg_hardstorage/keys/sftp_id_ed25519
      known_hosts:   /etc/pg_hardstorage/keys/known_hosts
```

### 5. Verify

```bash
pg_hardstorage repo check sftp://backup@nas.example.com/srv/pg-hardstorage
```

## Why we refuse `StrictHostKeyChecking=no`

Silently trusting unknown host keys is the single most common
SFTP misconfiguration in audited environments — a network
attacker who can MITM the SSH handshake gets to MITM every
backup write and read. The plugin requires a real `known_hosts`
file. CI sandboxes that genuinely don't have a stable host key
should write a per-test `known_hosts` from the test harness.

## Atomicity model

SFTP exposes no native conditional-write primitive. The plugin
emulates `IfNotExists` with a stat → write-to-temp → rename
sequence. The TOCTOU window between the stat and the write is
inherent to SFTP (and the same shape `rsync` / `scp` ship with);
because chunks are content-addressed, a duplicate write is
harmless. Manifest commits go through `RenameIfNotExists` which
shares the same posture and is the actual race winner.

## Troubleshooting

**`extras.known_hosts is required`** — the plugin refuses to
proceed without one. See step 3.

**`ssh: handshake failed: ssh: unable to authenticate`** — the
key wasn't authorised on the remote side, or the passphrase is
wrong, or the remote `sshd` rejects ed25519 (very old hosts).
Try:

```bash
ssh -i /etc/pg_hardstorage/keys/sftp_id_ed25519 backup@nas.example.com
```

**`ssh: handshake failed: knownhosts: key mismatch`** — the
remote host key changed (legitimate re-key, or attack). Confirm
out-of-band, then update `known_hosts` deliberately.

**Permission denied on the path** — the backup user must own
the path and have `rwx` on it. Check `ls -ld /srv/pg-hardstorage`
on the remote.

## Next steps

- [Add a deployment](deployment.md) wired to this repo
- [Set retention](../operating/set-retention.md)
- [`repo init` CLI reference](../../reference/cli/pg_hardstorage_repo_init.md)

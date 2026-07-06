---
title: Install pg_hardstorage on Windows
description: Use the cross-compiled Windows binaries to run
  pg_hardstorage as a standalone CLI against a remote
  PostgreSQL.  Covers binary install, %APPDATA% layout,
  PATH wiring, and what's not yet supported.
tags:
  - windows
  - install
  - operations
---

# Install pg_hardstorage on Windows

> Status — **alpha**.  CI compile-tests every command for
> both `windows/amd64` and `windows/arm64` on every commit
> (the `build (windows/…)` gate), but only `windows/amd64`
> is shipped as a tagged release asset — `windows/arm64` is
> CI-verified, not released.  The agent's scheduled-job
> runner has no Windows Service integration yet, and the
> installer (`.msi`, signed bundle) is not yet packaged.
> This page covers the **manual binary install** that an
> operator can use today against a remote PostgreSQL.

## What works on Windows today

- ✓ The CLI: `backup`, `restore`, `verify`, `repo init`,
  `repo check`, `repo scrub`, `wal stream`,
  `deployment add/list/edit/remove/test`, `doctor`,
  `status`, `audit *`, `compliance *`, `kms *`,
  `llm *`, every read-only / one-shot operation.
- ✓ Repository sinks: `file://`, `s3://`, `gcs://`,
  `azure://`, `sftp://`.  All compile-time linked, no
  Windows-specific gating.
- ✓ The compat shims (`pg-hardstorage-pgbackrest.exe`,
  `pg-hardstorage-barman.exe`,
  `pg-hardstorage-barman-wal-archive.exe`,
  `pg-hardstorage-walg.exe`).  Drop into PATH; they
  dispatch to `pg_hardstorage.exe` the same way they
  dispatch to `pg_hardstorage` on Linux.
- ✓ The LLM helper (`pg_hardstorage llm chat / ask`).

## What's not yet wired

- ✗ **`pg_hardstorage agent`** — runs but has no
  Windows Service Control Manager integration; the
  process exits when the console session ends.  Use the
  Task Scheduler or a third-party wrapper (NSSM /
  WinSW) for unattended scheduling until the SCM
  integration lands.  The agent's signal handling
  (Ctrl-C / Ctrl-Break) works as expected interactively.
- ✗ **`.msi` installer** — currently you copy the `.exe`
  files into `C:\Program Files\pg_hardstorage\` (or
  anywhere on PATH) by hand.
- ✗ **Code signing** — the binaries CI publishes are
  unsigned.  SmartScreen will warn on first run; click
  "More info → Run anyway" or strip the
  Mark-of-the-Web with `Unblock-File` in PowerShell.
- ✗ **Tablespace-symlink restore** — pg_hardstorage
  doesn't write OS-level symlinks during restore on
  any platform; tablespace remapping happens via the
  manifest.  No Windows-specific blocker.

## Install

### 1. Download the binaries

There are two distinct sources, and they ship different
sets of binaries:

- **Per-commit CI artifact** — `pg_hardstorage-windows-<arch>-<sha>`
  is uploaded on every commit (7-day retention) for both
  `amd64` and `arm64`. This bundle contains seven `.exe`
  files:

  ```
  pg_hardstorage.exe
  pg_hardstorage_testkit.exe
  pg-hardstorage-pgbackrest.exe
  pg-hardstorage-barman.exe
  pg-hardstorage-barman-wal-archive.exe
  pg-hardstorage-walg.exe
  pg-hardstorage-compat.exe
  ```

- **Tagged release** — the GitHub release ships only
  `pg_hardstorage.exe`, and only for `windows/amd64`
  (goreleaser publishes no `windows/arm64` release asset and
  none of the shim/testkit `.exe` files). If you need the
  shims or testkit on Windows, grab the per-commit CI
  artifact or build them from source with `GOOS=windows`.

### 2. Place them on PATH

PowerShell (run as Administrator for a system-wide install):

```powershell
$dest = "C:\Program Files\pg_hardstorage"
New-Item -ItemType Directory -Force -Path $dest | Out-Null
Expand-Archive -Path .\pg_hardstorage-windows-amd64-*.zip -DestinationPath $dest -Force

# Add to PATH for every user.
$path = [Environment]::GetEnvironmentVariable("Path", "Machine")
if (-not ($path -split ";" -contains $dest)) {
    [Environment]::SetEnvironmentVariable("Path", "$path;$dest", "Machine")
}

# Mark each binary as trusted so SmartScreen doesn't warn
# on first run.  Skip this step if you want to verify
# the warning appears (good once-per-install paranoia).
Get-ChildItem $dest -Filter *.exe | Unblock-File
```

For a single-user install drop them under
`%LOCALAPPDATA%\Programs\pg_hardstorage\` and adjust
the user PATH instead of the machine PATH.

### 3. Verify

```powershell
pg_hardstorage version
pg_hardstorage doctor
```

`doctor` is the canonical first-look — it reports the
resolved config / state / cache / log paths, the
keyring location, and any environment misconfiguration.

## Where pg_hardstorage stores files on Windows

Following Microsoft's Known Folders guidance:

| Domain | User mode (default) | System mode (elevated / LocalSystem) |
|---|---|---|
| Config | `%APPDATA%\pg_hardstorage\` (roams) | `%PROGRAMDATA%\pg_hardstorage\config\` |
| State | `%LOCALAPPDATA%\pg_hardstorage\state\` | `%PROGRAMDATA%\pg_hardstorage\state\` |
| Cache | `%LOCALAPPDATA%\pg_hardstorage\cache\` | `%PROGRAMDATA%\pg_hardstorage\cache\` |
| Logs  | `%LOCALAPPDATA%\pg_hardstorage\logs\`  | `%PROGRAMDATA%\pg_hardstorage\logs\` |
| Runtime | `%LOCALAPPDATA%\pg_hardstorage\run\` | `%PROGRAMDATA%\pg_hardstorage\run\` |
| Shared data | `%PROGRAMDATA%\pg_hardstorage\` | `%PROGRAMDATA%\pg_hardstorage\share\` |

System mode is not selected by a flag — there is no
`--mode` switch. The resolver auto-detects it: an elevated
Administrator / LocalSystem context resolves to system mode,
the interactive user resolves to user mode.

Resolved paths show in `pg_hardstorage doctor` with
`source: windows` so it's unambiguous why the resolver
picked a particular directory.

The same precedence chain applies on Windows as on
Linux:

1. `--config-dir` / `--state-dir` / etc. CLI flags
2. `PG_HARDSTORAGE_CONFIG_DIR` (and friends) env vars
3. `PG_HARDSTORAGE_ROOT` single-tree override
4. **Windows Known Folders** (the table above)

So an operator who'd rather collapse everything under
one tree (e.g. `D:\pg_hardstorage\`) sets
`PG_HARDSTORAGE_ROOT=D:\pg_hardstorage` and gets the
same `etc/`, `var/lib/`, `var/cache/`, ... layout that
the Linux build would produce.

## Run as a Windows Service (interim recipe)

Until the native SCM integration lands, the agent can be
run unattended with [NSSM](https://nssm.cc/):

```powershell
nssm install pg_hardstorage_agent ^
    "C:\Program Files\pg_hardstorage\pg_hardstorage.exe" ^
    agent --config "C:\ProgramData\pg_hardstorage\config\pg_hardstorage.yaml"
nssm set pg_hardstorage_agent AppStdout "C:\ProgramData\pg_hardstorage\logs\agent.out.log"
nssm set pg_hardstorage_agent AppStderr "C:\ProgramData\pg_hardstorage\logs\agent.err.log"
nssm set pg_hardstorage_agent Start SERVICE_AUTO_START
nssm start pg_hardstorage_agent
```

Stop and remove with `nssm stop` + `nssm remove`.

## Known limitations

- **No FIPS variant.**  The `pkcs11` build tag links
  against libp11 which has no native Windows
  distribution.  HSM-backed keyrings work on Linux
  only.
- **No firecracker / KVM sandbox.**  Restore-side
  isolation under `/sandbox` is Linux-only by design;
  the verification step uses the in-process
  `pg_verifybackup` path on Windows.
- **Path validation.**  We don't yet validate Windows
  drive-letter / UNC-style paths in operator-supplied
  flags as carefully as we do POSIX paths.  Stick to
  forward-slash or doubled-backslash forms inside
  YAML config files.

## Roadmap

Tracked on the project [issues
list](https://github.com/cybertec-postgresql/pg_hardstorage/issues).
The remaining surface (signed `.msi` installer,
SCM-aware agent, automated `windows/amd64` test job in
CI) is sequenced after the v1.0 stabilisation work.

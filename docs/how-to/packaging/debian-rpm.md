---
title: Build .deb and .rpm packages
description: Run `dpkg-buildpackage` and `rpmbuild` against the
              shipped packaging skeletons.
tags:
  - packaging
  - debian
  - rpm
  - lintian
---

# Build .deb and .rpm packages

> Produce installable Debian and RPM packages from the
> shipped packaging skeletons under `debian/`. Multiple
> binary packages (`pg-hardstorage`, `pg-hardstorage-common`,
> `pg-hardstorage-server`) come out of one source build;
> `lintian` is treated as a hard gate.

## What you need

- A Debian-family build host (Debian stable, Ubuntu LTS) or
  a `sbuild` chroot for clean reproducible builds.
- Build dependencies: `debhelper-compat (= 13)`, `dh-golang`,
  `golang-go (>= 2:1.26~)`, `git`. Install with:

  ```bash
  sudo apt build-dep .
  ```

- For RPMs: `rpmbuild`, `rpmlint`, and a Fedora / RHEL host
  (or a `mock` chroot).

## Steps

### 1. Build the .deb (Debian)

From the repo root:

```bash
dpkg-buildpackage -us -uc
```

Artefacts land in the parent directory:

```text
../pg-hardstorage_<version>_<arch>.deb
../pg-hardstorage-common_<version>_all.deb
../pg-hardstorage-server_<version>_<arch>.deb
../pg-hardstorage_<version>_<arch>.changes
```

The build delegates to the project Makefile's `make build`
so the package's binary uses the exact same flags as a
from-source install (`debian/rules`'s
`override_dh_auto_build`).

### 2. Lintian gate

```bash
lintian -i -E -I ../pg-hardstorage_*.changes
```

Aim is fully clean. Documented exceptions live in
`debian/pg-hardstorage.lintian-overrides`; we do **not**
pre-suppress — every override carries a justification on
the same line.

### 3. piuparts (install / upgrade / purge cycle)

```bash
sudo piuparts --pedantic-purge ../pg-hardstorage_*.deb
```

`piuparts` smoke-tests:

- The package installs cleanly into a minimal chroot.
- `pg_hardstorage doctor` works after install.
- An upgrade from N-1 to N preserves state under
  `/var/lib/pg_hardstorage`.
- A `dpkg --purge` removes config and state cleanly.

### 4. RPM build (Fedora / RHEL)

```bash
rpmbuild -ba pg_hardstorage.spec
```

Equivalent gate: `rpmlint`. The spec mirrors the Debian
maintainer scripts (`%pre`, `%post`, `%preun`, `%postun`)
so behaviour is consistent across distros.

### 5. Fresh-VM smoke

The pre-release gate runs on a vanilla Debian VM:

```bash
apt install ./pg-hardstorage_*.deb
pg_hardstorage init --yes --pg-connection postgres://...
pg_hardstorage backup db1
pg_hardstorage restore db1 latest --target /tmp/restored
pg_hardstorage verify db1 latest --full
apt remove --purge pg-hardstorage
# assert no stray files outside /var/lib/pg_hardstorage
```

That sequence is what we run end-to-end before signing off
on a release. CI runs the same shape under sbuild.

## What just happened

`dpkg-buildpackage` invoked the rules file, which delegates
the actual compile to the project Makefile. `dh-golang`'s
buildsystem is hooked up via `dh $@ --with=systemd,golang
--buildsystem=golang` so dependency discovery and import-
path conventions follow the standard Go-on-Debian pattern.

The package layout follows the [Filesystem layout & OS
packaging](../../explanation/architecture-tour.md) section
of the SPEC: binary at `/usr/bin/pg_hardstorage`, systemd
units under `/lib/systemd/system/`, sysusers + tmpfiles
drop-ins for the `pgbackup` user / state dirs, completions
under `/usr/share/{bash-completion,zsh,fish}/`. Every path
the binary writes is allocated by sysusers.d / tmpfiles.d
on first boot — no postinst custom logic needed.

## Package layout

The single source produces these binary packages:

| Package | Architecture | Contains |
| --- | --- | --- |
| `pg-hardstorage` | any | binary, systemd units, completions, manpages |
| `pg-hardstorage-common` | all | shared data: runbooks, OpenAPI, JSON schemas |
| `pg-hardstorage-server` | any | stub for v0.5 control plane (registers dep on `pg-hardstorage`) |

Roadmap (per the SPEC):

| Package | Lands |
| --- | --- |
| `pg-hardstorage-fips` | v0.5 — Conflicts with `pg-hardstorage` |
| `pg-hardstorage-pg-ext-{15,16,17}` | v0.5 — archive_library `.so` per PG major |
| `pg-hardstorage-selinux` | RPM-side; SELinux policy split-out |

## Conformance gates

What we hold ourselves to:

- `lintian -i -E -I` clean (overrides only with justification).
- `dpkg --verify pg-hardstorage` clean after install.
- `piuparts --pedantic-purge` clean (install / upgrade / purge).
- `reprotest` (reproducible builds) green.
- Debian Policy Manual compliance.
- `apt full-upgrade` from N-1 to N preserves all state.
- `debconf` first-run guidance: postinst can prompt for
  `dpkg-reconfigure pg-hardstorage` to walk through
  `pg_hardstorage init`.

## Troubleshooting

### `dh-golang` not found

Older Debian (stable - 1) lacks the new dh-golang. Build on
a current Debian or pull the package from `backports`. The
v0.1 fallback path is goreleaser-produced `.deb` / `.rpm`,
which doesn't depend on dh-golang.

### `lintian` errors on `package-installs-into-obsolete-dir`

systemd unit files moved from `/lib/systemd/system/` to
`/usr/lib/systemd/system/` in modern Debian. The package
follows the new convention; `lintian` warnings here are
either expected (legacy distro) or a real misroute. Check
the install file before suppressing.

### Binary works in dpkg, fails under systemd

Likely a sysusers / tmpfiles drop-in that didn't fire.
Check:

```bash
systemd-sysusers --no-pager
systemd-tmpfiles --no-pager
```

The drop-ins ship as
`/usr/lib/sysusers.d/pg-hardstorage.conf` and
`/usr/lib/tmpfiles.d/pg-hardstorage.conf` and are applied
automatically on first boot. A package install won't apply
them retroactively without `systemctl daemon-reload` plus a
sysusers / tmpfiles rerun — `dh_installsystemd` handles
that for us.

### CI build is "dirty" but local is clean

`debian/rules` reads `dpkg-parsechangelog -SVersion` for
the version stamp; CI runs in a detached HEAD and `git
describe --dirty` adds a suffix when generated files
linger in the workspace. Run `make clean` before
`dpkg-buildpackage`, or override `VERSION=` explicitly.

## Next steps

- [Build from source](build-from-source.md) — what the
  package wraps.
- [FIPS variant](fips-variant.md) — produces the
  `pg-hardstorage-fips` package layout.
- Filesystem layout in the
  [Operator Guide](../../operations/operator-guide.md) —
  what's installed where, and why.

# debian

Debian source-package metadata. Feeds `dpkg-buildpackage` invoked from
`../dockerfiles/pkg-build/Dockerfile.deb-builder` to produce the `.deb`s shipped
for Debian, Ubuntu, and derivatives.

## What lives here

Standard debian/ layout: the `control` file declares binary packages and their
dependencies, `changelog` drives the upload version, `copyright` records the
Apache-2.0 license, and per-package `*.install` files list which built artefacts
end up in which `.deb`. Each binary package also has a `*.lintian-overrides` to
silence checks we have deliberately reviewed.

## Key files / subdirs

- `control` — binary-package definitions and dependency graph
- `changelog` — single source of truth for the package version emitted by the
  build
- `copyright` — Apache-2.0 attribution per the `machine-readable-copyright`
  spec
- `pg-hardstorage.install` — files shipped in the main `pg-hardstorage` binary
  package
- `pg-hardstorage-common.install` — shared sample config, completions, man
  pages
- `pg-hardstorage-compat-barman.install` — Barman shim binary + its
  README.Debian
- `pg-hardstorage-compat-pgbackrest.install` — pgBackRest shim payload
- `pg-hardstorage-compat-walg.install` — WAL-G shim payload
- `pg-hardstorage.dirs` — empty directories that must exist after install
  (`/var/lib/...`)
- `pg-hardstorage.manpages` — man-page glob included from `../man/man1/`
- `pg-hardstorage.bash-completion` — bash completion picked up from
  `../completions/bash/`
- `*.lintian-overrides` — reviewed-and-accepted lintian warnings per binary
  package
- `pg-hardstorage-compat-*.README.Debian` — user-facing notes on each shim's
  drop-in semantics

## Read next

- `../packaging/README.md` — RPM / Arch / FreeBSD counterparts to this
  directory
- `../dockerfiles/pkg-build/Dockerfile.deb-builder` — the build container that
  consumes this tree
- `../docs/how-to/packaging/` — release-engineering playbooks

## Don't put X here

- Build logic — that lives in `../Makefile` and the pkg-build Dockerfiles.
- Distro-specific patches for non-Debian targets — see `../packaging/`.

# packaging

Per-OS-family native-packaging specs that aren't Debian. Each subdirectory is
the upstream-format recipe a downstream packager hands to `makepkg` / `rpmbuild`
/ `make package` to produce a system-native package.

## What lives here

One subdirectory per packaging ecosystem. Recipes are kept minimal — they call
into the same artefacts the Debian build produces (binaries, man pages,
completions, sample config) and stage them under the layout each distro expects.
The matching builder containers live in `../dockerfiles/pkg-build/`.

## Key files / subdirs

- `arch/PKGBUILD` — Arch Linux / pacman recipe; consumed by
  `../dockerfiles/pkg-build/Dockerfile.arch-builder`
- `rpm/pg_hardstorage.spec` — RPM spec for both RHEL- and SUSE-family targets;
  consumed by `Dockerfile.rpm-rhel-builder` and `Dockerfile.rpm-suse-builder`
- `freebsd/Makefile` — FreeBSD ports `Makefile`
- `freebsd/pkg-descr` — FreeBSD ports long description

## Read next

- `../debian/README.md` — Debian counterpart (lives in its own top-level dir
  per convention)
- `../dockerfiles/pkg-build/` — the container images that exercise these
  recipes in CI
- `../dockerfiles/testbed/README.md` — distro-matrix testbeds that install the
  produced packages

## Don't put X here

- Debian metadata — that has its own top-level `../debian/` per dpkg
  convention.
- Container / Helm chart manifests — see `../charts/` and
  `../dockerfiles/k8s/`.
- Build-side tooling — the Makefile and pkg-build Dockerfiles own that.

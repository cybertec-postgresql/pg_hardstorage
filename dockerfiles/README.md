# dockerfiles

Build / test container recipes that aren't the canonical runtime image. Grouped
by purpose: Kubernetes operator shims, package builders, and the distro testbed
matrix.

## What lives here

Each subdirectory targets a different stage of the release pipeline. The shipped
runtime image lives in `../deploy/docker/`; this tree is for the supporting
infrastructure that surrounds it. Dockerfiles here are referenced from
`../Makefile` and `../.github/workflows/`.

## Key files / subdirs

- `k8s/` — operator-shim images that translate CNPG / Crunchy / Spilo argv
  conventions to our CLI
  - `Dockerfile.cnpg-shim`, `Dockerfile.crunchy-shim`, `Dockerfile.spilo-shim`
    — runtime shims
  - `Dockerfile.argv-recorder`, `Dockerfile.crunchy-argv-recorder`,
    `Dockerfile.spilo-argv-recorder` — record real operator argv so the shims
    can be regression-tested
  - `argv-recorder/` — Go source for the recorder helper
- `pkg-build/` — distro builder containers used by CI to produce `.deb` /
  `.rpm` / `.pkg.tar`
  - `Dockerfile.deb-builder`, `Dockerfile.arch-builder`,
    `Dockerfile.rpm-rhel-builder`, `Dockerfile.rpm-suse-builder`
- `testbed/` — distro-matrix images that exercise the produced packages plus
  PostgreSQL combinations
  - `Dockerfile.debian-family`, `Dockerfile.arch-family`,
    `Dockerfile.rhel-family`, `Dockerfile.suse-family` — install-and-smoke per
    family
  - `Dockerfile.multi-pg-l4` — single image with multiple PG majors for L4
    cross-version scenarios
  - `entrypoint-pg.sh`, `entrypoint-multi-pg.sh` — startup glue

## Read next

- `../deploy/docker/README.md` — the canonical runtime image (not in this
  tree)
- `../packaging/README.md` — the package recipes the `pkg-build/` images
  consume
- `../test/scenarios/README.md` — the L1–L8 tiers that schedule the
  `testbed/` images

## Don't put X here

- The shipped runtime image — that's `../deploy/docker/Dockerfile`.
- Dev-loop or one-off Dockerfiles — keep those out of the repo; they bloat CI
  cache invalidation.

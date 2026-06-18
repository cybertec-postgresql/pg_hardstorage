# deploy

Bare-metal and container deployment artefacts that are not OS packages. Use this
when you cannot install a `.deb` / `.rpm` and don't want Helm — e.g. plain
systemd hosts, docker-compose labs, goreleaser publishes.

## What lives here

Two flavours: container build recipes for the canonical published images, and
systemd units that the OS packages also install. Files here are also what a
from-source operator copies into `/etc/systemd/system/` when packaging is
bypassed.

## Key files / subdirs

- `docker/Dockerfile` — canonical multi-stage build for the published
  `pg-hardstorage` image
- `docker/Dockerfile.goreleaser` — slim variant consumed by goreleaser for
  tagged releases
- `docker/Dockerfile.testkit` — image bundling the testkit binary for CI /
  sandboxes
- `systemd/pg_hardstorage.service` — single-instance unit (server mode)
- `systemd/pg_hardstorage@.service` — templated unit for per-cluster agent
  instances
- `systemd/pg-hardstorage.sysusers.conf` — `systemd-sysusers` declaration of
  the `pg-hardstorage` system user
- `systemd/pg-hardstorage.tmpfiles.conf` — `systemd-tmpfiles` rules for state
  / runtime dirs
- `systemd/README.md` — operator-facing install notes for the unit files

## Read next

- `../charts/README.md` — Helm-based Kubernetes deployment alternative
- `../packaging/README.md` — packaged install path that wraps these same
  artefacts
- `../docs/how-to/operating/` — runbooks for production operation

## Don't put X here

- Kubernetes manifests — `../charts/` for Helm, `../test/k8s/` for test
  fixtures.
- Package-format recipes — `../debian/` and `../packaging/`.
- Distro-test containers — `../dockerfiles/testbed/`.

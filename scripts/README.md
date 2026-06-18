# scripts

Small shell helpers that are user-facing or release-time tooling, not part of
the build or test runners. If a script is invoked by `make`, it belongs in
`../Makefile`'s recipes or under a more specific tree, not here.

## What lives here

Standalone executables a contributor or operator runs by hand: the canonical
install script the website serves, the maintainer's local dev-cluster spinner,
the homebrew tap manifest, and the demo quickstart used in onboarding videos.
Keep each script self-documenting (`--help`) and POSIX-portable unless the
comment block declares otherwise.

## Key files / subdirs

- `install.sh` — the curlable installer published at the official download
  URL; resolves the latest release and drops the binary into `$PREFIX/bin`
- `demo-quickstart.sh` — five-minute end-to-end demo (local repo, backup,
  restore) used by `../docs/tutorials/getting-started.md`
- `devcluster.sh` — local Patroni + pg_hardstorage dev cluster for maintainers
- `homebrew-formula.json` — machine-generated tap manifest consumed by the
  Homebrew formula repo on release

## Read next

- `../docs/tutorials/getting-started.md` — narrative wrapping
  `demo-quickstart.sh`
- `../Makefile` — build / test entry points (not here)
- `../docs/how-to/packaging/` — release engineering procedures

## Don't put X here

- CI logic — belongs in `../.github/workflows/`.
- Build steps — belongs in `../Makefile`.
- Test runners — belongs in `../test/` or `../internal/testkit/`.

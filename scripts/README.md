# scripts

Small shell helpers that are user-facing or release-time tooling, not part of
the build or test runners. If a script is invoked by `make`, it belongs in
`../Makefile`'s recipes or under a more specific tree, not here.

## What lives here

Standalone executables a contributor or operator runs by hand: the canonical
install script the website serves, the maintainer's local dev-cluster spinner,
and the demo quickstart used in onboarding videos.
Keep each script self-documenting (`--help`) and POSIX-portable unless the
comment block declares otherwise.

Homebrew packaging is not here: the cask is generated and pushed to the tap
(cybertec-postgresql/homebrew-tap) by goreleaser on release — see the
`homebrew_casks` stanza in `../.goreleaser.yaml`.

## Key files / subdirs

- `install.sh` — the curlable installer served at <https://get.pghardstorage.org>
  resolves the latest release (or `--version <tag>`), downloads the matching
  goreleaser tarball, verifies its SHA-256 against `checksums.txt` (and the
  cosign signature when cosign is installed), then drops the binary into
  `$PREFIX/bin`. The serving endpoint is the Cloudflare Worker under
  `../deploy/cloudflare/`.
- `demo-quickstart.sh` — five-minute end-to-end demo (local repo, backup,
  restore) used by `../docs/tutorials/getting-started.md`
- `devcluster.sh` — local Patroni + pg_hardstorage dev cluster for maintainers

## Read next

- `../docs/tutorials/getting-started.md` — narrative wrapping
  `demo-quickstart.sh`
- `../deploy/cloudflare/` — the Worker that serves `install.sh` at
  `get.pghardstorage.org`
- `../Makefile` — build / test entry points (not here)
- `../docs/how-to/packaging/` — release engineering procedures

## Don't put X here

- CI logic — belongs in `../.github/workflows/`.
- Build steps — belongs in `../Makefile`.
- Test runners — belongs in `../test/` or `../internal/testkit/`.

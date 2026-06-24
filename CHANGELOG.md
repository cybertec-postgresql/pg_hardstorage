# Changelog

All notable changes to `pg_hardstorage` are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/) and the
project uses [Semantic Versioning](https://semver.org/).

`pg_hardstorage` commits to a 24-month backward-compatibility window on every
on-disk and on-the-wire schema (backup manifests, configuration, output JSON,
and the on-disk chunk envelope): an agent built against a given schema version
keeps reading that version for at least 24 months after a successor lands.

## [1.0.3] — 2026-06-24

### Documentation: correctness sweep + cloud-support accuracy

Audited the documentation against the codebase and corrected false or
stale claims. The big one: pg_hardstorage backs up self-managed
PostgreSQL over the physical replication protocol (`BASE_BACKUP` + a
physical slot); fully-managed DBaaS — Amazon RDS, Aurora, Cloud SQL,
Azure Database, Neon, Supabase — do **not** expose `BASE_BACKUP` and are
out of scope. Every "works on managed PG" claim was removed (web-verified
against each vendor's replication docs). Also fixed: feature counts (six
storage backends, one LLM provider), PG-version support (15–18; 15/16/17
CI-required, 18 allow-failure), nonexistent CLI flags in tutorials / ops
guides, broken in-repo file paths, stale version strings, and the
AES-256-GCM-SIV-vs-GCM and cosign-vs-Ed25519 descriptions. CNPG-I, Rekor
anchoring, skill signing, and the FIPS image are now clearly marked
roadmap. Download / verify examples use a `VERSION` variable so they no
longer go stale.

### Documentation: highlight encryption-key custody (#8)

The encryption tutorial and FAQ now state plainly where the local KEK
lives (`kek.bin` in the keyring directory), that losing it makes every
backup under it unrecoverable, that the keyring directory must be backed
up separately from the repository, and that `PG_HARDSTORAGE_KEYRING_DIR`
overrides its location (with `pg_hardstorage doctor` reporting the
resolved path). Also corrected a stale "GCP/Azure/Vault KMS slated for
v0.5+" note (those providers ship today).

### Packaging: wire container-image publishing (GHCR)

The release pipeline can now build and publish multi-arch (amd64/arm64)
distroless images to GHCR with keyless cosign image signatures. Publishing
is gated on the `PUBLISH_CONTAINERS` repo variable — set it once the org
enables Actions package-write on `ghcr.io`; until then the release ships
binaries / `.deb` / `.rpm` / Homebrew as before. Image-level SLSA
provenance remains roadmap. A `goreleaser check` step now validates the
release config in CI.

## [Unreleased]

### Fix: deployment-scoped commands now read the deployment config (#12)

`pg_hardstorage backup <deployment>` (and `restore`, `verify`, `list`,
`show`, `status`, `hold`, `rotate`, `recovery`, `repair`, `wal
preflight/stream/list/audit/prune/gaps`, `partial`, `kms verify/shred`,
…) used to demand `--pg-connection` / `--repo` even when the named
deployment already declared them in `pg_hardstorage.yaml`. They now
resolve those values from the deployment catalogue when the flags are
omitted (explicit flags still win); a deployment that isn't configured,
or a genuinely missing flag, still errors as before. Resolution happens
once, in a shared root pre-run hook, so every deployment-scoped command
behaves identically.

### Packaging: remove the obsolete homebrew-formula.json manifest

Dropped `scripts/homebrew-formula.json`, a leftover hand-maintained tap
manifest that nothing consumes: the Homebrew artefact is generated and
pushed to the tap by goreleaser on release. Updated `scripts/README.md`
accordingly.

### Packaging: publish a Homebrew cask on release

goreleaser now generates and pushes a Homebrew cask to the org-wide tap
(cybertec-postgresql/homebrew-tap) on each release, so
`brew install cybertec-postgresql/tap/pg_hardstorage` works on macOS
(Apple Silicon) and Linux (amd64/arm64). A cask (not a formula) is used
because goreleaser deprecated the formula pipe in v2.16. The macOS path
strips the Gatekeeper quarantine xattr on install, since the binaries
are cosign-signed but not Apple-notarised. No hard PostgreSQL dependency:
the agent talks to PostgreSQL over the replication protocol, so the
optional psql client is surfaced as a caveat instead. The push uses a
dedicated HOMEBREW_TAP_TOKEN secret.

### Installer: fix and harden the curl|sh installer

The `scripts/install.sh` one-liner now works against real releases: it
builds the versioned goreleaser archive name, resolves `latest` via the
GitHub release redirect, and parses `--version`/`--bindir`/`--no-verify`
flags correctly (previously `latest` and the unversioned archive name
both 404'd, and `--version` was mis-read). The script is strict POSIX
`sh` so the canonical `curl | sh` works under dash/busybox without a
bash re-exec. Downloads are verified by SHA-256 against `checksums.txt`,
and by cosign signature when cosign is installed. Added a Cloudflare
Worker (`deploy/cloudflare/`) to serve the script at get.pghardstorage.org.

### Docs: brand the documentation site

The documentation site now matches the pghardstorage.org brand: the
website's navy + cyan palette (light and dark schemes), the wordmark in
the header and a light/dark home-page hero, favicon, typography tuning,
a branded footer with CYBERTEC links, and a right-hand mobile navigation
drawer. The home-page title was de-duplicated and made SEO-friendly, and
Open Graph + Twitter Card meta tags were added for social share previews.
All assets are repo-local (air-gapped posture); no new build dependencies.

### Docs: publish the documentation site to GitHub Pages

The docs CI built and validated the site but never published it. A
push-on-main-gated deploy job now publishes it to GitHub Pages at
docs.pghardstorage.org. PRs continue to only build + preview.

### Added

- Initial public release.

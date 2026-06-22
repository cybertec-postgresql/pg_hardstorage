# Changelog

All notable changes to `pg_hardstorage` are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/) and the
project uses [Semantic Versioning](https://semver.org/).

`pg_hardstorage` commits to a 24-month backward-compatibility window on every
on-disk and on-the-wire schema (backup manifests, configuration, output JSON,
and the on-disk chunk envelope): an agent built against a given schema version
keeps reading that version for at least 24 months after a successor lands.

## [Unreleased]

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

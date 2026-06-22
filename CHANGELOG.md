# Changelog

All notable changes to `pg_hardstorage` are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/) and the
project uses [Semantic Versioning](https://semver.org/).

`pg_hardstorage` commits to a 24-month backward-compatibility window on every
on-disk and on-the-wire schema (backup manifests, configuration, output JSON,
and the on-disk chunk envelope): an agent built against a given schema version
keeps reading that version for at least 24 months after a successor lands.

## [Unreleased]

### Packaging: publish a Homebrew formula on release

goreleaser now generates and pushes a Homebrew formula to the org-wide
tap (cybertec-postgresql/homebrew-tap) on each release, so
`brew install cybertec-postgresql/tap/pg_hardstorage` works on macOS
(Apple Silicon) and Linux (amd64/arm64). No hard PostgreSQL dependency:
the agent talks to PostgreSQL over the replication protocol, so the
optional psql client is surfaced as a caveat instead. The formula push
uses a dedicated HOMEBREW_TAP_TOKEN secret.

### Docs: publish the documentation site to GitHub Pages

The docs CI built and validated the site but never published it. A
push-on-main-gated deploy job now publishes it to GitHub Pages at
docs.pghardstorage.org. PRs continue to only build + preview.

### Added

- Initial public release.

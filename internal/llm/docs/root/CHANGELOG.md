# Changelog

All notable changes to `pg_hardstorage` are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/) and the
project uses [Semantic Versioning](https://semver.org/).

`pg_hardstorage` commits to a 24-month backward-compatibility window on every
on-disk and on-the-wire schema (backup manifests, configuration, output JSON,
and the on-disk chunk envelope): an agent built against a given schema version
keeps reading that version for at least 24 months after a successor lands.

## [Unreleased]

### Added

- Initial public release.

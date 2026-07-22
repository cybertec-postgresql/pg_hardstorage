# Changelog

All notable changes to `pg_hardstorage` are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/) and the
project uses [Semantic Versioning](https://semver.org/).

`pg_hardstorage` commits to a 24-month backward-compatibility window on every
on-disk and on-the-wire schema (backup manifests, configuration, output JSON,
and the on-disk chunk envelope): an agent built against a given schema version
keeps reading that version for at least 24 months after a successor lands.

## [Unreleased]

### Fix: intermittently unrestorable encrypted backups under concurrent WAL streaming (#31)

With `wal stream` running, a concurrently-taken base `backup` could
commit a manifest that was silently **unrestorable** — `verify` reported
mass chunk-integrity failures and `restore` failed with
`encryption: unknown algorithm: 1`, even though `backup` exited 0.

Root cause was a check-then-act race in the shared-DEK coordination. The
CAS deduplicates chunks by plaintext hash, so every encrypted artifact
under one KEK must share one DEK. Resolution only scanned *committed*
manifests, so two writers that both started before either committed each
minted a **different** DEK. A PostgreSQL full-page image in WAL that
chunked to the same bytes as a base-backup file then deduped to one CAS
slot, stored under one writer's DEK while the other's manifest referenced
it under the other DEK — undecryptable.

The DEK is now minted through an **atomic single-winner PUT** on a
well-known shared-DEK object (`keys/shared-dek/<kekref-hash>.json`): the
first writer wins, every concurrent writer reads back and reuses the
winner's DEK, so streaming and base backups always converge on one DEK.
Existing repos are seeded transparently from their manifests on first
write. Covered by a 24-way concurrent regression test and validated
end-to-end (streaming + racing backups → all verify + restore cleanly;
exactly one shared-DEK object).

## [1.0.12] — 2026-07-16

### Docs: remove false-capability claims (managed DBaaS + unshipped features)

An audit for the "documents a capability that doesn't actually work"
class of bug, prompted by finding that several places claimed support
for fully-managed DBaaS.

- **Managed DBaaS**: the LLM-embedded README, SPEC, and the Kubernetes
  sidecar chart (Chart.yaml + README) stated or implied pg_hardstorage
  works against Amazon RDS/Aurora, GCP Cloud SQL, Azure Database, and
  similar — while the rest of the docs correctly explain it cannot:
  managed services do not expose `BASE_BACKUP` / physical replication
  to customers. All corrected to the accurate "self-managed PostgreSQL
  only" framing. The replication-protocol data plane removes the
  *host-access* barrier — not the `BASE_BACKUP` barrier.
- **Rekor**: a `TransparencyLog` code comment claimed a `rekor.Log`
  implementation ships; only the self-hosted `StorageBackedLog` exists.
  External Rekor is post-v1.0 roadmap (now stated as such).
- **PCI-DSS evidence bundle**: the QSA runbook instructed verifying an
  image-level SLSA attestation that isn't produced (container image
  unpublished; image SLSA is roadmap). Added the caveat and a working
  blob/tarball `slsa-verifier` alternative.
- **FIPS artifact**: build-flavours described an "official
  pg-hardstorage-fips distribution artifact… out of the box"; no such
  artifact ships (it's roadmap). Reworded to build-from-source + a
  planned-artifact note.
- **SPEC packaging**: Scoop and the `-fips`/`-pg-ext` container image
  variants were listed as shipped; marked planned/gated.

No behaviour change.

## [1.0.11] — 2026-07-16

Twelve operator-inconvenience fixes found by exercising the CLI surface,
each covered by a regression test.

### Fix: false alarms and silent wrong-target

- `repo scrub` reported 100% chunk corruption (exit 9) on every
  ENCRYPTED repository — the default posture after `init` — because it
  built a CAS with no decryptor. It now scrubs manifest-aware (the same
  per-manifest CAS `repair scrub` uses), so encrypted chunks decrypt
  and verify. Scheduled scrubs no longer page on every run.
- The global `-c`/`--config` flag was advertised everywhere but read
  nowhere; the tool always loaded the XDG/FHS default. It is now honored
  for both reads and write-back, so `-c staging.yaml` operates on that
  file.
- `lint` always returned `{"status":"valid"}` without reading anything.
  It now validates the resolved config with the real loader (strict
  KnownFields + validation) and fails, with the reason, on a broken one.

### Fix: dry-run / advisory tools no longer give false confidence

- `recovery windows` advertised a PITR range straight across a WAL
  archive hole; it now caps `latest_restore_lsn` at the first hole and
  records the gap.
- `restore --preview` reported "Pre-flight: ✓ ready" for a target past a
  WAL hole that the real restore warns will HALT recovery; preview now
  surfaces the same `wal_archive_hole` finding.
- `capacity report` extrapolated a seconds-long sampling window into
  absurd per-day growth labeled "medium confidence"; confidence now
  requires a real observation window (≥1 day for medium, ≥1 week for
  high) and a sub-day window carries an explicit caveat.
- `rotate` stamped legally-HELD backups `[del ]` in its per-backup
  listing while the summary said `held: N (excluded from delete)`; held
  backups now render `[held]`.

### Fix: wrong error class, hollow stubs, muscle memory

- `recovery readiness` printed the RTO throughput as a nonsensical
  duration (`46603h22m40s`) instead of a byte-rate (`160.0 MiB/s`).
- `--incremental-from` against a PostgreSQL < 17 server was reported as
  the generic `internal` (file-a-bug) code; it is now the structured
  `backup.incremental_unsupported` usage error with a hint.
- `repo init` accepts the repository URL via `--repo` (matching every
  other `repo` verb), not only as a bare positional.
- `explain <command>` now returns the command's real summary, usage, and
  description instead of echoing the argument back.
- `glossary <term>` now returns the term's definition (an unknown term
  is `notfound.term`) instead of dropping the description.

Also: the renderer integer-fidelity fix (YAML/CSV scientific-notation)
was extended to a shared `jsonshape` helper covering tap/junit/pdf/
template.

## [1.0.10] — 2026-07-15

### Fix: `recovery drill` failed every WAL-streaming backup (#26)

The verify sandbox that `recovery drill` and `verify --full` use ran
`pg_verifybackup` without `-n`/`--no-parse-wal`. pg_hardstorage stores
WAL in the repository rather than inside the base backup, so the
restored data directory legitimately has an empty `pg_wal/` — the
WAL-parse step therefore failed every structurally-valid WAL-streaming
backup with `could not find any WAL file`, and the drill reported
`verdict: fail` for backups that restore, recover, and serve data
correctly. The sandbox now verifies the manifest and file checksums
with `-n`, matching the restore path's `--verify` gate (the same
defect was fixed there in 1.0.8).

### Fix: `hold remove` reported success for holds that don't exist

Releasing a hold with a typo'd backup ID printed `✓ Hold released`
and exited 0 while the real hold silently kept blocking retention — a
false success on the legal-hold path. Removal of a nonexistent hold
now fails with `notfound.hold` (exit 6) and points at `hold list`;
releasing an existing hold is unchanged.

### Fix: JSON output shape papercuts

- `backup compare -o json` double-nested its payload under
  `.result.result.*`; the comparison fields now sit at `.result.*`
  like every other command.
- `list` on a deployment with no backups emitted `"backups": null`;
  it now emits `[]`, so `jq '.result.backups[]'` and every other
  iterator handle the empty case.

## [1.0.9] — 2026-07-13

Twenty operator-annoyance fixes, found by systematically exercising the
user-facing surface (first-run flows, error hints, exit codes, output
consistency) and each covered by a regression test.

### Fix: first-run experience

- `init` no longer busy-loops forever (flooding the terminal) when
  stdin is closed — a CI pipe or Ctrl-D now aborts with a structured
  error pointing at flags + `--yes`.
- `init --quick` defaults to a user-writable repository path for
  non-root users instead of failing on `/var/backups/pg_hardstorage`
  with a permission error.
- init's "Next steps" suggests the flagless `wal stream <deployment>`
  (the config it just wrote makes it work) instead of a literal
  `--pg-connection ...` placeholder that retried an unparseable DSN
  forever; operator-input (`usage.*`) errors now fail the stream
  setup fast instead of retrying.
- Ctrl-C / SIGTERM now cancel the command context so deferred cleanup
  runs — interrupting `demo` no longer leaks its throwaway PostgreSQL
  container.

### Fix: hints and error classification

- Every remediation hint is copy-pasteable: doctor's audit-anchor
  hints include `--repo <url>`; the checkpoint-mismatch suggestion
  gives the resume command (it previously steered operators — and
  automation reading its `command` field — toward `rm -rf` of the
  partially-restored target, and referenced a flag that doesn't
  exist); the GDPR erasure report and `jit` help no longer recommend
  the nonexistent `kms shred --tenant`; the plain-restore notice
  names the real `--to` flag.
- A typo'd subcommand under a group (`wal audi`, `repo bogus`) now
  fails with exit 2 and a "did you mean" instead of printing help and
  exiting 0 (a cron job with a typo stayed green forever); unknown
  top-level commands also exit 2.
- An empty backup-ID argument (unset shell variable) is a usage error
  (exit 2) — `verify` previously reported it as a manifest SIGNATURE
  failure (exit 9, the pager-worthy "corrupt/tampered" code).
- A typo'd deployment name yields `notfound.deployment` listing the
  configured names instead of demanding `--pg-connection`/`--repo`.

### Fix: consistency and safety

- `--version` works (CLI muscle memory); the help banner no longer
  claims "v0.2"; `changelog` reports the real binary version.
- `daily_at` schedules are documented as host-local time (they always
  were) and the schedule display shows the actual zone + UTC offset.
- `backup delete` — the most destructive verb — now requires `--yes`
  (or an approval), matching every other gated verb.
- Bare `status` / `rotate` / `audit anchor` resolve `--repo` from the
  config when every deployment shares one repository.
- `--verify` and `--verify-restore` accept each other's vocabulary
  (`skip`≡`off`, `require`≡`required`).
- Durations render as `N ms` everywhere (list/init/verify matched
  show/backup/restore); the `status` tombstone footnote no longer
  blows the table fifty columns wide.
- The generated `restore_command` runs `wal fetch` with `-o text -q`,
  so routine end-of-WAL probes log one line instead of ten-line JSON
  documents in the PostgreSQL server log.

## [1.0.8] — 2026-07-06

### Fix: post-restore verification failed on every base-only restore

The `restore --verify` gate ran `pg_verifybackup` without `-n`, so it
tried to parse WAL. A pg_hardstorage restore lays down the base backup
only — the WAL needed to reach consistency is fetched at recovery time
via the `restore_command` — so the restored data directory has no
`pg_wal` segments yet, and `pg_verifybackup` failed every normal restore
with `could not find any WAL file`, reporting `Verification: failed`. It
now passes `-n` (`--no-parse-wal`), verifying the manifest and file
checksums; a clean restore reports `Verification: passed`.

### Fix: the interactive `simple` helper accepted the wrong repo schemes

`pg_hardstorage_simple` validated `gs://` and `azure://` — schemes with
no registered backend — and rejected the real `gcs://` and `azblob://`.
It now accepts exactly the schemes the storage registry provides
(`file` / `s3` / `gcs` / `azblob` / `sftp` / `scp`).

### Documentation: full accuracy pass against the code

Validated the README and the entire documentation tree against the
shipped binary — capturing real command output where examples are shown
— and corrected everything that did not match: nonexistent commands and
flags, wrong error codes and config keys, stale "roadmap/v0.5" framing
for shipped features, an incorrect Tier-2 plugin-protocol description
(the shipped transport is stdio JSON-RPC), unpublished-artifact
references, and roughly thirty fabricated sample-output blocks. Link
integrity was verified (no dead links).

## [1.0.7] — 2026-07-02

A broad code-review pass fixed 79 correctness bugs across the codebase,
each with a regression test. The whole suite — unit, race, integration
(against real PostgreSQL), and the Patroni failover / data-integrity
lane — is green. Highlights, grouped by blast radius:

### Fix: data-integrity and durability

- Restore placed non-default tablespace contents under the data
  directory root instead of their real tablespace location (while
  `tablespace_map` pointed at an empty directory). Files now carry
  their owning tablespace and restore to the correct path.
- The local-filesystem barrier could, on a retried commit after an
  fsync error, drop already-staged chunks — leaving a committed
  manifest that referenced objects never published (an unrestorable
  backup). Retries now preserve every staged write.
- The Azure backend's rename deleted the source before its async copy
  completed, so a manifest commit could report success with the
  destination absent; it now waits for copy completion.
- Air-gap bundle import now verifies each chunk's SHA-256 against its
  content-addressed key, so a corrupt or tampered bundle can't plant a
  wrong-content chunk that later backups dedup against.
- A WAL slot that Patroni re-created at promotion, ahead of the agent's
  last archived byte, silently masked a real WAL hole; the gap is now
  detected and surfaced so restore pre-flight can refuse a PITR into
  the missing range.

### Fix: security and privacy

- The PKCS#11 KMS reference stamped into every manifest could carry an
  inline HSM PIN in cleartext; the PIN is now stripped from the
  persisted reference.
- `llm ask` / `llm explain` silently ignored a configured `strict` /
  `local-only` privacy mode, and the chat privacy gate ignored an
  endpoint set via environment — either could let a local-only session
  reach a public endpoint. Both now enforce the resolved endpoint.
- Chain-restore staging moved off a predictable, world-writable temp
  path to a private per-restore directory.

### Fix: retention, holds, and the control plane

- Concurrent retention sweeps could orphan a live backup chain or
  defeat a legal hold placed mid-sweep; both delete paths now re-check
  and roll back.
- Agents advertised only `backup`, so restore and verify jobs enqueued
  through the control plane sat queued forever; agents now claim every
  job kind they can execute, and job execution no longer blocks
  heartbeats.

### Fix: compatibility shims

- The Barman, WAL-G, and pgBackRest compatibility layers emitted
  command-line arguments and generated configuration the native CLI
  rejected; the affected `recover` / `check` / `backup-fetch` /
  recovery-target / config-translation paths now work.

### Fix: reporting

- `duration_ms` fields in backup, restore, gameday, and verification
  JSON emitted nanoseconds under a millisecond key (values inflated a
  million-fold); they now emit milliseconds. The JSON keys are
  unchanged.

Also fixed: numerous CLI verb correctness issues (`repo scrub` /
`repo gc` / `repo check` / `status` / `doctor` / `repair` / `audit` /
`list` / `logs`), storage-backend listing/temp-file hygiene, logical-
receiver shutdown and flush correctness, and post-restore verification
cleanup. See the commit history for the full itemised list.

## [1.0.6] — 2026-06-27

### Fix: backups with a non-default tablespace (#17)

A backup of a cluster that has any user tablespace failed to commit with
`backup.manifest_invalid: backup_label is empty (required for restore)`.
PG streams the base/default tablespace archive — the one carrying
`backup_label` and `tablespace_map` — *last* when user tablespaces exist,
but the tar sink only looked for those files in the first archive. It now
captures them from whichever archive holds them, so multi-tablespace
clusters back up (and restore) correctly.

### Fix: `pg_hardstorage demo` now actually runs (#15)

The `demo` command previously printed a one-line description and exited
without doing anything. It now runs the real end-to-end flow — start a
throwaway PostgreSQL in Docker, initialise a repo, back up, restore, and
verify, then clean up — driving your `docker` CLI so a non-default daemon
set via `DOCKER_HOST` (Lima, Colima, Podman) is honoured, and reporting a
clear error if Docker isn't reachable instead of silently succeeding.

## [1.0.5] — 2026-06-26

### Docs: refine product messaging and positioning

More precise product messaging and positioning across the documentation
and the project spec. Wording-only; no code, CLI/API, or on-disk schema
changes.

## [1.0.4] — 2026-06-24

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

## [1.0.1] — 2026-06-23

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

## [1.0.0] — 2026-06-18

### Added

- Initial public release.

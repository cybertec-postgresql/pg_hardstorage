# Changelog

All notable changes to `pg_hardstorage` are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/) and the
project uses [Semantic Versioning](https://semver.org/).

`pg_hardstorage` commits to a 24-month backward-compatibility window on every
on-disk and on-the-wire schema (backup manifests, configuration, output JSON,
and the on-disk chunk envelope): an agent built against a given schema version
keeps reading that version for at least 24 months after a successor lands.

## [Unreleased]

### Security

- **`audit verify-bundle` now binds the recorded signer to the key that
  validates the bundle.** An audit-evidence bundle carries the events, a
  detached signature, and the public key in the same tarball, so the
  signature alone only ever proved "signed by whoever ships inside this
  file". An attacker who rewrote the events could re-sign them under their
  own key, drop in their own `public_key.pem`, and leave the operator's
  `public_key_fingerprint` untouched in `bundle.json` — the bundle verified
  and the identity an auditor reads was the victim's. `VerifyBundle` now
  refuses a bundle whose bundled key does not hash to the fingerprint the
  manifest records. Honest bundles are unaffected; a bundle produced before
  this change still verifies, because the exporter has always written the
  matching fingerprint.

- **A non-loopback control plane now refuses to start without
  authentication.** `requireAuth` skipped bearer-token checking whenever no
  token was configured — documented as "intended for behind-mTLS
  deployments", but nothing enforced it. A server could therefore bind a
  non-loopback address with plain TLS, no client-certificate verification
  and no token, exposing deployment enumeration, job enqueue, and
  `POST /v1/deployments/<n>/restores` with an arbitrary absolute
  `target_dir` to anyone who could reach the port. A non-loopback `listen`
  now requires either `auth.token_file` or `tls.client_ca_file`, checked in
  the constructor so a misconfiguration fails at startup rather than on the
  first request. Loopback is unchanged, so the out-of-box local CLI posture
  still works. **Operators running a server-TLS-only, non-loopback
  deployment must add one of the two before upgrading.**
- **A job-supplied repo URL is only trusted against the agent's declared
  repo.** All three executors fell back to the job's URL when the deployment
  had none, and applied the cross-repo guard only when the deployment
  declared one. A `wal stream`/doctor-only deployment therefore accepted any
  control-plane-supplied repo: `{"repo":"sftp://attacker/loot"}` made the
  agent take a fresh physical base backup and stream the live cluster into
  it, with no local warning. The agent now refuses a job-supplied repo when
  it has none configured, and the server rejects an off-allowlist repo with
  `400 usage.bad_repo`.
- **Repository credentials are redacted in deployment log lines.**
- **Deployment names and backup IDs are validated at the REST boundary**
  (F-0004), so a traversal name gets a clean `400` instead of a silent
  `200` plus an empty list, and a hostile backup ID fails at the API rather
  than deep inside the agent executor.
- **`scp` path containment is checked by resolution, not by banning the
  `..` substring** (F-0002). The old ban was escape-proof but over-strict:
  `validateStorageID` permits dots in deployment names, so a deployment
  called e.g. `db..prod` produced legal keys that `List`/`Delete` refused,
  silently breaking every `scp://` repo for that deployment.
- **Toolchain pinned to go1.26.6**, and `otel` → v1.44.0 / `x/net` →
  v0.56.0.

### Fixed

- **The audit-chain append loop can no longer spin forever.** `Store.Append`
  claims its sequence slot with a conditional put and relinks onto the
  winner when it loses the race. If a slot's stored event body reported a
  sequence at or below the one just attempted — a hand-edited event, a
  half-migrated legacy chain, an object restored into the wrong slot — the
  relink sent the loop back to the same occupied slot indefinitely, in a
  goroutine that held no lock and ignored cancellation. The loop now forces
  the sequence strictly forward, honours context cancellation on every
  iteration, and gives up after 1024 collisions with a diagnosable error.
- **Auxiliary WAL archive files (`.history`, `.backup`) get the same
  split-brain check segment manifests already get.** `PushAuxiliaryFile`
  treated "an object already exists at this key" as an idempotent
  `archive_command` retry without comparing content. Two clusters sharing a
  deployment name (a cloned datadir, a restored copy still archiving) write
  the same key with different bodies, so PG was told "archived" while the
  repo held the other cluster's timeline history — invisible until a restore
  read the wrong parent timeline. Conflicting content now fails with
  `splitbrain.content_mismatch`; identical bytes remain an idempotent
  success.
- **Integrity runs hold one referrer entry per backup, not per chunk
  reference.** The fleet-wide chunk→referrers map appended a backup ID for
  every chunk *occurrence*, so a manifest referencing one chunk thousands of
  times (a repeated all-zeroes page) paid thousands of entries for a single
  name — across every chunk, for the whole walk — only for the report to
  collapse them. Reported output is unchanged.

- **`repo bundle import` rejected every chunk of every real bundle.** A
  chunk key addresses the PLAINTEXT — `PutChunk` hashes the body, then
  compresses (zstd by default) and optionally encrypts before storing —
  while a bundle preserves the on-disk layout and therefore carries the
  STORED bytes. Import hashed what was in the tar and compared it to the
  key, which only agrees when the chunk was written with the `none` codec.
  Since every repository compresses by default, the air-gap bundle feature
  did not work at all. Import now decodes the envelope and verifies what the
  key actually addresses; a chunk whose plaintext does not hash to its key
  is still refused, so the anti-forgery guarantee is unchanged.
- **A backup could adopt a chunk it cannot decrypt in a multi-KEK
  repository.** The cross-DEK adopt guard probed once per CAS, on the
  reasoning that readability is a property of `(repo, DEK)`. That holds for
  a single-KEK repository and is false for a mixed one — which is exactly
  what the guard defends, since per-tenant KEKs sharing a repo is a
  supported configuration. A backup that adopted one of its own chunks first
  resolved the guard OK, and every later adoption of another tenant's chunk
  went unchecked: the manifest committed references to chunks the backup
  cannot read, so it exited 0 and failed only at restore. The guard now
  verifies the premise — one listing of the shared-DEK prefix — and checks
  every adoption in a multi-KEK repo, leaving the single-KEK fast path and
  its cost profile unchanged.
- **GCS answered "object missing" only at open, never mid-stream.** The
  plugin mapped `ErrObjectNotExist` to `storage.ErrNotFound` when opening a
  reader, but the client can also report it from the BODY read when an
  object is deleted between open and pull. The raw client error then escaped
  the plugin and every caller keyed on `ErrNotFound` stopped recognising a
  plain missing object — including `Lease.Acquire`, whose
  released-between-put-and-read retry never fired on GCS, turning a routine
  concurrent lease release into a failed backup.
- **`logs --since 24h` was rejected by journalctl.** The flag is documented
  as `DUR-OR-TS` with `"24h"` as its first example, and the value was passed
  through untouched; systemd requires a sign on a relative time, so the most
  obvious way to use the flag failed with a generic `internal` error. A bare
  duration is now negated ("that long ago"); every other spelling
  (`yesterday`, `1 hour ago`, absolute timestamps, already-signed values)
  passes through unchanged.
- **`backup` no longer panics when the wall clock steps backwards.** A
  backward NTP step or VM suspend/resume between two lease renewals made
  `now+ttl` fall at or before the stored expiry, tripping an invariant that
  killed the agent mid-backup. A renewal that cannot extend now keeps
  holding without writing — the body would be byte-identical, so no
  mutual-exclusion window narrows.
- **A backup can no longer start from LSN 0 or an empty recptr.** Two
  independent paths could produce a start LSN of zero when the LSN query
  failed on a fresh no-slot start, or when `BASE_BACKUP` returned an empty
  `recptr` (CORRUPT-1, CORRUPT-2). Both are refused.
- **`.history` / `.backup` archive files get the same split-brain check
  segment manifests already get**, so two clusters sharing a deployment name
  can no longer overwrite each other's timeline history invisibly.
- **Leaks:** an interrupted base backup tears down its open tablespace
  (previously leaking the sink's parser goroutine and its pipe for the
  process lifetime); the CAS adoption set is released after segment commit,
  so a long-lived `wal stream` no longer grows it without bound; stale
  agent-registry entries are pruned on heartbeat.

### Added

- **`kms verify --require-encrypted`.** For a fleet whose policy is
  "everything is encrypted", a manifest that lost its encryption block is
  the event the audit exists to catch; the default posture counts it as an
  operator policy choice and exits 0. With the flag, unencrypted manifests
  are listed in `failures` and the command exits 9. The result body records
  the policy it was judged under.

### Changed

- **Documentation: the audit log is a linear hash chain, not a Merkle
  tree.** SPEC, glossary, compliance mappings, and the explanation pages
  described the chain as "Merkle" and its export as a "Merkle proof". The
  implementation stores each event's predecessor hash the way git stores
  commits, and `audit export` ships a chain proof (head pointer plus the
  window's edge hashes), not an inclusion proof against a tree root. The
  tamper-evidence property is unchanged; what a third-party verifier
  recomputes is not.
- **Documentation: transparency-log anchoring and KMS provider coverage now
  match the runtime.** The shipped transparency log is the self-hosted,
  storage-backed one; external Rekor anchoring stays roadmap and is no
  longer described as a live cadence. `SECURITY.md` lists the KMS providers
  actually registered (AWS, GCP, Azure Key Vault, Vault Transit, PKCS#11,
  plus the local keyring) and states plainly that TPM-backed custody is not
  implemented.

- **`logs` on a unit systemd does not know now exits 6 (`notfound.unit`)
  instead of 0.** The previous detection keyed on journalctl exiting 1 for
  "no entries", which real journalctl does not do — it exits 0 both for a
  quiet unit and for one that does not exist, so the not-found branch was
  unreachable and `logs --unit no-such-unit` returned success with no lines.
  Existence is now decided by systemd (`LoadState`), and a unit that exists
  but is merely quiet is still not an error. **Anything scripted against the
  old exit 0 will see the change.**
- **Performance.** WAL pruning reads manifests only for the prunable prefix
  instead of every segment (binary search over ordering; over-pruning is
  impossible by construction). The chunker's hot path no longer copies the
  live tail after each chunk — a 4 MiB stream allocates ≤4 objects instead
  of 285, with bit-identical boundaries. The adopted-chunk commit gate fans
  out, the DEK-reuse manifest read is bounded, and `/readyz` caches its
  probe for 30 s.
- **CI workflows fire only on a `v*` tag push or manual dispatch**;
  per-PR/per-push triggers and cron schedules were removed. The recommended
  developer flow is `make ci` locally before tagging.
- **New developer targets:** `make test-scenarios` runs the whole
  174-scenario corpus (the previous target ran 12 of them, so the other 162
  were wired into nothing and had drifted); `make test-scenarios-lint`
  schema-checks all of them without Docker; `make test-stress` re-runs the
  ordering-sensitive packages without `-race`; every test target now prints
  the architecture it ran on.

## [1.2.4] — 2026-08-19

### Fixed

- **Numeric CLI options reject non-finite (`NaN` / `Inf`) values.** Go's
  flag parsing accepts `"NaN"`, `"Inf"`, and `"+Inf"` as valid `float64`s,
  so a numeric option could arrive non-finite and slip past a plain
  `x <= 0` / `x < 0` bound (every comparison with `NaN` is false, and `Inf`
  passes a lower bound), then feed a nonsensical multiplier or price into a
  gate. `--capacity-safety-factor`, `--safety-factor`, `--price-per-gb-month`,
  `--threshold`, `--spike-factor`, and `--max-mbps` now reject a non-finite
  value with a usage error. Salvaged from #30 (postgresql007).

### Added

- **`allow_unenforceable_lease` per-deployment config for backends without
  atomic conditional writes.** Ceph S3 and some MinIO deployments cannot
  perform an atomic conditional put, so the per-deployment backup lease
  cannot be acquired and an agent's *scheduled* backups fail with
  `backup.lease_failed`. The new `deployments.<name>.allow_unenforceable_lease`
  flag lets the operator proceed anyway — the scheduler skips the lease,
  mirroring the existing `backup --allow-concurrent` semantics. Safe when
  exactly one agent runner exists (the sidecar chart enforces and documents
  `replicas=1`); the operator takes explicit responsibility by setting it.
  Honoured on **both** agent backup paths — the control-plane executor and
  the in-process schedule that the sidecar chart's `pg_hardstorage agent`
  actually runs.

  🎉 **pg_hardstorage's first external contribution** — thank you
  [@flku-snp](https://github.com/flku-snp) (#47). The in-process-schedule
  wiring + its integration test were added as a follow-up so the option
  behaves the same in both agent modes.

## [1.2.3] — 2026-08-18

### Fixed

- **Restore of a backup with a non-default tablespace now creates the
  `pg_tblspc/<oid>` symlinks, so verify and startup succeed (#50).**
  A restore materialised a non-default tablespace's files at their
  (remapped) location and wrote `tablespace_map`, but never created the
  `pg_tblspc/<oid>` symlink in the restored data directory — the code
  assumed PostgreSQL would create those links at recovery start.
  However, the in-process `pg_verifybackup` (and PG startup) runs
  *before* recovery and resolves a tablespace file's `pg_tblspc/<oid>/…`
  path through that link, so the restore failed with
  `verifybackup_failed: file "pg_tblspc/…": file missing from restored
  datadir` and `pg_tblspc/` was left empty. `pg_basebackup` creates these
  symlinks itself; the restore now does too, pointing each at the same
  location `tablespace_map` records, before the verify gate. Covered by a
  new real-PostgreSQL integration test that creates a table in a
  non-default tablespace, backs it up, and restores it clean.
- **The post-restore boot smoke test skips clearly when `postgresql.conf`
  lives outside PGDATA (#50).** On the Debian/Ubuntu
  `/etc/postgresql/<v>/main` layout (or any cluster with an external
  `config_file`), `BASE_BACKUP` carries no `postgresql.conf` into the data
  directory, so the `pg_ctl start` smoke test failed with a cryptic
  "could not access the server configuration file". That is a
  configuration layout, not a broken backup: `postverify` now detects the
  absent `postgresql.conf` and skips with an actionable reason
  (`restore.postverify_skipped`) in the default mode — or fails
  `--verify-restore=require` with the same clear explanation — instead of
  the raw `pg_ctl` error. Normal restores keep `postgresql.conf` inside
  PGDATA and are unaffected.

## [1.2.2] — 2026-08-17

### Fixed

- **Storage janitors and guards no longer mishandle a deployment or
  backup ID that contains a `.tmp.` / `.json.tmp.` substring (silent
  data loss).** A deployment or backup ID may legitimately contain dots
  — only path separators and control characters are barred — but
  several storage walks matched the commit-staging-temp marker against
  the FULL object key instead of just the filename. A committed object
  under such a name (e.g. a backup `db1.full.tmp.abc` or a deployment
  `db.json.tmp.x`) could be mistaken for an in-flight staging temp and
  skipped or swept: `repo gc` could delete a live backup's manifest or
  reap a still-referenced chunk, `wal prune` could advance its frontier
  past a live backup and delete WAL it still needs, `repo replicate`
  could silently omit a backup from a DR replica, the archive-frontier
  lookups could report "nothing archived" (blinding failover-gap
  detection), and the foreign-cluster `system_identifier` guard could
  fail open. Every affected matcher is now scoped to the key's
  basename, so a committed object is never mistaken for a temp
  regardless of its deployment or backup name. Reachable only for the
  unusual (but valid) dot-`tmp` naming; no conventional deployment was
  affected.

- **`backup` no longer silently drops a TDE declaration — the
  `source_tde` manifest stamp works.** The runner could already record
  source Transparent Data Encryption on the manifest and `wal push`
  had a `--tde` flag, but the `backup` command wired up neither: it had
  no TDE flag and never read the deployment's `tde:` block, so
  `source_tde` was always `null` even for a declared-TDE deployment
  (CYBERTEC PGEE, pg_tde, EDB TDE), contradicting the TDE-awareness
  docs. `backup` now gains `--tde`, `--tde-engine`, and `--tde-key-ref`
  and also reads `tde.enabled` from the deployment config (the flags
  win when both are set); the declaration is stamped onto the manifest
  as `source_tde` and surfaced in the backup result JSON and text
  output. That stamp is what lets a later restore refuse a
  vanilla-PostgreSQL target loudly instead of writing a data dir full
  of ciphertext. The field is additive and omitted when TDE is not
  declared, so nothing changes for existing non-TDE deployments. Note
  this is posture metadata only: a backup and same-engine restore of a
  TDE cluster already worked with no flag, because `BASE_BACKUP` and
  replication deliver plaintext over the wire (the source engine
  decrypts above the replication boundary) — the flag records the
  source posture, it is not required for the backup to succeed. (#48)

- **Restore refuses a non-contiguous chunk list instead of writing
  scrambled bytes.** `materializeFile`, which rebuilds every file from
  its chunks, wrote them in slice order and checked only per-chunk
  length and total size — so a chunk list that was reordered or gapped
  but still summed to the file size would restore byte-scrambled data
  that passed every check, silently. It was safe in practice because
  `Manifest.Validate` enforces contiguity and restore re-runs it, but
  the byte-order of restored data should not rest on a single upstream
  guard. `materializeFile` now confirms each chunk's recorded offset
  matches the running write position and refuses otherwise — the same
  belt-and-braces posture as the restore-side identity check. Found by
  a direct-coverage pass on a function that previously had none.

- **The `--to` time parser rejects malformed 12-hour clocks instead of
  silently misreading them.** A 12-hour clock has hours 1..12, but the
  parser accepted `13am` (resolving to 13:00 — 1pm), `0am` (midnight),
  and `0pm` (noon): a plausible typo — meaning `1am`, typing `13am` —
  silently produced a `recovery_target_time` twelve hours off, which no
  downstream layer can catch once the instant is well-formed. Malformed
  12-hour hours are now rejected loudly. Found by a fuzz pass on the
  time parser; every valid form (`12am`→midnight, `12pm`→noon) is
  unchanged.

## [1.2.1] — 2026-08-09

### Fixed

- **The keystore accepts owner-only key files stricter than 0600,
  and the sidecar chart's keyring actually loads (#46).** Both
  permission gates (`kek.bin`, signing key) demanded exactly `0600`;
  a read-only `0400` — precisely what a mounted Kubernetes Secret
  with the chart's own `defaultMode` produces — was refused, so the
  keyring feature shipped in the chart could never start. The gates
  now refuse what they always meant to refuse: any group or other
  bit. On top, Kubernetes rewrites Secret-file modes under `fsGroup`
  (group-read gets OR'd in), so the chart no longer mounts Secrets at
  the keyring path at all: a new `install-keyring` initContainer runs
  the new `pg_hardstorage keyring install` command — built for
  distroless images — copying the material into an in-memory volume
  with owner-only modes. A misconfigured Secret now fails the init
  step loudly instead of producing a keyless pod. Credit to the
  issue-#46 reporter, whose follow-up measured the fsGroup behaviour
  before we shipped into it.

- **A recreated replication slot no longer kills the streamer.** The
  resume floor check refused whenever the computed start sat below the
  slot's `restart_lsn`, treating it as proof the WAL was recycled. It
  is not: `restart_lsn` is a retention floor, and Patroni recreates
  permanent slots at the promotion point — after a failover (the
  chaos gate's new DCS-outage fault found it on a demotion storm) the
  recreated slot routinely sits above a perfectly servable archive
  frontier, and the predictive refusal stopped `wal stream`
  permanently in a self-healing situation. The stream now warns on
  the mismatch and ATTEMPTS the resume; PostgreSQL is the arbiter. If
  the WAL is genuinely gone, walsender's own "requested WAL segment
  has already been removed" is classified as the terminal error with
  the same `wal.start_before_slot_restart_lsn` code and the same
  re-anchor remediation — real losses read exactly as before, only
  the false positives are gone.


## [1.2.0] — 2026-08-08

### Fixed

- **GCS operations fail in bounded time instead of retrying for the
  caller's lifetime.** The SDK's default retries idempotent operations
  until the context deadline with backoff pauses up to 30 seconds —
  and a `wal stream` holds its context for days, so a GCS outage
  parked operations indefinitely (the storage fault soak caught a
  worker 37 minutes asleep inside the SDK's retry loop). The client
  now caps retries at five attempts with a 5-second backoff ceiling:
  any single operation fails visibly within ~15 seconds, and the
  callers' own retry and refusal machinery — which knows how to say
  things out loud — takes it from there. Same family as the SFTP
  dead-connection fix.

### Fixed

- **The window between an existing backup and a stream's first start is
  now recorded — and a recovery that would cross it refuses.** `init
  --quick` (or any backup) followed by starting `wal stream` leaves WAL
  nothing covers: the backup bundles WAL to its own stop, and the fresh
  replication slot anchors wherever PostgreSQL is *when the slot is
  created*. A `--to-latest` or standby recovery from that backup replays
  the bundled WAL, asks `restore_command` for the next segment, gets
  "not in repo" — and PostgreSQL cannot distinguish a hole from the
  genuine end of the archive, so it ends recovery, **promotes**, and
  reports success arbitrarily far behind. `wal audit` is equally blind:
  it sees holes *between* archived segments, and this hole ends where
  archived WAL begins. Found by inspection while chasing a boot-proof
  failure (whose true cause turned out to be a test-workload fault): the
  common `init --quick`-then-stream flow is usually saved by the slot
  anchor aligning DOWN into the backup's final segment, but any longer
  pause before the first `wal stream` — an operator finishing setup, a
  deploy step between — leaves the window uncovered.

  Two halves, mirroring the Patroni failover-gap machinery: the stream's
  fresh-slot start persists the uncovered window as a gap record (with a
  CRITICAL event if persistence fails — a lost record is a restore that
  silently truncates later), and unbounded recovery (`--to-latest`,
  standby) now *refuses* with `restore.target_in_wal_gap` when a
  recorded gap lies at or beyond the backup's stop. Gaps entirely below
  the stop never refuse — that history is never replayed. Take a fresh
  backup to re-anchor PITR; `--skip-gap-check` remains the eyes-open
  override.

- **Time/name-target PITR no longer refuses over gaps its seed backup
  can never reach.** The `--to <time>` / named-restore-point gap
  preflight refused whenever the deployment had *any* recorded WAL gap.
  That blanket rule predated the seed backup's stop LSN being available
  at the check, and it composed badly with retention: gap records are
  per-deployment and outlive the backups they described, so once
  retention expired the generation a gap belonged to, **every**
  time-targeted restore of that deployment refused forever — over a
  window no surviving backup's replay could even enter (found composing
  the pre-stream-gap fix above with the retention janitors). The refusal
  and its advisory warning now apply the same bound the unbounded check
  uses: a gap ending at or below the chosen seed's stop is history the
  restore never replays and is ignored; gaps at or beyond the stop
  refuse exactly as before, and an unknown stop keeps the conservative
  blanket posture.

- **`backup undelete` re-checks the WAL, not just the chunks.** A
  tombstoned backup does not hold the WAL-prune frontier — that is the
  point of retention — so `wal prune --apply` legitimately deletes the
  archived segments right after a deleted backup's stop. Resurrecting
  that backup then handed back one that restores and boots perfectly,
  but whose `--to-latest`, standby, or time-target recovery replays its
  bundled WAL, asks `restore_command` for the next segment, and finds
  the pruned hole: PostgreSQL cannot distinguish it from the end of the
  archive, so a one-shot restore **promotes** silently behind and a
  standby freezes forever waiting. Pruning leaves no gap record, so
  none of the restore-side refusals could fire — the only signal was a
  Warning-severity contiguity event at restore time. Undelete now
  probes forward WAL coverage at the resurrection point and persists
  the missing window as a gap record (surfaced in the command's result
  as `wal_gap_recorded`), which routes the doomed restores into the
  existing typed `restore.target_in_wal_gap` refusal — with full
  seed-stop precision, so restores from newer backups are untouched,
  and `--skip-gap-check` remains the eyes-open override. The backup
  itself still resurrects: restoring *within* its own window is
  legitimate and unaffected.

- **The fresh-slot gap recorder no longer claims archived WAL as
  missing after a failover.** A Patroni failover destroys the
  streamer's replication slot on the new leader, so the reconnect
  creates a fresh slot — the same code path as a first-ever stream
  start, which records the uncovered window as a WAL gap. But the
  recorder opened that window at the *oldest live backup's stop*: in a
  deployment that had streamed for months, one failover recorded a gap
  spanning months of successfully archived WAL. Gap records are
  eternal, so every unbounded restore from every backup older than the
  failover then refused `restore.target_in_wal_gap` forever — a
  permanent false positive that pushes operators toward a reflexive
  `--skip-gap-check`, which also bypasses the refusals that are true.
  The window now opens at the **archive frontier** (current timeline
  first, then the nearest timeline below — the coordinator's own
  rule, never max-across, so diverged old-timeline WAL past the branch
  does not count as coverage), falling back to the oldest backup's
  stop only when nothing is archived — the original first-stream case,
  which is unchanged. When the archive already reaches the new slot's
  anchor, nothing is recorded at all.

- **Infrastructure failures during recovery no longer masquerade as
  end-of-archive.** PostgreSQL's `restore_command` contract is
  narrower than it looks: *every* plain nonzero exit means "that file
  is not available", and during unbounded recovery that means end of
  archive — stop, **promote**, report success. Only a termination by
  signal aborts recovery. The generated command passed non-not-found
  exit codes through in the belief they would surface as a crash; they
  never did. An S3 outage, an expired credential, a keyring refused
  for its file mode, a corrupted segment manifest, a chunk the
  janitors swept, even the agent binary missing from the recovery
  environment (shell exit 127) — each one silently ended recovery and
  promoted a truncated cluster; the verifier's sandbox could
  false-green the same way. The one-shot tail (PITR, `--to-latest`,
  time-travel, verify) now maps not-found to PG's expected exit 1 and
  self-terminates with `SIGABRT` on anything else, so recovery aborts
  loudly at the fault. Standby restore_commands keep the lenient
  pass-through (`BuildStandby`): a standby polls forever and "not
  archived yet" is exit-nonzero by contract — aborting there would
  crash a replica on every transient blip; a frozen standby is lag,
  which monitoring already sees, while a promoted-behind cluster is
  data loss.

- **Every restore_command now arms the cluster-identity check.** The
  check shipped armed on `restore` and the standby auto-recovery path;
  the four remaining generators — standby bootstrap, time-travel, the
  agent's restore executor, and the verifier's sandbox — passed an
  empty identity and generated identity-blind commands. All six sites
  now thread the seed backup's system_identifier (best-effort: an
  unreadable manifest leaves the check unarmed, never blocks). The
  standby variant composes with its lenient tail deliberately: a
  foreign-lineage standby freezes at the boundary logging the typed
  mismatch every poll instead of replaying foreign WAL. For the
  verifier this closes a correctness corner: a backup must not
  green-light by replaying WAL from a different lineage that shares
  the deployment name. A source-level guard enumerates all six sites
  and fails if any regresses to an unarmed builder.

- **The stream retry loop's decisions are now directly testable.**
  The loop's control flow — permanent-vs-retryable classification, the
  no-progress stop backstop, duration-aware backoff reset, the
  draining-primary fast path — was covered only by a hand-mirrored
  simulator that verified its own mirror and silently omitted the
  entire mid-stream half. The decisions now live in a pure state
  machine (`streamRetryPolicy`) the loop consults, tested directly:
  eleven cases including six the simulator never modeled. Behaviour
  preserved bit-for-bit, including the emitted-backoff asymmetry
  between the setup and stream-break paths.

- **Restores refuse foreign WAL at the first byte, by name.** The
  archive-side guards (`wal stream`, `wal push`) refuse a cluster's
  system-identifier change at write time — but an archive that already
  mixes lineages (a deployment name reused after a wipe, a pre-guard
  mix, an `--allow-system-identifier-change` archive) reaches
  recovery, and PostgreSQL notices only mid-replay, after the
  restore's wallclock is spent, with a FATAL that names neither the
  deployment nor the repair. `restore` and the standby auto-recovery
  path now thread the seed backup's identifier into the generated
  `restore_command` (`wal fetch --expect-system-identifier`), so the
  first foreign segment is refused with a typed
  `wal.fetch.system_identifier_mismatch` naming both identities — and
  the strict tail aborts recovery loudly right there. Best-effort and
  additive: an unreadable manifest or a pre-schema archive simply
  leaves the check unarmed, never blocks a restore.

- **The agent-kill drill classifies an impossible budget before
  running anything.** `recover_within` shorter than the drill's lease
  TTL is the operator's parameter, and is now reported as
  misconfiguration up front — previously the check sat after the
  drill's timing-sensitive lease steps, so on a loaded host the
  2-second probe lease could expire mid-drill and the operator saw a
  spurious product failure instead of their own budget.

- **An SFTP handle heals itself after a dead connection is torn
  down.** The keepalive fix above turns a dead peer into an error —
  but the handle it killed used to stay dead: a `wal stream` holds one
  storage plugin for days and the CLI opens the repository once,
  outside its retry loop, so a single 70-second network stall stopped
  archiving until an operator restarted the process. Every operation
  now passes through a reconnect gate: after a teardown, the next
  operation re-dials with the parameters `Open` stored (rate-limited
  to one attempt per interval, so an ongoing outage cannot stampede),
  and a genuinely unreachable server fails fast with a typed error.
  Measured recovery: the first operation after the network returns
  completes in under half a second.

- **A dead SFTP connection is now an error, not a forever-hang.**
  Caught live by the storage fault soak: `ssh.ClientConfig.Timeout`
  covers only the dial, and `pkg/sftp` sets no deadlines after it, so
  a peer that went silently away (NAT table expiry, power loss, a
  network partition) left every operation blocked indefinitely — a
  `wal fetch` inside `restore_command` hanging recovery (PostgreSQL
  waits on the command, so not even the signal tail can fire), a
  backup that never finishes and never fails, an archiver stalled
  while `pg_wal` fills the disk. The plugin now runs an SSH-level
  keepalive probe (the `ServerAliveInterval` idea): a reply proves the
  round-trip, a timeout counts a miss, and enough consecutive misses
  tear the connection down — transport first, since a graceful SFTP
  close writes on the same dead connection — which surfaces every
  parked and future call as a "connection lost" error within ~70
  seconds. TCP keepalives are armed as well (kernel defaults alone
  take hours). A healthy connection is never touched: the
  false-positive direction is covered by its own test.

- **A restore that cannot reach the newest timeline now refuses
  instead of silently stopping short.** PostgreSQL discovers the
  newest timeline by probing `<N>.history` files ascending and stops
  at the *first miss*: one history file that was never archived (a
  promotion race, a lost spool) or was lost later makes a
  `--to-latest` recovery silently end on an older timeline and promote
  — success reported, every segment archived on the newer timeline(s)
  ignored. A pinned `--timeline N` fails the same way without
  `N.history`, which carries the ancestry chain. The new preflight
  enumerates the timelines the archive holds segments for and refuses
  (`restore.timeline_history_unreachable`) when a history file needed
  to reach the requested timeline is in neither location `wal fetch`
  serves them from (the archive path and the streaming follower's
  timeline store) — naming the missing file and the `wal push` command
  that re-archives it. `--skip-gap-check` remains the eyes-open
  override.

### Added

- **`restore --to-latest`: end-of-archive recovery.** There was no CLI
  spelling for the most common disaster-recovery operation — restore the
  backup, then replay *every* archived WAL segment. A restore without a
  `--to*` target wrote no recovery files at all (while a doc comment
  claimed otherwise), so booting it silently ignored everything archived
  after the backup; and a fake far-future `--to` is no workaround, since
  PostgreSQL 13+ FATALs when a recovery target outruns the WAL.
  `--to-latest` writes `recovery.signal` + `restore_command` with no
  target — PostgreSQL's definition of replay-to-end — then applies
  `--to-action`. Conflicts with point targets are refused at flag level.

### Fixed

- **`wal fetch` decrypt failures now name the exact refusing step.**
  The error collapsed keyring-path-unresolvable, missing `kek.bin`, a
  kek.bin *refused for its file mode* (the keystore requires 0600),
  wrong KEK, and KMS-unreachable into one "could not be resolved"
  message. `wal fetch` runs inside `restore_command`, in the most
  minimal environment the product ever sees, mid-recovery — the
  operator gets one shot at reading that line, and each cause has a
  different fix.

- **Test-only:** `testkit.ExpectedPGMajor` silently coerced any
  unlisted major — including 19 — to 17, so a matrix believing it
  tested PG 19 was testing 17 under a 19 label. It now accepts any
  major in [15,29] verbatim and panics on garbage.

- **`backup undelete` warns that a policy rotate will re-delete the
  resurrected backup.** Rotation deleted it as policy-excess; an
  undelete makes it excess again, and the next `rotate --apply`
  re-tombstones it — measured, and policy-correct. What made it a trap
  was silence: an operator who just recovered a backup reasonably
  believes it stays recovered, and the next cron run undoes them. The
  success output now names the behaviour and the remedy — a hold, which
  both rotation and deletion respect — and the command help documents
  it.

- **`backup undelete` re-verifies chunks at the moment of resurrection,
  not just before it.** Undelete's restorability pre-flight ran while
  the manifest was still hidden, and a concurrent `repo gc --apply`
  sweep works from a reference snapshot taken before the undelete began
  — so the pre-flight passing guaranteed nothing about the seconds that
  followed. If gc's delete loop swept the chunks in that window, the
  undelete reported `restored=true` and the operator held a backup that
  lists as live and cannot restore. The chunks are now re-checked
  immediately after the tombstone marker is removed; on a miss the
  original marker is re-installed byte-for-byte (policy, reason and
  timestamps intact) and the call fails with the same
  `conflict.chunks_missing` refusal the pre-flight gives. The residual
  window shrinks from the duration of gc's delete loop to milliseconds.

- **The documented cascade unwind now works verbatim.** `backup delete
  --cascade` returns its `cascade_deleted` slice leaf-first — the
  correct order for deletion and exactly the wrong one for
  resurrection, because undelete refuses an incremental whose ancestor
  is still tombstoned. The docs told operators the slice "is exactly
  what you pass back to unwind a wrong cascade"; doing precisely that
  failed on the first ID. Batch undelete now resurrects ancestors
  before descendants automatically (within the batch; a tombstoned
  ancestor *outside* the batch still refuses, correctly), so the slice
  works as returned — the situation where an operator is following
  instructions verbatim under stress. Outcomes are still reported in
  the order given.

- **A backup can no longer commit a manifest over chunks a concurrent
  `repo gc --apply` deleted.** Deduplication adopts existing chunks via
  a `Stat` — it touches no object and refreshes no mtime, so gc's
  `--min-chunk-age` floor (which protects chunks a backup *wrote*) never
  sees it. If a tombstone expired mid-backup, its orphaned chunks could
  be adopted by the in-flight backup and then swept before the manifest
  committed: a brand-new backup, born unrestorable, reporting success.
  gc's guards (live-lease refusal, reference re-collect) are timing
  guards that shrink the window; none closes it, because the adopt is
  invisible to gc.

  Closed from the writer's side: the CAS now records every hash it
  adopted rather than wrote (hint-confirmed Stat hits and lost
  `IfNotExists` races), and the backup runner re-Stats exactly those
  immediately before manifest commit — refusing, loudly and retryably,
  if any is gone. The retry rewrites the chunk with a fresh mtime that
  the age floor then protects. gc additionally re-scans backup leases
  *after* its reference re-collect (a full manifest walk, minutes on a
  large repo), so a backup starting during that walk is noticed before
  the sweep rather than after.

  The same gate protects WAL, where it is the *only* protection: a
  streamer holds no backup lease, so gc's live-lease refusal never
  covers it, and identical plaintext genuinely recurs in WAL (an
  unchanged page resurfacing as a full-page image). Both `wal stream`
  and `wal push` now verify adopted chunk references before committing
  a segment manifest — a committed segment referencing a swept chunk is
  WAL that cannot be fetched at recovery while the archive looks
  gap-free.

- **A standby built over a holed archive is now warned about.** The WAL
  contiguity preflight ran only for LSN targets, and a standby has no
  target by construction — so it was never checked, while being the
  consumer that suffers most from a hole. For a one-shot restore a
  missing segment stops recovery loudly; for a standby it is the normal
  signal for "not archived yet, keep waiting", so PostgreSQL stays up,
  answers read queries and reports healthy while frozen at the gap.
  Nothing distinguishes it from a standby that is merely caught up, and
  an operator holding it for DR finds out at failover.

  The stated limitation — that non-LSN targets cannot be resolved to a
  segment range before recovery — is true for time and name targets,
  where only PostgreSQL knows the LSN, and those remain skipped. It was
  not true for unbounded recovery, whose upper bound is simply the
  archive frontier. Failovers are the usual source of such holes, which
  is what makes the standby case worth covering.

- **The timeline-history chain survives a promotion the streamer
  missed.** `wal stream` captured the history of the timeline it was
  following, which is enough only when a streamer witnesses every
  promotion. A failover during an agent restart, a deploy or a crash
  left no history file for that timeline — and a hole in the middle of
  the chain does not lose one timeline, it truncates the chain from
  there on: PostgreSQL discovers the newest timeline by probing
  `restore_command` for successive history files and stops at the FIRST
  miss. With `recovery_target_timeline='latest'` that is silent. PG
  concludes the newest timeline is the one before the hole, recovers
  along it, and reports success having dropped everything after.

  Measured against a real cluster with two promotions and the streamer
  absent for the middle one: `00000003.history` present,
  `00000002.history` missing, and a recovery would have landed on
  timeline 1. The capture now walks the whole ancestry, skipping files
  already stored so a caught-up archive costs no round trips, and runs
  before the resume point is resolved — a streamer that must refuse
  because its WAL was recycled is exactly the one whose ancestry nobody
  else is recording.

- **The WAL sink's contiguity guard now covers the opening record.** The
  stream-level check compares each record against the end of the
  previous one, and its baseline was unset until a record arrived. That
  is sound for one continuous stream, but `wal stream` builds a new sink
  on every reconnect attempt — so after each reconnect the first record
  was accepted at whatever position it carried, and every later record
  was measured against *that*. A stream that resumed past a hole looked
  perfectly contiguous for the rest of its life.

  The sink is now told the position the streamer asked PostgreSQL to
  resume from, and refuses an opening record that begins after it. Only
  that direction is a fault: a walsender may legitimately open at a page
  or segment boundary at or below the requested position, and refusing
  those extra bytes would turn a healthy stream into a crash loop — a
  worse way to lose WAL than the gap being guarded against.

  This is the safety net beneath the resume fixes above rather than a
  replacement for them; a net that only holds when the thing above it is
  already correct is not a net.

- **`wal stream` refuses to archive from a demoted node without saying
  so.** The streamer has no Patroni awareness — leader-following is
  delegated to libpq, which only works when `--pg-connection` is a
  multi-host DSN carrying `target_session_attrs=primary`. A single-host
  DSN has nothing to route, so after a failover the streamer reconnects
  to the node it always used, which is now a replica. PostgreSQL permits
  physical replication from a standby, so nothing failed: measured
  against a real cluster, the streamer was still running 90 seconds
  after its node was demoted, having reconnected and resumed, reporting
  nothing unusual. It archives second-hand WAL from a replica while
  every health signal says otherwise, and if that replica falls behind
  or is reinitialised, WAL the primary already recycled never reaches
  the archive.

  The refusal is retryable rather than fatal, since during a failover
  every node is briefly in recovery and a leader-aware DSN reaches the
  new primary within a few attempts. It is not part of the preflight, so
  `--skip-preflight` cannot waive it. `--allow-standby-source` keeps
  deliberate archiving from a replica available.

- **`wal push` refuses WAL from a different cluster than the deployment
  already holds.** `wal stream` has guarded this since the pg_upgrade
  work; the archive_command path did not. What made it look covered was
  the split-brain check, which compares system identifiers only when the
  *same* segment number is already archived — a duplicate check, not a
  continuity check. A foreign cluster whose segment numbers happened not
  to collide (the normal case after a pg_upgrade, which resumes at a
  higher LSN) archived into the deployment unopposed.

  The consequence reaches past a cluttered archive: the resume and gap
  computations read the archive frontier as the highest segment's end
  LSN, without regard to which cluster wrote it, so a foreign segment at
  a higher number drags the frontier forward and `wal stream` resumes
  past WAL the real cluster has not archived yet. `wal push` now takes
  the same `--allow-system-identifier-change` escape hatch as `wal
  stream`, and the refusal names both that flag and the better answer —
  a fresh deployment, which keeps the old lineage restorable.

- **`wal stream` now captures the timeline-history file a cross-failover
  PITR needs.** Nothing produced `<tli>.history` in a streaming-only
  deployment: the only writer ran under `agent` with a Patroni URL
  configured, and a `wal stream`-only HA setup runs neither that nor an
  `archive_command`. Because the default `recovery_target_timeline` is
  `latest`, PG does not fail when the file is absent — it follows the
  highest timeline it can resolve history *for*, so recovery silently
  proceeds along the pre-failover timeline and promotes a database
  missing everything written after the promotion. The capture is
  best-effort (refusing to stream would trade a PITR limitation for
  losing all subsequent WAL) but never silent: a failure emits
  `wal.timeline`/`history_not_captured` explaining the consequence.
  Timeline 1 has no parent and is skipped without comment.

- **`wal stream` no longer skips the old timeline's WAL after a
  promotion.** The resume point came from a lookup scoped to a single
  timeline. After a failover the new primary reports timeline N+1 and
  nothing is archived under it yet, so the lookup missed and the miss
  fell through to the fresh-deployment branch, which anchors at the
  slot's `restart_lsn` — the new leader's *current* position. Every
  byte between the old timeline's archived frontier and there was
  never requested, and nothing reported it: the slot reconciler ran
  with `lastConfirmedLSN=0` so no gap was computed, the fresh branch
  skipped the floor check, and the sink's contiguity guard resets on
  every reconnect. The stream reported success; the hole surfaced
  later as a PITR that could not cross the window.

  A miss on the current timeline now falls back to the frontier of the
  timeline the cluster branched from — a position the new primary can
  still serve, since the lineages share history up to the branch
  point. If it has already recycled past it, the run fails with
  `wal.start_before_slot_restart_lsn`, which carries the remediation
  for unrecoverable WAL. Loud and known beats silent and lost.

  A deployment running only `wal stream` — the documented
  streaming-only HA posture — had no other producer of this signal.

- **The agent measures the failover gap instead of reading it as a
  bootstrap.** The Patroni coordinator asked for the archive frontier
  using the *new* leader's timeline, so on the first reconcile after
  every promotion the lookup missed and returned zero — which the slot
  reconciler reads as "first-time bootstrap" and which short-circuits
  the gap calculation. On the one event the calculation exists for, it
  was guaranteed to measure nothing: no gap event, nothing recorded,
  and later no record with which to refuse an unsafe PITR. A real
  failover gap and a clean handover produced identical output.

- **`wal audit` detects WAL holes that straddle a timeline change.**
  Gap detection skipped every timeline transition outright, so it could
  only find holes *inside* one timeline. A failover is a timeline
  transition, which put the blind spot exactly where an HA deployment
  is most likely to lose WAL — and the nightly soak's "the WAL lineage
  must be gap-free" gate runs `wal audit`, so it inherited the same
  hole. Segment numbering is continuous across a promotion, so the
  ordinary arithmetic was already correct at the boundary; the skip was
  never needed. Overlap, where an old timeline holds segments past the
  branch point written by a primary that was later fenced, is still not
  reported — that is diverged history, not missing WAL. Gaps that
  straddle a change now carry `end_timeline` in the JSON (omitted
  otherwise, so the result shape is unchanged for existing consumers)
  and name the transition in text output.

- **`gameday run patroni_failover` measures the invariant it
  declares.** The slot-observation seam was never wired outside tests,
  so the shipped tier-L4 drill always took its unmeasured branch and
  returned a pass on the strength of the leader having moved — which is
  not evidence that the promotion kept the WAL. It also evaded the
  guard written for exactly this shape, which keys on evidence tagged
  `deferred` while this branch said `unmeasured`. The seam is now
  wired to the same reconciler path the agent uses; when it genuinely
  cannot be measured the run is deferred rather than passed; and the
  guard now works from a classified set of kinds, so the next synonym
  has to be declared rather than slipping through.

- **A repository can be append-only.** Manifests, integrity runs, DSA
  reports, threshold rosters and headers, insider scans, timeline
  histories, tombstones and pushed auxiliary files were all published
  by writing `<key>.tmp.<rand>` and renaming it into place — which on
  S3 is `HeadObject` + `CopyObject` + **`DeleteObject`**. A repository
  kept as an anti-ransomware copy of record therefore accrued a delete
  marker per WAL segment and per base backup ([#45]).

  Publishing now uses a single conditional `PUT` where the backend
  advertises `ConditionalPut`: atomic, so nothing partial is visible,
  and `If-None-Match: *` rejects a second writer — the two properties
  staging existed to provide, with no staged object and so no delete.
  Backends that cannot (SFTP without `hardlink@openssh.com`) stage
  exactly as before.

  Two paths that deliberately REPLACE an object — rebuilding a corrupt
  replica, and `repair manifest`'s overwrite — became a direct atomic
  `Put`. Both previously deleted the key first, leaving a window with
  no object at all.

- **Manifests commit on stores without a conditional COPY** ([#45]).
  S3-compatible stores commonly implement a conditional PUT and not a
  conditional COPY; the staging path needed the COPY, so on those
  stores no manifest could commit at all. Base backups failed with
  `NotImplemented`; `wal stream` did not fail visibly at all — see
  below.

- **`wal stream` stops instead of retrying an unwinnable stream**
  ([#45]). Only pre-stream *setup* errors were classified as permanent,
  so a failure once streaming had begun was retried forever. With a
  repository that could not commit, the reported symptom was the slot
  active, chunks accumulating, `wal list` empty, memory climbing, and
  the process OOM-killed and restarted from the same LSN — with nothing
  logged. The loop now stops on a recognised permanent condition, and
  on a backstop of five consecutive attempts that synced nothing, which
  covers permanent failures that carry no matchable error code.

- **`--output json` no longer suppresses warnings and errors** ([#45]).
  The suppression exists so a JSON consumer parses the final Result
  rather than a progress stream, but it applied to every severity — so
  the reconnect warnings above went nowhere under the renderer a
  Kubernetes deployment would obviously choose.

- **The Helm chart never mounted the keyring it documented** ([#46]).
  `helm-sidecar-chart.md` stated that `/etc/pg_hardstorage/keyring/`
  "mounts as part of the ConfigMap". No template provided it, and the
  StatefulSet had three fixed mounts with no way to add a fourth — so
  there was no way at all to persist a keyring for
  `kek_ref: local:default` on the chart.

  The keyring now mounts from a **Secret**, via `keyring.existingSecret`
  (bring your own — sealed-secrets, external-secrets, a CSI driver) or
  `keyring.files` (inline `filename` → base64, for small deployments).
  `extraVolumes` / `extraVolumeMounts` cover anything the chart does
  not model. Nothing is rendered when both keyring options are empty,
  so a cloud-KMS deployment mounts exactly what it did before.

  The page was wrong about the destination as well as the mechanism:
  key material in a ConfigMap is readable by anyone with
  `get configmap`, a lower bar than `get secret` wherever RBAC
  separates the two. It now describes the Secret.

- **`scp`: a successful read reported failure on Close.** `Close`
  returned `io.EOF` after a complete, correct read — the reader waits
  for the remote `cat` to finish, and closing an already-finished ssh
  session yields `io.EOF`, which was passed straight through.
  `defer rc.Close()` hides this, which is how it survived since
  v1.0.0. A caller that checks `Close` — the correct way to use an
  `io.ReadCloser` — discarded perfectly good bytes and reported a
  fault in the data.

- **`scp`: a failed remote read looked like an empty object**, and a
  key deleted concurrently surfaced as a transport failure rather than
  `ErrNotFound`. Both are read-side, on the path a restore takes.

- **Two game-day scenarios could not fail.** `agent_kill` and
  `patroni_failover` each appended evidence saying the runtime drive
  was deferred and then returned `pass: true`. `gameday run
  patroni_failover` — declared tier L4, the tier an auditor reads as
  "we tested catastrophic failover" — exited 0 having promoted nothing
  and measured nothing, and `gameday report` counted it a success,
  indistinguishable from a scenario that ran and held. Both now drive
  real faults (see Added); a scenario that declares an invariant
  without driving it reports `deferred: true`, `pass: false` and
  `notimpl.scenario` — not a failed invariant, but never a pass.

- **`gameday run <scenario>` misdiagnosed a missing `--repo`.** The
  unknown-deployment hint, which is right for `backup <deployment>`,
  fired on any `usage.missing_flag` mentioning `--repo` — telling
  operators that `patroni_split_brain` "is not in pg_hardstorage.yaml
  (configured: ...)" and pointing them at `deployment list` for a
  scenario name, moving the exit code from 2 (usage) to 6 (notfound).
  It is now scoped to commands whose positional really is a deployment.

- **Documentation that described a binary we do not ship.** A sweep of
  every operator-facing surface; each drift below is now held by a
  test:

  - `severity` was documented as an `int8` carrying "RFC 5424 numeric
    severity" in both the `Event` and `Error` tables. It is a
    **string** on the wire (`{"severity": "warning"}`, never `4`), so
    a consumer comparing `severity <= 4` never matches and never
    errors. The `component` and `op` examples named `wal`, `kms`,
    `backup.start` and `kms.unwrap`; none of those exist.
  - 19 documented `jq` expressions across 13 pages addressed
    `.result.body.<field>`. There is no `body` level, so all 19
    evaluated to `null`: the SLO gates in `slo-as-code.md` are `jq -e`
    and would fail forever, R2's recovery loop iterates `null`, and
    `--template` renders a missing key as empty **with exit 0**.
  - `storage.no_space` and `kms.key_missing` were documented with exit
    8, which the reference page defines as transient-retry. Neither
    code exists; the real conditions are `preflight.repo_full`
    (exit 4) and `restore.kek_resolve_failed` / `restore.kek_mismatch`
    (exit 1). An `if [ $? -eq 8 ]` retry loop was wrapped around a
    full disk.
  - `usage.no_pg_verifybackup` (exit 2) is `verify.missing_tool`
    (exit 9) — misuse and verification-failed are opposite ends of a
    cron policy.
  - The `splitbrain.*` namespace was undocumented entirely: three
    codes an operator can hit with nowhere to look them up.
    `wal.slot_create_failed` on the reference page is
    `wal.slot_ensure_failed`.
  - R6 described the WAL-gap signal as a `wal.slot_recreated` notice.
    That name does not exist; a non-zero gap raises
    `wal.follower.wal_gap_detected` at **critical**, and the notice is
    the no-gap case. `enable-policy.md` claimed an audit event on
    every startup that is never emitted, and `plugins/index.md` listed
    five plugin-lifecycle audit types that do not exist, linking a
    reference page that is not in this repository.
  - `doctor --fix`, `list --tenant` and `gameday run --scenario` do
    not exist. `rotate-kek.md`'s two console blocks were invented, and
    its resumability loop polled a `would_rewrite` field the output
    does not carry.

### Added

- `repo check` reports the repository's **commit mode**
  (`conditional_put` or `stage_and_rename`), so the difference is
  visible before it bites rather than after.
- The S3 backend's `conditional_put=native` parameter is documented.
  Behind an `endpoint=` override the plugin will not assume the store
  enforces `If-None-Match` on PUT — assuming wrongly would make every
  single-winner guarantee silently false — so an operator whose store
  does enforce it must say so to get the append-only commit path.

- **`gameday` scenarios drive real faults.**

  - `patroni_failover` reads the current leader, POSTs `/switchover`,
    waits for a different member to take the leader lock, and
    re-measures replication-slot continuity across the promotion. It
    fails if the leader never moves, if Patroni refuses (no healthy
    candidate), or if the slot had to be recreated past the last
    confirmed LSN. Needs `--deployment` with `patroni.url` set, or the
    new `--patroni-url`.
  - `patroni_split_brain` (new) drills the guarantee R7 depends on: a
    divergent writer must not archive over a segment we already hold.
    It archives a probe segment, re-archives it with different content
    from the same cluster and from a different system identifier
    (expecting `splitbrain.content_mismatch` and
    `splitbrain.system_identifier_mismatch`), then confirms an
    identical re-push still succeeds — refusing an `archive_command`
    retry would wedge WAL archiving for the whole deployment.
  - `agent_kill` drills what a killed agent actually leaves: an
    unrenewed backup lease. It asserts a second agent is excluded
    while the lease is live, and that exactly one of five racing
    agents reclaims it after expiry. It does not signal a process —
    there is no supervisor to re-exec one, and the `pg_backup_start`
    leak its old invariant named cannot happen here, because
    `BASE_BACKUP` runs over a replication connection PostgreSQL tears
    down on disconnect.

  The repository-scoped drills write under a probe deployment name and
  delete it afterwards.

- `patroni.Client.Switchover` — the only mutating Patroni call we
  make. A 412 refusal is its own error class: the cluster answered and
  said no, usually because no replica is healthy enough to promote,
  which is a finding about the cluster rather than a failure to reach
  it.

- `gameday run --patroni-url`, for drilling a cluster that has no
  deployment entry yet.

- The `gameday` result body carries `deferred` and `misconfigured`, so
  a consumer can tell a scenario that did not run from one that ran
  and failed, and a missing flag from a broken invariant.

### Internal

- **Seven docs-truthfulness guards.** The config-YAML surface already
  had one (issue #44's outcome); the rest did not. Repository-URL
  parameters and `PG_HARDSTORAGE_*` variables (both directions), CLI
  flags shown in prose, `.result` paths resolved against each
  command's own result body, error codes and exit codes, event names
  and severities, and — after [#46] — the Helm chart rendered with
  `helm template` and checked against the pages that describe it.

  Two were rewritten after they cried wolf on correct documentation.
  One existing guard turned out to be unable to fail:
  `TestExitCodes_NoUndocumentedRoutes` iterated hand-written lists
  while its comment claimed a new route would fail it. The routing
  table is now enumerable data read at run time, pinned by
  `mutation_exit_route_undocumented`.

- **Windows and macOS unit lanes.** We ship binaries for both and had
  never executed a test on either.

- **A container/volume leak gate** after the integration suite. A
  local sweep once left 699 dangling volumes, which fills the runner
  disk and surfaces as "no space left" in an unrelated job.

- **The Docker soaks moved behind build tags** (`chaos`, `repro34`).
  Making a soak unable to skip had made it run in the default suite,
  where a 3-node Patroni chaos soak hit the `-race` timeout.
  `TestSoaksDoNotSkipOnUnsetEnv` could not see the second one at all:
  its pattern was `PGHS_[A-Z_]+`, with no digits, so
  `PGHS_REPRO34_BIN` was unmatchable.

- **Capability-gated skips must name where the weak backend's
  behaviour is asserted**, and the storage "skips forbidden" lane now
  covers all six backends rather than four — azblob alone had ten
  docker-gated skips outside it.

- A randomised read-race soak across all five real backends, which is
  what found the `scp` Close bug above.

- The release preflight fails on a missing Homebrew tap credential
  instead of discovering it after the tag.


## [1.1.1] — 2026-08-04

### Changed — action may be required

- **A backup now refuses to start when the repository backend cannot
  enforce the backup lease.** The lease is built on an atomic
  create-if-absent; a backend that only emulates one (stat, then write)
  lets two runners pass the check together and both proceed, so the
  lease is written, looks correct in the repo and to `repo gc`, and
  excludes nothing.

  In practice this is **SFTP against a server that does not advertise
  `hardlink@openssh.com`** — every other backend (`file://`, S3, GCS,
  Azure, `scp://`) advertises a native or `link(2)`-based conditional
  create and is unaffected. Affected setups now fail at acquire with
  `backup: repository backend cannot enforce the backup lease`, naming
  the backend and the extension.

  Move to an SFTP server that offers the extension, or to another
  backend. `LeaseOptions.AllowUnenforceable` overrides it for the case
  where exactly one runner can possibly exist — a library option, not a
  flag, matching how unenforceable WORM is handled.

- **A backup refuses to deduplicate against a chunk its own data key
  cannot read.** Chunk keys are global to a repository but the shared
  DEK is per-`kek_ref`, so a deployment pointed at a new `kek_ref`
  without a rotation used to commit a manifest referencing chunks it
  could not decrypt — a backup that succeeded and then failed at
  restore. It now stops with `does not decrypt with this backup's data
  key` and points at `kms rotate`.

### Fixed

- **Two backups of one deployment could run concurrently.** Breaking a
  lapsed lease was a recheck, an overwrite and a timed read-back, with
  nothing atomic in it: a reclaimer that stalled longer than the settle
  window overwrote a winner that had already verified, and both
  reported the lease held. Breaking now requires winning an atomic
  claim keyed to the lease being broken, so a stalled reclaimer cannot
  write at all. Reachable on every backend, not only in theory.
- `gcs`: a Put whose body failed mid-stream committed the truncated
  object instead of aborting it.
- `wal stream --once` could block indefinitely instead of honouring a
  caller-supplied deadline: `cli.Run` discarded any context set by its
  caller rather than deriving its signal context from it.
- KMS provider configuration was not resolved in two remaining unwrap
  paths, so a cloud-KMS deployment could fail to restore or verify.

### Added

- `leases/<deployment>/breaks/<token>.json` — break claims, written
  only when a crashed holder's lease is reclaimed. They accumulate with
  crashes rather than with backups, are a few hundred bytes each, and
  are deliberately never deleted: removing one would let a reclaimer
  still holding that stale token overwrite a live lease. `repo gc`
  ignores them.

### Internal

- Substantial test additions across the lease (now exercised against
  every real backend, not only `file://`), the cross-KEK dedup guard,
  storage capability honesty, SSH credential precedence, config
  round-tripping, the agent's KMS plumbing and the CLI coverage gate;
  plus fixes to test infrastructure that was itself unsound — a shared
  container image tag that raced between packages, host keys pinned by
  port rather than by container, and several tests whose stop condition
  was a wall-clock sleep rather than the thing they were waiting for.

## [1.1.0] — 2026-08-02

A minor bump rather than a patch: this release **adds configuration
surface**. `kms.providers[]` and per-deployment `kek_ref` are new keys
in `pg_hardstorage.yaml`, and the `scp://` storage backend goes from
unusable to working. Existing configurations are unaffected — no
migration, and nothing changes posture on upgrade.

### Security

- **Three CVEs removed from the release binary.** A pre-tag scan of the
  built artifact found vulnerable symbols linked into it:
  `google.golang.org/grpc` 1.80.0 (GO-2026-6061, xDS RBAC + HTTP/2
  transport — the client-stream symbols are on the GCP KMS path),
  `golang.org/x/text` 0.37.0 (GO-2026-5970, infinite loop on invalid
  input in `norm`), and `crypto/tls` from the Go 1.26.3 standard
  library (GO-2026-5856, Encrypted Client Hello privacy leak). Bumped
  to grpc 1.82.1 and x/text 0.39.0, and the Go toolchain pin moves
  1.26.4 → 1.26.5 across every workflow. `go.mod` now carries a
  `toolchain go1.26.5` directive so a local build gets the same
  standard library CI does, rather than whatever the developer happens
  to have installed. The artifact scans clean.

- **The vulnerability gate had been failing open.** `make govulncheck`
  ran only govulncheck's source mode, which on Go 1.26 panics with
  `ForEachElement called on type containing *types.TypeParam` — a
  govulncheck/x-tools generics bug. A panicking gate reported nothing
  and looked green. The target now also runs binary mode against the
  built artifact, and *that* is the hard gate: it needs no SSA
  analysis, so it survives toolchain skew. It is what found all three
  CVEs above; source mode never got far enough to see them.

### Added

- **Cloud KMS is configurable in `pg_hardstorage.yaml`** ([#44]). The
  top-level `kms.providers[]` block and per-deployment `kek_ref` that
  every KMS how-to documented now exist in the config schema; following
  those pages previously failed the strict loader outright
  (`field kms not found in type config.Config`).
- `doctor` reports `config.kek_ref_unknown_scheme` when a deployment's
  `kek_ref` names a scheme no provider in the running build claims.

### Fixed

- **Agent-driven backups can use a cloud KEK** ([#44]). A cloud KEK was
  selectable only through `--kek` / `--kms-config` on an individual
  command, so the scheduler and the control-plane executor — neither of
  which has a command line — always fell back to the local `kek.bin`.
  Both now resolve the deployment's `kek_ref` through the same resolver
  `backup` uses (`runner.ResolveEncryption`), as do `wal push`,
  `wal stream`, `restore`, `verify`, `partial restore` / `partial dump`,
  and recovery drills. A declared `kek_ref` whose provider fails to open
  now fails the run instead of silently downgrading custody.
- **Recovery drills of a cloud-KMS deployment could never pass.** A drill
  builds its restore internally and takes no KMS flags, and its DEK
  resolver hardcoded a nil provider config — so any deployment whose
  provider needs an explicit region or credential failed every drill, and
  `doctor` reported its backups as unproven (CRITICAL).
- `helm-sidecar-chart.md` documented a third, non-existent config shape
  (`kms.<name>.type` / `key_id`); corrected to the real schema.
- **`azure-key-vault://` was never a valid KEKRef scheme.** The
  encryption tutorial and `kms --help` both advertised it; the provider
  registers `azure-kv`. Following either produced
  `kms: unknown KEKRef scheme` on the first encrypted backup.
- **Three more families of unloadable documented config**, found by the
  new schema meta-test and each the same defect as [#44] — the loader is
  strict, so these did not degrade, they refused the whole file:
  - `sinks[].filter` (7 sink how-tos + the operator guide). Sink plugins
    read `min_severity` from `config:`; the `components` allowlist was
    never implemented by any sink and has been removed from the docs.
  - `deployments[].worm` / `worm_retention` (`pci-dss.md`). WORM is a
    repository property set by `repo init --worm-mode/--worm-retention`,
    deliberately init-time only.
  - `deployments[].extras` (`repository-scp.md`, `repository-sftp.md`) —
    see the known issue below.

- **The `scp://` storage backend was unusable and now works.** It reads
  its credentials from a storage-plugin `extras` map, but nothing
  populates that map in production — `storage.Open` builds the config
  from the URL alone — and unlike `sftp://`, `scp://` had no
  environment fallback. Every operation failed at open with
  `scp: extras.known_hosts is required`, so the backend could not be
  used at all. It now reads `PG_HARDSTORAGE_SCP_KNOWN_HOSTS`,
  `…_IDENTITY_FILE`, `…_IDENTITY_PASSPHRASE`, and `…_PASSWORD`,
  matching the sftp plugin's precedence exactly.

  The defect survived a full contract suite because every storage
  suite constructs `StorageConfig` by hand *with* `Extras` populated,
  which the real caller never does. New container-backed tests
  (`internal/plugin/storage/wiring_e2e_test.go`) drive `storage.Open`
  — the production entry point — for `scp://`, `sftp://` and `s3://`,
  so a backend reachable only from a test harness now fails CI.

- **`RenameIfNotExists` disagreed across backends.** `fs://` created the
  destination's parent directory (`os.MkdirAll`) and `s3://` has no
  directories at all, but `scp://` and `sftp://` failed with a bare
  "No such file or directory" when the destination prefix did not exist
  yet. Same documented operation, four backends, two answers. Current
  callers rename within a single directory — a manifest's staging file
  sits beside its final key — so nothing broke in practice, but a caller
  that renamed across prefixes would have worked on a local repo and
  failed on an SSH one. Both SSH plugins now create the parent, matching
  the `fs` reference.

  The contract suite could not have caught this: every rename case used
  `ren/src` → `ren/dst`, one prefix, so the destination directory always
  already existed. A `RenameIfNotExists_AcrossPrefix` case now covers it
  for every backend.

- **The storage contract contradicted itself on `ContentSHA256`.**
  `PutOptions.ContentSHA256` documented an unconditional "the plugin
  MUST verify after writing and return `ErrChecksumMismatch`", while
  `Capabilities.VerifiesContentSHA256` — twenty lines above it in the
  same file — documented the verification as opt-in, advertised only by
  `fs`. S3, Azure, GCS, SFTP and SCP deliberately ignore the field and
  rely on transport-layer integrity, and `internal/repo.CAS` correctly
  gates on the capability (computing the hash unconditionally cost ~9%
  of wal-stream CPU). The behaviour was right; the contract text was
  not, and a caller who trusted it would set `ContentSHA256` against S3
  believing they had post-write verification while nothing checked
  anything. The MUST is now explicitly conditional on the capability.

  A `ContentSHA256_MatchesAdvertisedCapability` contract case now pins
  the two together in whichever direction a backend claims, so a plugin
  that advertises the capability without implementing it fails.

### Known issues

- **`scp://` and `sftp://` are configurable only through
  `PG_HARDSTORAGE_SCP_*` / `PG_HARDSTORAGE_SFTP_*` environment
  variables**, not through `pg_hardstorage.yaml`. The plugins read a
  storage-plugin `extras` map that nothing populates; wiring a config
  surface to it is follow-up work.

- **The LLM helper's bundled runbooks were stale.** The corpus the
  assistant serves mid-incident is a `go:embed` copy refreshed by
  `make sync-llm-docs`; it was never re-run after the CLI-example
  corrections, so R2 still told operators to use `audit append --type
  kms.shred --kek-ref`, flags that no longer parse. Re-synced, and now
  enforced by a test.

### Testing

- A meta-test validates every config snippet in `docs/` against the real
  `config.Config` schema, so a page can't advertise an invented key
  again. It records (and requires the removal of) three pre-existing
  drift families it found: `sinks[].filter`, `deployments[].extras`, and
  `deployments[].worm`/`worm_retention`.
- `TestBundledCorpusMatchesCanonicalDocs` fails whenever a bundled LLM
  doc drifts from its canonical source. The Makefile claimed CI checked
  this; it didn't.
- `TestDocsKEKRefSchemesAreRegistered` checks every KEKRef scheme the
  docs teach against `kms.DefaultRegistry` — the check that would have
  caught `azure-key-vault://`.

[#46]: https://github.com/cybertec-postgresql/pg_hardstorage/issues/46
[#45]: https://github.com/cybertec-postgresql/pg_hardstorage/issues/45
[#44]: https://github.com/cybertec-postgresql/pg_hardstorage/issues/44

## [1.0.17] — 2026-07-27

### Failure-class test harnesses

Five new test harnesses target the failure *classes* the corruption
audits kept finding, so the next bug of each shape is caught
structurally instead of by the next audit:

- **Repo model checker** (`TestModelCheck_*`): seeded random operation
  sequences (plant/delete/undelete/gc/rotate/hold/replicate) through
  the real CLI paths, with global invariants checked after every step
  — every live manifest restorable, every live incremental's chain
  live, the audit chain verifying, the replica never lying. Fixed
  seeds run in CI; a 2000-op randomized run joins the nightly
  chaos-soak workflow, logging its seed for exact replay.
- **Idempotency sweep** (`TestIdempotencySweep`): every maintenance
  command run twice; the second run must succeed and leave durable
  repo bytes unchanged. Already caught (and this change fixes) a
  wart in the shared-DEK rotation migration: re-runs rewrote the
  key slots with fresh nonces on every invocation.
- **Fault sweep** (`TestFaultSweep`): one injected storage failure at
  every call position of each maintenance command; a faulted run must
  fail loudly, and a clean re-run must converge to the fault-free
  reference state.
- **Golden repo** (`TestGoldenRepo_StillReadable`): a frozen
  miniature repo committed under testdata/ (manifests, encrypted
  chunks, shared-DEK object, audit chain) that every future build
  must open, verify, decrypt, and restore byte-exact — the
  format-drift class (verify-sandbox major, verify-anchor indexing)
  caught before release instead of in the field.
- **Encryption path parity**
  (`TestEncryptionResolutionParity_CLIvsAgent`): the CLI and agent
  must resolve backup encryption identically for every keyring state.

### Internal invariant assertions (fail-closed)

New `internal/invariant` package: `invariant.Assert` panics with a
greppable "invariant violation:" prefix when an internal assumption —
a condition that can only be false if the BINARY has a bug — is
violated. Deliberately fail-closed with no production off-switch:
for a backup tool, crashing one run is always cheaper than letting
corrupted internal state flow into a committed artifact; the agent's
schedule engine already converts task panics into task failures, so
one impossible state aborts that task without killing the fleet.
Environmental conditions (storage errors, hostile data, races) remain
ordinary error handling.

Assertions added where the corruption hunts showed invariant-shaped
failure modes: WAL sink hand-off (only exactly-full, segment-aligned
segments may commit), manifest commit (attestation must exist after
signing), lease renewal (must strictly extend expiry — fencing
monotonicity), CDC chunker (boundary within (0, max], ≥ min
mid-stream — a drifting boundary silently regresses dedup repo-wide),
and shared-DEK resolution (never the all-zero key). Additionally,
`Manifest.Validate` now refuses an inverted LSN range (stop before
start), which previously committed green and could never reach
consistency.

### Corruption hunt, round three: nine more fixes

Fresh audits of replication/heal/standby/timetravel and the audit
chain / control plane / config layers, plus the remaining round-two
backlog:

- **Audit chain: a stale head pointer could silently destroy a
  committed event.** The pointer update after an Append is
  best-effort; a crash left it stale-but-valid, the next Append
  recomputed the same sequence, and on a no-conditional-put backend
  its Put overwrote the committed event — with the replacement linking
  PrevHash correctly, so even `VerifyChain` stayed green. Append now
  probes the slot first and relinks instead of writing.
- **`audit verify-anchor` cried tamper on healthy repos.** It resolved
  the anchored event by list index == sequence, which breaks under
  mixed key layouts and WORM retention pruning. Resolution is now by
  parsed sequence.
- **Config drop-in overlays silently dropped retention, TDE,
  audit-anchor, SLO, residency, and classification** for any
  deployment defined in two files — the operator's `keep_monthly: 60`
  drop-in vanished and rotate pruned with the default policy.
  `mergeDeployment` now carries every field, with a reflection canary
  test that fails when a future field lacks a merge arm.
- **Replication committed manifests to the replica over missing or
  failed chunks** — a DR replica that lies about restorability,
  permanently invisible to `replicate verify` once the source copy is
  GC'd. Manifests (backup and WAL) are now withheld unless every
  referenced chunk landed.
- **Timetravel with an LSN target always picked the newest backup**,
  so every historical-LSN session failed at the restore reachability
  gate (PG cannot rewind) — the feature's primary use case was
  inoperative. The picker now selects the latest backup with
  `StopLSN <= target`.
- **A hold on an incremental wedged retention for the whole
  deployment**: the held child left the delete batch, its parent
  stayed in, and the chain guard refused the entire batch — every
  scheduled rotate deleted nothing for the hold's lifetime. The hold
  filter is now chain-aware (ancestors kept as `held_chain_anchor`,
  the rest of the sweep proceeds).
- **The pre-backup ENOSPC gate projected from the latest manifest
  regardless of type**: weekly fulls were projected at the daily
  incremental's size (gate false-passes, full dies mid-run on ENOSPC)
  and incrementals at the full's size (cheap scheduled backups falsely
  refused). Projection is now type-aware.
- **Restore resume trusted the checkpoint blindly**: after an OS
  crash the checkpoint can claim files the filesystem lost (parent
  dirs were never dir-fsynced), yielding a datadir with missing
  relation files — silent for TDE/pre-manifest sources, a wedged
  retry loop otherwise. Resume now stats each claimed file against
  its manifest size and re-materialises on mismatch.
- **Timeline-history capture on failover was one-shot**: a transient
  error during the unrepeatable promotion window skipped both the
  capture and the backfill, silently capping
  `recovery_target_timeline=latest` at the previous timeline in
  streaming-only HA. Capture now retries with backoff, escalates to
  CRITICAL on final failure, and no longer blocks the backfill.

Still documented for a future round: heal's plaintext re-verify
ordering on partial heals, control-plane job requeue after lost
claims, backup WAL-range coverage validation, timeline-history
archival in plain `wal stream`, `.deferred-*` staging reaper,
shared-DEK nonce budget.

### Corruption hunt, round two: eight more fixes

A second audit round (restore/rotate/tarsink, S3/init/manifest, plus
the backlog from round one) fixed eight more corruption and
backup-availability bugs, each with a regression test:

- **KEK rotation could permanently destroy backups.** The manifest
  rewrite was Put-tmp → DELETE original → rename, and a rename failure
  "cleaned up" by deleting the tmp — destroying the only copy at the
  primary key; the backup vanished from every listing AND from GC's
  reference walk, so the next sweep reaped its chunks. The rewrite is
  now a single atomic overwrite: a valid manifest body exists at the
  key at every instant.
- **KEK rotation bricked all future backups and made rotated backups
  unrestorable.** Rotation rewrapped manifests but left the
  authoritative shared-DEK object under the retired KEK (every
  subsequent backup and `wal stream` then hard-failed), and the
  rotated manifests' new `local:v2`-style KEKRef was rejected by every
  shipped resolver. Rotation now migrates the shared-DEK object (same
  DEK, new wrap) including the fixed `local:default` alias the
  CLI/agent stamp; `ResolveOrMint` falls through to the manifest scan
  when the object won't unwrap; and both resolvers route any `local:*`
  ref to the keyring.
- **`backup undelete` could resurrect an incremental whose parent
  chain stayed tombstoned** — a live-listing backup every restore
  refuses, hardening into permanent loss once GC grace expired. The
  chain is now walked and the first dead ancestor named.
- **`repo.Open`'s forward-format gate failed OPEN on transient read
  errors** of `_repo_version.json` (throttle, partition, IAM deny were
  treated like "marker absent = v1.0"), letting an old binary mutate a
  future-format repo whose manifests it skips as "malformed" — and GC
  would reap chunks those manifests reference. Now fail-closed; only a
  definitive not-found takes the legacy path.
- **`Manifest.Validate` accepted structurally unrestorable chain
  shapes**: an incremental with no (or a self-referential) parent
  committed green, skipped the parent-liveness and chain-protection
  guards, and failed only at restore. Validate now enforces
  type/parent/timeline invariants at the same gate that already
  refuses undecryptable encryption shapes.
- **The S3 plugin claimed `ConditionalPut: true` for every endpoint.**
  On S3-compatible endpoints the claim was a guess — and it disables
  the exact mitigations built for honest-false backends (audit-chain
  read-back, lease warning). Custom `?endpoint=` overrides now report
  false unless the operator vouches with `?conditional_put=native`
  (MinIO ≥ 2024, R2); AWS proper stays true.
- **`verify --full` fabricated "skipped" out of environment
  failures**: a sandbox that couldn't see the freshly-written
  `backup_manifest` (bad bind-mount, remote DOCKER_HOST, permissions)
  was classified as "manifest was not captured" and exited 0. When the
  caller captured a manifest, that stderr is now a real failure.
- **Backends with no enforceable durability (sftp/scp) were silent.**
  The backup runner now emits `backup`/`durability_unenforceable` and
  `wal stream` emits `wal.durability`/`unenforceable` when a backend
  has neither a real barrier nor inline-durable writes — a
  storage-host power loss there can tear chunks under a committed
  manifest or lose WAL the slot has advanced past.

Documented, not yet fixed (next round): timeline-history archival in
plain `wal stream`, backup WAL-range coverage validation, restore
resume re-validation after host crash, legal-hold chain-aware
retention filtering, type-aware capacity projection, `.deferred-*`
staging reaper, shared-DEK nonce budget.

### Corruption hunt: five fixes for silent data-corruption and backup-availability bugs

A three-way audit of the write path, the WAL pipeline, and the
encryption/agent layers found and fixed five bugs, each with a
regression test:

- **Agent-scheduled backups were silently unencrypted.** Neither the
  agent's schedule engine nor the control-plane executor ever consulted
  the keystore, so every scheduled backup wrote plaintext even in a repo
  initialised with `--encrypt` — and plaintext-hash dedup then welded
  plaintext and encrypted backups onto the same chunks (manifests
  claiming aes-256-gcm referencing cleartext chunks, breaking the
  crypto-shred guarantee; unencrypted manifests referencing GCM
  envelopes they can never decrypt). Both paths now resolve encryption
  exactly like the interactive CLI, and a corrupt KEK fails the backup
  loudly instead of silently falling back to plaintext.
- **One wedged task starved the agent's scheduler forever.** Tasks fire
  serially with no deadline, so a single hung Run (wedged docker daemon
  during a drill, D-state I/O during a backup) silently stopped every
  scheduled backup on the agent while the process looked healthy. Every
  task now runs under a wall-clock ceiling (per-task override, default
  2× its own cadence clamped to [1h, 48h]); at the ceiling its context
  is cancelled and — if it still won't return — the task is abandoned
  with a loud error so the other tasks keep firing.
- **`repo gc --apply` (and `repair chunks --orphans --apply`) could
  delete chunks an in-flight backup had deduplicated against.** The
  chunk-age floor only protects chunks a running backup *wrote*; chunks
  it *deduplicated against* are old by definition, so a sweep could
  reap them and a signed, committed backup would reference deleted
  chunks. Both sweeps now refuse while any unexpired backup lease
  exists (an unreadable lease refuses too — never "couldn't check,
  deleting anyway") and re-collect references immediately before
  deleting so backups committed since the first snapshot keep their
  chunks. Dry-runs are unaffected.
- **The WAL sink missed gaps that landed exactly on a segment
  boundary.** Right after a segment hand-off there is no current
  segment, so both per-segment guards went blind and a skip to a later
  segment's first byte would commit past an unrecorded hole — PG then
  recycles the missing WAL and the gap becomes permanent. The sink now
  enforces strict stream-level contiguity: every record must start
  exactly where the previous one ended.
- **The streamer reported the apply LSN as the RAM-buffered receive
  position.** Under `synchronous_commit=remote_apply` (reachable via
  `--skip-preflight` or a post-start GUC change) the primary would ACK
  commits whose WAL existed only in the streamer's volatile buffers —
  acknowledged transactions lost if the host died. The standby status
  update now reports apply = flush (the durably-synced position); the
  fast-shutdown drain (issue #101) is unaffected because it only needs
  the write field.

## [1.0.16] — 2026-07-24

### Integrity program: scheduled drills, contract enforcement, chaos soak

Three layers landed to keep "backup that won't restore" in the class of
bugs machines catch first (see the new
[Integrity testing](https://pghardstorage.org/operations/integrity-testing/)
page):

- **Scheduled recovery drills.** Deployments can now declare a
  `schedule.drill` alongside `backup` and `rotate`
  (`pg_hardstorage schedule db1 'daily_at 03:00' --task drill`); the
  agent restores the latest backup into a scratch dir and verifies it on
  that cadence, recording every verdict in the repo's drill history.
  `doctor` gained drill-freshness checks — `recovery.drill_never_run`
  (notice), `recovery.drill_failing` (critical), and
  `recovery.drill_stale` (critical, tunable via `--drill-max-age`,
  default 7d) — plus a `drills` report section for dashboards.
- **Storage-contract concurrency cases are now mandatory.** Every
  backend runs `ParallelPuts_SingleWinner` and
  `ParallelOverwrites_NoTornContent`; a backend claiming
  `ConditionalPut: true` that loses the race is red, while an honest
  `false` skips and its callers degrade loudly instead: the backup
  runner emits a `lease_unenforceable` warning event and the audit
  chain read-back-verifies every slot it wins. The scp backend is now
  contract-tested against a real `sshd`, which surfaced (and fixed) a
  session-open retry needed under `MaxSessions` backpressure. A
  dedicated CI job runs these with the `DEMAND` env vars so the fixtures
  can never silently skip.
- **Nightly chaos soak with a restore-proof gate.** A seeded random
  fault loop (Patroni switchovers, leader pauses, concurrent-backup
  bursts) over a real 3-node cluster with an encrypted repo and a
  continuous — never restarted — `wal stream`. Pass requires every
  committed backup to `verify --full` AND restore, a gap-free
  `wal audit`, and exactly one shared-DEK object; the seed is logged so
  any failure replays deterministically.

The very first soak run caught a real bug, now fixed: **`verify --full`
(and the agent's scheduled verify) ran the wrong-major sandbox for every
backup.** Manifests store the plain PostgreSQL major (`pg_version: 17`),
but the sandbox-major helper expected `server_version_num` form and
divided by 10000 — every backup fell back to the PG18 default sandbox,
whose `pg_verifybackup` rejects a PG17 `pg_control` with "CRC is
incorrect". Healthy, fully-restorable backups were reported as
corrupt. The helper now recognises plain majors; the failure path also
gained the actual pg_verifybackup output in its error message (it
previously pointed at a `tool_stdout` body field that error results
don't carry, and dropped stderr — where pg_verifybackup writes its
findings — entirely).

Also fixed while validating: agent-scheduled drills against an
encrypted repo failed instantly because the drill task did not wire the
keystore KEK resolver the way the `recovery drill` CLI does — it now
does.

## [1.0.15] — 2026-07-23

### Fix: Patroni switchover hang — the real cause (#34, supersedes 1.0.14)

1.0.14 shipped an INCOMPLETE fix for #34 based on a wrong root cause
(reconnect backoff after a server `CopyDone`). Reproducing the bug on a
real 3-node Patroni cluster proved that theory wrong — the old leader
still hung — and revealed the actual mechanism:

When the demoting primary shuts down, its physical walsender waits for
our client to FLUSH-confirm the shutdown-checkpoint LSN before it will
exit. But the checkpoint lands in a *partial* WAL segment, and the WAL
sink only advances its flush position (`SyncedLSN`) when a full 16 MiB
segment commits — so our reported flush never reaches the checkpoint.
PG then spins reply-requested keepalives forever (measured ~1.26M in
~2.5 min), the postmaster can't finish its fast-shutdown, and the
Patroni demote hangs until the streamer is restarted.

The streamer now detects that spin (a burst of caught-up keepalives
with no new WAL) and ends the stream so the walsender can exit; the
reconnect routes to the new primary and resumes gap-free from the real
flush position. The ineffective 1.0.14 reconnect-backoff change is
reverted. Verified end-to-end against a real 3-node Patroni switchover
(old leader now rejoins as a streaming replica in ~90 s; without the
fix it stays stuck at `stopping` indefinitely) plus a unit regression
for the spin detector.

## [1.0.14] — 2026-07-23

### Fix: Patroni switchover hang (#34)

`wal stream` prevented an old leader from restarting during a Patroni
switchover. When the demoting primary's walsender ended the COPY
(`CopyDone`), the stream reconnected on its 1-second floor — but during
`demote in progress` the old node is still a read-write primary, so
`target_session_attrs=primary` routed the reconnect straight back to it
and re-armed a walsender that blocked the very fast-shutdown in
progress. Server-initiated stream ends now reconnect with an escalating
grace delay (≥ 10 s), giving the node walsender-free windows to finish
the demote; the next reconnect then routes to the new primary.

### Fix: concurrency audit — ten bugs (three demonstrated under `-race`)

A three-way review (WAL pipeline, backup/CAS/storage, daemons/audit):

- **Backup lease mutual exclusion** was breakable: a stale-reclaim race
  let two backups hold the same deployment lease, and the runner only
  logged lease loss instead of aborting. Reclaim/renew rewritten to
  recheck → overwrite-in-place → settle-verify; lease loss now aborts
  the backup.
- **scp / sftp fake `IfNotExists`**: the `stat` + `mv -T` emulation let
  two writers both "win" (rename overwrites), silently forking the
  shared DEK (the #31 data-loss class) and destroying committed audit
  events on those backends. Commit is now atomic `ln -T` (link(2)
  EEXIST); sftp advertises `ConditionalPut` only with the hardlink
  extension; the shared-DEK mint reads back for defense in depth.
- **`wal stream` could hang forever** after a status-tick send failure
  on a quiet stream (the receive now unblocks on the per-call context).
- **System-identifier continuity** is rechecked on every reconnect, so
  a failover onto a different cluster can no longer interleave foreign
  WAL into the deployment's lineage.
- The **first Ctrl-C** no longer kills the graceful WAL stop before
  `pg_switch_wal` runs, and `clean_stop` is reported honestly.
- **WAL-gap records** are persisted via a detached context on shutdown
  and the CRITICAL escalation fires on every unpersisted exit path.
- The **agent registry** no longer shares a mutable slice with in-flight
  `/v1/agents` responses; the **syslog sink** no longer closes a
  connection out from under a concurrent emit.

### Fix: restore verification false failure

The post-restore `SELECT 1` probe rejected psql stderr diagnostics
(e.g. the collation-version `WARNING` when a cluster built against one
glibc starts on another) mixed into combined output; it now checks only
the final result row.

## [1.0.13] — 2026-07-22

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

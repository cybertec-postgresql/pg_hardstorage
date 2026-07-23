// Package runner wires the full backup pipeline:
//
//	conn open -> version probe -> IDENTIFY_SYSTEM -> BASE_BACKUP through tarsink
//	-> manifest assembly -> sign -> commit -> post-commit verify
//
// One exported function — Take — drives all of it. The CLI's `backup`
// command is a thin shim over Take.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/tracing"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// TakeOptions configures a single backup run.
//
// Required fields: PGConnString, RepoURL, Deployment, Signer, Verifier.
// Everything else has sensible defaults.
type TakeOptions struct {
	// PGConnString is a libpq URI / DSN for the source database. The
	// connection user needs the REPLICATION attribute (CREATEROLE +
	// ALTER ROLE ... REPLICATION) and pg_hba.conf must permit
	// `replication` from the agent's IP.
	PGConnString string

	// RepoURL is the destination repository (file:///, s3://, ...).
	// The repo MUST already exist (created via `pg_hardstorage repo init`).
	RepoURL string

	// Deployment is the logical deployment name. Becomes part of the
	// backup ID and the manifest's `deployment` field.
	Deployment string

	// Tenant scopes the backup for multi-tenant deployments. Empty
	// defaults to "default" — single-org users never see the field.
	Tenant string

	// Signer signs the manifest. Required: we don't write unsigned
	// manifests.
	Signer *backup.Signer

	// Verifier verifies the manifest immediately after commit, as a
	// post-commit sanity check. Mismatched key here means we just wrote
	// a manifest the rest of the system can't trust — surfaced as an
	// error so the caller knows. Defaults to deriving from Signer.
	Verifier *backup.Verifier

	// Label is the human-readable backup label PG embeds in
	// backup_label. Defaults to the BackupID we generate.
	Label string

	// Fast forces an immediate CHECKPOINT. Defaults to false.
	Fast bool

	// IncludeManifest requests PG's own backup_manifest in the stream.
	// Defaults to true so we have it available for cross-checks.
	IncludeManifest bool

	// IncludeWAL embeds the WAL files needed to make the backup
	// self-consistent into the basebackup tar stream — equivalent
	// to `pg_basebackup -X stream` on the wire.  Defaults to false
	// (production deployments use `wal stream` for continuous
	// archiving; embedding WAL doubles the backup size for short
	// retention windows).  The testkit flips this on for self-
	// contained correctness scenarios so the basebackup alone
	// carries every WAL segment recovery needs.
	IncludeWAL bool

	// InactivityTimeout overrides the default streaming watchdog. Zero
	// uses the basebackup default (90 s).
	InactivityTimeout time.Duration

	// StallTimeout is the upper bound on how long the backup may go
	// WITHOUT emitting any progress event before it's force-aborted
	// with a structured `backup.io_starved` error.  Zero (the default)
	// disables the guard — callers that don't care about runaway
	// hangs (interactive CLI users hitting Ctrl-C) leave it at zero.
	//
	// Why this exists: under heavy concurrent host disk pressure
	// (soak testing reproducer: 4 driver slots all hammering
	// the same daemon), the chunker / tarsink path blocks at
	// `wchan = blk_mq_get_tag` for tens of minutes with no progress
	// events emitted.  The orchestrator above sees no events, no
	// error, and hangs indefinitely waiting for backup.completed.
	// With StallTimeout set, the watchdog cancels the context and
	// the caller gets a clean failure they can retry / triage instead
	// of a wedged slot.
	//
	// Soak driver sets this to 5 min by convention: long enough that
	// a slow-but-real backup on a degraded disk doesn't false-positive,
	// short enough that a wedged slot recovers in the iteration's
	// natural cadence.
	StallTimeout time.Duration

	// Encryption, when non-nil, encrypts every chunk written by this
	// backup with AES-256-GCM under a DEK that's repo-scoped per
	// KEKRef: the runner reuses any existing wrapped DEK already in
	// the repo for the same KEK, and only generates a fresh DEK on
	// the first encrypted backup in scope.  Each manifest still
	// records its own freshly-wrapped form of that DEK (the wrap
	// nonce is non-deterministic), and restore unwraps it the same
	// way it always did.
	//
	// Why repo-scoped instead of per-backup: the CAS deduplicates
	// chunks by plaintext hash across every backup, so a chunk
	// written once is referenced by every later manifest that hashes
	// the same plaintext.  Per-backup random DEKs left those shared
	// chunks unreadable from every manifest after the first — issue
	// #28.  Per-repo DEK keeps cross-backup dedup intact.
	//
	// Setting Encryption is a per-backup choice.  A repo can hold a
	// mix of encrypted and unencrypted backups; the manifest's
	// EncryptionInfo determines which kind any given backup is.
	Encryption *EncryptionConfig

	// Incremental, when non-nil, requests a PG 17+ incremental
	// backup against the named parent backup. The runner reads the
	// parent's manifest, extracts the BackupLabel + per-file PG
	// manifest data PG needs for incremental anchoring, and passes
	// it to BASE_BACKUP via the INCREMENTAL option.
	//
	// Requires PG 17+ on the source DB AND `summarize_wal = on`
	// already enabled when the parent backup was taken (PG can't
	// retro-summarise WAL). Restore-side wiring (`pg_combinebackup`)
	// is a follow-up — for now an incremental backup commits with
	// `Type = "incremental_lsn"` and `ParentBackupID = parent` so
	// the restore path can detect the chain even before chain-walk
	// support lands.
	Incremental *IncrementalConfig

	// OnEvent receives progress events as the backup runs. Optional;
	// nil means events are discarded. Events are emitted on the run
	// goroutine, so the callback must return promptly.
	OnEvent func(*output.Event)

	// OnFile, when non-nil, fires once per regular file as it
	// commits its last chunk to the CAS — issue #9.  Wired into
	// tarsink via WithFileObserver so a `--verbose` renderer can
	// stream per-file progress (path, size, chunk count, dedup
	// savings) without waiting for manifest commit.  Callbacks
	// must NOT block; they back-pressure the basebackup pipe.
	OnFile func(tarsink.FileStats)

	// Actor identifies who initiated the backup, for the hash-chained
	// audit log. Free-form (operator email, agent ID, "scheduler",
	// etc.). Empty when the runner is invoked from a path that
	// doesn't track an explicit principal — the audit event still
	// records the action, just without an actor field.
	Actor string

	// SourceTDE, when non-nil, declares that the source PostgreSQL
	// has Transparent Data Encryption enabled (CYBERTEC PGEE,
	// pg_tde, EDB TDE, ...).  The runner stamps the manifest's
	// SourceTDE field accordingly; restore-time tooling consults
	// it to refuse vanilla-PG targets and to relax pg_verifybackup
	// expectations.  Operator-supplied via deployment config
	// (config.TDEConfig.Enabled) or per-invocation flag.  See
	// docs/explanation/tde-awareness.md.
	//
	// The backup wire protocol (BASE_BACKUP, START_REPLICATION)
	// itself does NOT change under TDE — PGEE / pg_tde decrypt at
	// the replication boundary so bytes-on-the-wire are plaintext.
	// This field is purely a propagation to restore-time tooling.
	SourceTDE *backup.SourceTDEInfo

	// SkipLease disables the per-deployment backup lease that
	// otherwise prevents two concurrent backups of the same
	// deployment (across processes / agents sharing the repo) from
	// running at once.  Default false: the lease is acquired right
	// after the repo opens and released when the backup ends; a
	// crashed holder's lease expires and is reclaimed automatically.
	// Set true only for deliberately-overlapping runs where the
	// operator accepts the doubled source load.
	SkipLease bool

	// LeaseOwner is the identity recorded in the backup lease (shown
	// in the "already in progress" error a blocked second backup
	// gets).  Empty defaults to "<hostname>/pid-<pid>".
	LeaseOwner string
}

// EncryptionConfig configures per-backup encryption.
//
// Two custody shapes are supported:
//
//   - Local KEK (the v0.1..shape): KEK is the on-disk
//     32-byte master key; the runner wraps the per-backup DEK
//     with AES-256-GCM and the KEK locally.  Provider is nil.
//
//   - Cloud KMS: Provider is non-nil; KEK is unused.
//     The runner generates a per-backup DEK, calls
//     Provider.WrapDEK to ask the cloud KMS to wrap it server-
//     side, and records the resulting bytes (+ Provider.KEKRef())
//     in the manifest's encryption block.  The KEK never reaches
//     the operator's host.
//
// In both shapes the manifest stores `KEKRef` + the wrapped
// DEK; restore consults `keystore.UnwrapDEK` which dispatches
// by KEKRef scheme back to the correct path.
type EncryptionConfig struct {
	// KEK is the on-disk master key (local-custody shape).
	// Must be exactly 32 bytes.  Unused when Provider is set.
	KEK [encryption.KeyLen]byte

	// KEKRef is the manifest-stamped identifier the restore-
	// time operator consults to pick the right unwrap path.
	// Examples: "local:default", "aws-kms://arn:aws:kms:...".
	// When Provider is set, KEKRef is auto-populated from
	// Provider.KEKRef() — operators don't have to keep the two
	// in sync.
	KEKRef string

	// Provider, when non-nil, is the cloud-KMS provider that
	// wraps the per-backup DEK.  Caller manages the lifecycle
	// (open before TakeBackup, close after).
	Provider kms.Provider
}

// IncrementalConfig configures a PG 17+ incremental backup. The
// parent's PG-emitted backup_manifest body (NOT pg_hardstorage's
// JSON manifest) is what BASE_BACKUP INCREMENTAL consumes — it's
// the JSON shape pg_basebackup writes to backup_manifest at the
// root of every full backup. We extracted it on the parent commit
// and re-feed it here.
type IncrementalConfig struct {
	// ParentBackupID is the pg_hardstorage manifest ID of the
	// parent backup. Recorded in the new manifest's
	// `parent_backup_id` field so restore can walk the chain.
	ParentBackupID string

	// ParentPGManifest is the verbatim bytes of PG's
	// backup_manifest from the parent. PG validates the JSON +
	// confirms WAL summaries cover the gap; we just pass through.
	ParentPGManifest []byte
}

// Result is the structured outcome of a successful TakeBackup. It maps
// 1:1 to the JSON shape the CLI emits and is what the dispatcher hands
// back to consumers.
//
// Duration serializes as WHOLE MILLISECONDS under the frozen key
// duration_ms (MarshalJSON below): a raw time.Duration under a _ms key
// would emit nanoseconds, inflating every consumer's reading 1e6x.
type Result struct {
	BackupID         string        `json:"backup_id"`
	Deployment       string        `json:"deployment"`
	Tenant           string        `json:"tenant"`
	PGVersion        int           `json:"pg_version"`
	SystemIdentifier string        `json:"system_identifier"`
	StartLSN         string        `json:"start_lsn"`
	StopLSN          string        `json:"stop_lsn"`
	Timeline         uint32        `json:"timeline"`
	StartedAt        time.Time     `json:"started_at"`
	StoppedAt        time.Time     `json:"stopped_at"`
	Duration         time.Duration `json:"-"`

	// File and chunk counters — useful for the audit log and for
	// progress reporting.
	FileCount        int   `json:"file_count"`
	TablespaceCount  int   `json:"tablespace_count"`
	LogicalBytes     int64 `json:"logical_bytes"`
	UniqueChunkCount int   `json:"unique_chunk_count"`
	TotalChunkRefs   int   `json:"total_chunk_refs"` // sum across all files
	UniqueChunkBytes int64 `json:"unique_chunk_bytes"`

	// Dedup records how many chunks PutChunk actually wrote vs found
	// already present in the repo — measured at write time, so it
	// reflects real work done, distinct from the manifest-derived
	// UniqueChunkCount above. On a re-backup of a mostly-unchanged
	// database HitsStorage dominates; on a first backup it is zero.
	Dedup repo.DedupStats `json:"dedup"`

	// PrimaryKey is where the manifest landed in the repository. The
	// CLI prints it; orchestrators use it to fetch the manifest back.
	PrimaryKey string `json:"primary_key"`
}

// MarshalJSON emits duration_ms as whole milliseconds (see Result doc).
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result // no methods: avoids recursing into MarshalJSON
	return json.Marshal(struct {
		alias
		DurationMS int64 `json:"duration_ms"`
	}{alias(r), r.Duration.Milliseconds()})
}

// UnmarshalJSON is the inverse of MarshalJSON (ms → time.Duration).
func (r *Result) UnmarshalJSON(b []byte) error {
	type alias Result
	aux := struct {
		*alias
		DurationMS int64 `json:"duration_ms"`
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	r.Duration = time.Duration(aux.DurationMS) * time.Millisecond
	return nil
}

// Take executes the full backup pipeline:
//
//  1. Open the repo (must already exist).
//  2. Open a regular-mode PG conn, query server_version.
//  3. Open a replication-mode PG conn, run IDENTIFY_SYSTEM.
//  4. Run BASE_BACKUP through tarsink.Sink (chunker -> CAS).
//  5. Assemble a backup.Manifest from sink output + protocol metadata.
//  6. Sign + commit the manifest atomically (with replica copy).
//  7. Verify the just-committed manifest with the supplied Verifier.
//  8. Return the structured Result.
//
// Resource cleanup: every connection / handle opened by TakeBackup is
// closed before return, in success and failure paths alike.
//
// On any failure the partial state is left invisible at the manifest
// level: a partial chunk set in the CAS is harmless (orphan chunks GC
// reaps later, and a retry de-dupes them away). Only a successful Run
// produces a committed primary manifest.
func Take(ctx context.Context, opts TakeOptions) (*Result, error) {
	if err := validateOptions(&opts); err != nil {
		return nil, err
	}

	emit := opts.OnEvent
	if emit == nil {
		emit = func(*output.Event) {}
	}

	// Metrics: one deferred recorder catches every failure return path
	// below.  We only count a completion once the backup has actually
	// "started" (past PG identification + type resolution) so a config
	// or repo-open error doesn't show up as a failed backup of an
	// unknown type — those are surfaced via the returned error, not the
	// backup_completed_total{result="failure"} series.  The success path
	// sets completed=true and records its own metrics inline.
	backupTypeLabel := string(backup.BackupTypeFull)
	var started, completed bool
	defer func() {
		if started && !completed {
			metrics.BackupCompleted(opts.Deployment, backupTypeLabel, "failure")
		}
	}()

	// Stall watchdog.  When opts.StallTimeout > 0, every emit() also
	// stamps a "last progress" timestamp, and a goroutine cancels
	// ctx with backup.io_starved if no event arrives within the
	// window.  Disabled when StallTimeout is zero (the default for
	// interactive-CLI callers who want to keep their Ctrl-C
	// privilege).  See TakeOptions.StallTimeout comment for the
	// reproducer history.
	if opts.StallTimeout > 0 {
		var (
			stallCancel    context.CancelCauseFunc
			lastProgressMu sync.Mutex
			lastProgress   = time.Now()
		)
		ctx, stallCancel = context.WithCancelCause(ctx)
		defer stallCancel(nil)

		// Wrap emit so every event resets the watchdog.
		inner := emit
		emit = func(ev *output.Event) {
			lastProgressMu.Lock()
			lastProgress = time.Now()
			lastProgressMu.Unlock()
			inner(ev)
		}

		// Watchdog goroutine.  Polls every 30s; the granularity
		// is fine because StallTimeout is on the order of minutes.
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					lastProgressMu.Lock()
					stalled := time.Since(lastProgress)
					lastProgressMu.Unlock()
					if stalled > opts.StallTimeout {
						stallCancel(fmt.Errorf(
							"backup.io_starved: no progress event for %s (StallTimeout=%s) — "+
								"likely host disk saturation; check `iostat -x 1` and reduce concurrent backup load",
							stalled.Round(time.Second), opts.StallTimeout))
						return
					}
				}
			}
		}()
	}

	// Top-level span. Closed at function exit; child spans for the
	// expensive stages attach to it. The deployment + backup-id
	// attributes are populated as we learn them.
	ctx, span := tracing.Tracer().Start(ctx, "pg_hardstorage.backup",
		trace.WithAttributes(
			attribute.String("deployment", opts.Deployment),
			attribute.String("repo_url", opts.RepoURL),
			attribute.Bool("encrypted", opts.Encryption != nil),
		))
	defer span.End()

	startedAt := time.Now().UTC()

	// 1. Open the repo. Confirms the URL is a real pg_hardstorage repo
	//    (HSREPO present + schema matches) before we touch PG.
	repoMeta, sp, err := repo.Open(ctx, opts.RepoURL)
	if err != nil {
		return nil, mapRepoErr(opts.RepoURL, err)
	}
	defer sp.Close()

	// 1b. Backup lease.  Prevent a second concurrent backup of the
	//     same deployment — possibly from another agent/process that
	//     only shares this repo — from running at once.  A crashed
	//     holder's lease expires and is reclaimed automatically; a
	//     long backup is kept alive by the Maintain goroutine below.
	if !opts.SkipLease {
		// Loud degradation on backends without an atomic conditional
		// put (honest ConditionalPut=false): the lease's mutual
		// exclusion is not guaranteed there, and the operator should
		// know rather than discover it as two overlapping backups.
		if !sp.Capabilities().ConditionalPut {
			emit(output.NewEvent(output.SeverityWarning, "backup", "lease_unenforceable").
				WithSubject(output.Subject{Deployment: opts.Deployment}).
				WithBody(map[string]any{
					"message": "this storage backend cannot perform an atomic conditional write, so the backup lease cannot strictly guarantee a single concurrent backup; avoid running overlapping backups of this deployment from multiple hosts",
				}))
		}
		lease, lerr := backup.AcquireBackupLease(ctx, sp, opts.Deployment, backup.LeaseOptions{Owner: opts.LeaseOwner})
		if lerr != nil {
			if errors.Is(lerr, backup.ErrBackupInProgress) {
				// conflict.* namespace → ExitConflict, so a cron-driven
				// backup can tell "someone else is already running it"
				// from a real failure by exit code alone.
				return nil, output.NewError("conflict.backup_in_progress", lerr.Error()).Wrap(lerr)
			}
			return nil, output.NewError("backup.lease_failed",
				fmt.Sprintf("backup: acquire lease for %q: %v", opts.Deployment, lerr)).Wrap(lerr)
		}
		// Release with a background context so it still runs when the
		// backup's own ctx was cancelled (timeout / Ctrl-C).
		defer func() { _ = lease.Release(context.Background()) }()
		// Lease loss must ABORT the backup, not just log: at that point
		// another backup believes it owns the deployment, and running
		// both to completion is exactly what the lease exists to prevent
		// (the old callback only emitted a warning — concurrency audit).
		// Rebind ctx so everything downstream is cancelled on loss;
		// transient renew errors stay warning-only.
		bctx, cancelBackup := context.WithCancelCause(ctx)
		defer cancelBackup(nil)
		ctx = bctx
		leaseCtx, leaseStop := context.WithCancel(ctx)
		defer leaseStop()
		go lease.Maintain(leaseCtx, leaseLossAborter(opts.Deployment, emit, cancelBackup))
	}

	// Build the CAS with optional encryption + WORM retention. Per-
	// backup DEK generated here; the wrapped form lands in the
	// manifest below. When the repo's HSREPO carries a WORMPolicy,
	// we capture `now` once and propagate the same retention deadline
	// to every chunk + manifest committed in this run — keeps the
	// fleet-wide audit story coherent (every byte from this backup
	// retired at the same moment).
	wormNow := time.Now().UTC()

	// Dedup hints: seed the CAS with the chunk hashes from this
	// deployment's most recent prior backup. A re-backup of a mostly-
	// unchanged database then confirms each unchanged chunk with one
	// cheap Stat probe instead of re-compressing, re-encrypting and
	// re-uploading it. Strictly best-effort — any failure here just
	// means no hints (a normal, un-accelerated backup), never a
	// backup failure. A first backup finds no prior manifest and gets
	// an empty set, which is a no-op.
	hints, hintErr := loadDedupHints(ctx, sp, opts.Deployment)
	if hintErr != nil {
		emit(output.NewEvent(output.SeverityWarning, "backup", "dedup_hints_unavailable").
			WithSubject(output.Subject{Deployment: opts.Deployment}).
			WithBody(map[string]any{"error": hintErr.Error()}))
	} else if len(hints) > 0 {
		emit(output.NewEvent(output.SeverityInfo, "backup", "dedup_hints_loaded").
			WithSubject(output.Subject{Deployment: opts.Deployment}).
			WithBody(map[string]any{"hint_chunks": len(hints)}))
	}

	// Chunks are written DurabilityDeferred — no per-chunk fsync —
	// and made crash-durable by a single cas.Barrier after
	// BASE_BACKUP completes, before the manifest is committed.
	casOpts := []casdefault.Option{
		casdefault.WithCompressionLevel(repoMeta.Compression),
		casdefault.WithChunkDurability(storage.DurabilityDeferred),
		casdefault.WithDedupHints(hints),
	}
	cas := casdefault.NewWithRetention(sp, repoMeta.WORM, wormNow, casOpts...)
	var encryptionInfo *backup.EncryptionInfo
	if opts.Encryption != nil {
		// Pick the KEKRef we'll wrap under.  Provider supplies it
		// authoritatively in the cloud-KMS shape; the local-KEK
		// shape carries it on EncryptionConfig.
		var kekRef string
		if opts.Encryption.Provider != nil {
			kekRef = opts.Encryption.Provider.KEKRef()
		} else {
			kekRef = opts.Encryption.KEKRef
		}

		// Issue #28 + dek-reuse safety: the CAS dedups chunks by
		// plaintext hash across every backup, so all encrypted backups
		// under one KEK must share ONE plaintext DEK — otherwise a chunk
		// this backup dedups against an earlier one is wrapped under a
		// DEK that can't decrypt it. selectDEK reuses the repo's existing
		// DEK for this KEK, generating a fresh one ONLY when it has
		// positively confirmed none exists. A lookup error or an existing-
		// but-unusable DEK fails the backup rather than silently forking
		// the DEK into an unrestorable backup.
		dek, err := selectDEK(ctx, sp, kekRef, opts.Encryption)
		if err != nil {
			return nil, err
		}

		enc, err := aesgcm.New(dek[:])
		if err != nil {
			return nil, fmt.Errorf("backup: build encryptor: %w", err)
		}
		cas = casdefault.NewEncryptedWithRetention(sp, enc, repoMeta.WORM, wormNow, casOpts...)

		// Wrap the DEK for this manifest.  Two paths:
		//
		//   - Provider != nil: cloud-KMS shape.  Provider.WrapDEK
		//     is the server-side encrypt; Provider.KEKRef() is
		//     what we stamp on the manifest so restore picks the
		//     right provider via the kms registry.
		//   - Provider == nil: local-KEK shape.  Wrap in-process
		//     with AES-256-GCM under the on-disk KEK.
		//
		// Wrap is non-deterministic in both shapes (random IV /
		// KMS-side nonce), so the per-manifest WrappedDEK bytes
		// differ even when the underlying DEK was reused.
		var wrappedDEK []byte
		if opts.Encryption.Provider != nil {
			wrappedDEK, err = opts.Encryption.Provider.WrapDEK(ctx, dek[:])
			if err != nil {
				return nil, fmt.Errorf("backup: cloud-KMS wrap DEK: %w", err)
			}
		} else {
			wrappedDEK, err = encryption.Wrap(opts.Encryption.KEK, dek)
			if err != nil {
				return nil, fmt.Errorf("backup: wrap DEK: %w", err)
			}
		}
		encryptionInfo = &backup.EncryptionInfo{
			Scheme:          enc.Name(),
			KEKRef:          kekRef,
			WrappedDEK:      base64Encode(wrappedDEK),
			EnvelopeVersion: 2,
		}
	}
	manifestStore := backup.NewManifestStore(sp)

	// 2. Probe the PG version on a regular-mode connection. Replication
	//    mode can't run SHOW, and we want the version recorded on the
	//    manifest.
	pgVersion, err := probeVersion(ctx, opts.PGConnString)
	if err != nil {
		return nil, err
	}
	emit(output.NewEvent(output.SeverityInfo, "backup", "pg_probed").
		WithBody(map[string]any{"pg_version": pgVersion.Major, "raw": pgVersion.Raw}))

	// 3. Open the replication-mode connection. From here on, the conn
	//    is dedicated to BASE_BACKUP; we close it after the run.
	replConn, err := pg.Connect(ctx, opts.PGConnString, pg.ModeReplication)
	if err != nil {
		return nil, err
	}
	defer replConn.Close(ctx)

	identity, err := pg.IdentifySystem(ctx, replConn)
	if err != nil {
		return nil, fmt.Errorf("backup: IDENTIFY_SYSTEM: %w", err)
	}
	emit(output.NewEvent(output.SeverityInfo, "backup", "identified").
		WithBody(map[string]any{
			"system_id": identity.SystemID, "timeline": identity.Timeline,
			"xlogpos": identity.XLogPos,
		}))

	// 4. Generate the backup ID up-front so the label can mirror it.
	// Backup type is full unless an Incremental config is set, in
	// which case we tag it as incremental so retention + restore
	// can chain-walk correctly.
	backupType := backup.BackupTypeFull
	if opts.Incremental != nil && len(opts.Incremental.ParentPGManifest) > 0 {
		// Refuse incremental against pre-PG-17 servers up front. This
		// is an environment precondition (the operator's server is too
		// old), NOT a tool bug — classify it as a structured usage-class
		// error, not the generic "internal" (file-a-bug) code.
		if pgVersion.Major < 17 {
			return nil, output.NewError("backup.incremental_unsupported",
				fmt.Sprintf("backup: incremental backups require PostgreSQL 17+; source is PG %d", pgVersion.Major)).
				WithSuggestion(&output.Suggestion{
					Human: "upgrade the source server to PostgreSQL 17+ (and enable summarize_wal), or take a full backup by omitting --incremental-from",
				}).Wrap(output.ErrUsage)
		}
		backupType = backup.BackupTypeIncremental
	}
	backupID, err := generateBackupID(opts.Deployment, backupType)
	if err != nil {
		return nil, err
	}
	label := opts.Label
	if label == "" {
		label = backupID
	}

	// Stamp the backup ID on the top-level span so a tracing UI can
	// pivot on it to find this specific run's children.
	span.SetAttributes(attribute.String("backup_id", backupID))
	if opts.Incremental != nil {
		span.SetAttributes(
			attribute.Bool("incremental", true),
			attribute.String("parent_backup_id", opts.Incremental.ParentBackupID),
		)
	}

	// 5. Set up tarsink and run BASE_BACKUP. The basebackup.Run call
	// covers pg_backup_start → tar stream → pg_backup_stop in one
	// shot; that's the natural single-span boundary. INCREMENTAL is
	// just another option: PG sends INCREMENTAL.<filename> tar
	// entries for changed-only files; the chunker's CAS dedup means
	// we don't pay double for unchanged blocks even when full
	// backups go through the same path.
	var sinkOpts []tarsink.Option
	if opts.OnFile != nil {
		sinkOpts = append(sinkOpts, tarsink.WithFileObserver(opts.OnFile))
	}
	sink := tarsink.New(ctx, cas, sinkOpts...)
	backupTypeLabel = string(backupType)
	started = true
	metrics.BackupStarted(opts.Deployment, backupTypeLabel)
	emit(output.NewEvent(output.SeverityInfo, "backup", "started").
		WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: backupID, Tenant: opts.Tenant}))

	bbOpts := basebackup.Options{
		Label:             label,
		Fast:              opts.Fast,
		Manifest:          opts.IncludeManifest,
		IncludeWAL:        opts.IncludeWAL,
		InactivityTimeout: opts.InactivityTimeout,
	}
	if opts.Incremental != nil {
		bbOpts.IncrementalManifest = opts.Incremental.ParentPGManifest
	}
	bbCtx, bbSpan := tracing.Tracer().Start(ctx, "pg.basebackup.stream",
		trace.WithAttributes(
			attribute.String("label", label),
			attribute.Bool("fast", opts.Fast),
			attribute.Bool("incremental", opts.Incremental != nil),
		))
	bbRes, err := basebackup.Run(bbCtx, replConn, bbOpts, sink)
	if err != nil {
		bbSpan.SetStatus(codes.Error, err.Error())
		bbSpan.End()
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("backup: BASE_BACKUP: %w", err)
	}
	bbSpan.SetAttributes(
		attribute.Int("tablespaces", len(bbRes.Tablespaces)),
		attribute.Int64("bytes_received", int64(bbRes.Stats.BytesReceived)),
	)
	bbSpan.End()
	emit(output.NewEvent(output.SeverityInfo, "backup", "stream_complete").
		WithBody(map[string]any{
			"tablespaces":    len(bbRes.Tablespaces),
			"bytes_received": bbRes.Stats.BytesReceived,
			"messages":       bbRes.Stats.MsgsReceived,
		}))

	// Durability barrier: every chunk so far was written
	// DurabilityDeferred (no per-chunk fsync). Barrier fsyncs them
	// all as one batch — the chunks MUST be crash-durable before
	// the manifest that references them is committed, or a crash
	// could leave a committed manifest pointing at lost chunks.
	if err := cas.Barrier(ctx); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("backup: durability barrier: %w", err)
	}

	// 6. Assemble + sign + commit the manifest.
	m := buildManifest(opts, bbRes, sink, backupID, identity, pgVersion.Major)
	m.Encryption = encryptionInfo

	// Manifest-invariants gate (issue #91).  ManifestStore.Commit
	// does NOT run Manifest.Validate; until this gate landed, a
	// manifest that failed an invariant (e.g. empty BackupLabel
	// because the tarsink never saw PG's backup_label entry,
	// invariant violations from a future schema regression, …)
	// would commit cleanly, pass basic `verify`, and explode only
	// at restore / `verify --full` time with a structured but
	// late `manifest.invalid` error.  Running Validate before
	// commit moves that failure to backup time where the operator
	// has full context — and ensures every committed manifest in
	// the repo IS restorable.
	if err := m.Validate(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, output.NewError("backup.manifest_invalid",
			fmt.Sprintf("backup: refusing to commit %s: manifest fails its own invariant check: %v",
				backupID, err)).
			WithSuggestion(&output.Suggestion{
				Human: "the backup completed but the manifest is malformed and would not restore. " +
					"Common cause: PG's BASE_BACKUP didn't emit backup_label in the streamed tarball — " +
					"check the source PG version (PG 15+ required), confirm pg_backup_start/stop are " +
					"the non-exclusive variants, and re-run the backup.",
			}).Wrap(err)
	}

	// WAL-gap embedding. If the deployment has any persisted
	// gap records, snapshot them onto the manifest so restore
	// can refuse PITR within the gap range without having to
	// consult live gapstate (which may be unavailable / GC'd /
	// wiped). The manifest is signed; the embedded gaps cannot
	// be tampered with after commit.
	//
	// We embed all gaps for the deployment regardless of TLI —
	// a gap on TLI 5 is still relevant if the operator restores
	// to an LSN in that range, and the LSN ranges across TLIs
	// don't overlap (PG's WAL stream is monotonic). Best-effort:
	// failure to read gapstate downgrades to a structured event;
	// the backup commits without embedded gaps and restore falls
	// back to live gapstate consultation.
	if gaps, gerr := readGapsForManifest(ctx, sp, opts.Deployment); gerr != nil {
		emit(output.NewEvent(output.SeverityWarning, "backup", "wal_gaps_unreadable").
			WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: backupID}).
			WithBody(map[string]any{"error": gerr.Error()}))
	} else if len(gaps) > 0 {
		m.WALGaps = gaps
		emit(output.NewEvent(output.SeverityNotice, "backup", "wal_gaps_embedded").
			WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: backupID}).
			WithBody(map[string]any{
				"gap_count": len(gaps),
				"hint":      "this backup carries the+ WAL-gap metadata; restore will refuse PITR within these ranges",
			}))
	}

	// Essential-files gate (issue #84).  PG's BASE_BACKUP silently
	// skips files that have disappeared from PGDATA mid-stream — an
	// operator who removes postgresql.conf from a running server's
	// data directory gets a backup that pg_verifybackup accepts but
	// pg_ctl start cannot.  Catch that here, before commit, by
	// asking the source PG where its config files live and
	// asserting the in-PGDATA ones are present in the manifest.
	//
	// Best-effort: if probeConfigLocations fails (older PG, permission
	// issue), we emit a warning and let the commit proceed — the
	// downstream restore-time pg_verifybackup still runs.
	if locs, lerr := probeConfigLocations(ctx, opts.PGConnString); lerr != nil {
		emit(output.NewEvent(output.SeverityWarning, "backup", "essential_files_unchecked").
			WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: backupID}).
			WithBody(map[string]any{
				"error": lerr.Error(),
				"hint":  "could not query data_directory / config_file from source PG; skipping the pre-commit essential-files check",
			}))
	} else if eerr := backup.CheckEssentialFiles(m, locs.DataDirectory, locs.ConfigFile, locs.HbaFile, locs.IdentFile); eerr != nil {
		span.SetStatus(codes.Error, eerr.Error())
		return nil, output.NewError("backup.missing_essential_files",
			fmt.Sprintf("backup: refusing to commit %s: %v", backupID, eerr)).
			WithSuggestion(&output.Suggestion{
				Human: "the source data directory is missing files PG requires to start. " +
					"Inspect the source server: were the listed files removed while PG was running? " +
					"Restore them on the source, then re-run the backup.",
			}).
			Wrap(eerr)
	}

	commitOpts := backup.CommitOptions{
		OnReplicaError: func(rerr error) {
			// Primary is committed; redundancy copy failed. Surface as
			// a warning so the operator knows the cross-prefix-survival
			// guarantee is weaker for this backup until the next commit
			// of the same body lands a replica successfully.
			emit(output.NewEvent(output.SeverityWarning, "backup", "manifest.replica_failed").
				WithSubject(output.Subject{
					Deployment: opts.Deployment,
					BackupID:   backupID,
					Tenant:     opts.Tenant,
				}).
				WithBody(map[string]any{
					"error": rerr.Error(),
				}).
				WithSuggestion(&output.Suggestion{
					Human:   "the primary manifest is committed; the replica copy will be retried on the next successful commit. If the failure persists, run `pg_hardstorage doctor` to inspect the repository.",
					Command: "pg_hardstorage doctor " + opts.Deployment,
				}))
		},
	}
	// WORM propagation to manifest commits. Same `wormNow` captured
	// at CAS construction is used here so chunks + manifest share
	// the retention deadline.
	if !repoMeta.WORM.IsZero() {
		commitOpts.RetainUntil = repoMeta.WORM.RetainUntil(wormNow)
		commitOpts.RetentionMode = storage.WORMMode(repoMeta.WORM.Mode)
	}
	commitCtx, commitSpan := tracing.Tracer().Start(ctx, "manifest.commit",
		trace.WithAttributes(
			attribute.String("backup_id", backupID),
			attribute.Int("file_count", len(m.Files)),
		))
	if err := manifestStore.Commit(commitCtx, m, opts.Signer, commitOpts); err != nil {
		commitSpan.SetStatus(codes.Error, err.Error())
		commitSpan.End()
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("backup: commit manifest %s: %w", backupID, err)
	}
	commitSpan.End()

	// 7. Re-read + verify with the user-supplied Verifier — proves the
	//    just-written manifest is consumable by the rest of the system.
	if _, err := manifestStore.Read(ctx, opts.Deployment, backupID, opts.Verifier); err != nil {
		return nil, fmt.Errorf("backup: post-commit verify: %w", err)
	}

	stoppedAt := time.Now().UTC()
	res := summarize(opts, m, sink, identity, pgVersion.Major, startedAt, stoppedAt)
	res.PrimaryKey = backup.PrimaryPath(opts.Deployment, backupID)
	// Dedup outcomes measured at PutChunk time (the CAS counts them as
	// it runs); safe to snapshot now that BASE_BACKUP + Barrier are
	// done and no more chunks will be written.
	res.Dedup = cas.DedupStats()

	// Metrics: the backup is durably committed and verified.  Record the
	// terminal success plus the size/dedup/duration signals dashboards
	// chart.  completed=true disarms the deferred failure recorder.
	completed = true
	metrics.BackupCompleted(opts.Deployment, backupTypeLabel, "success")
	metrics.ObserveBackupDuration(opts.Deployment, backupTypeLabel, res.Duration.Seconds())
	metrics.SetBackupBytes(opts.Deployment, res.LogicalBytes, res.UniqueChunkBytes)
	metrics.AddChunkUploads(opts.Deployment, res.Dedup.Misses, res.Dedup.HitsInMemory+res.Dedup.HitsStorage)

	emit(output.NewEvent(output.SeverityInfo, "backup", "committed").
		WithSubject(output.Subject{
			Deployment: opts.Deployment,
			BackupID:   backupID,
			Tenant:     opts.Tenant,
			LSN:        bbRes.StopLSN,
			Timeline:   bbRes.StopTimeline,
		}).
		WithBody(map[string]any{
			"duration_ms":        res.Duration.Milliseconds(),
			"file_count":         res.FileCount,
			"unique_chunk_count": res.UniqueChunkCount,
			"unique_chunk_bytes": res.UniqueChunkBytes,
			"total_chunk_refs":   res.TotalChunkRefs,
			"logical_bytes":      res.LogicalBytes,
			"primary_key":        res.PrimaryKey,
			"chunks_written":     res.Dedup.Misses,
			"chunks_deduped":     res.Dedup.HitsInMemory + res.Dedup.HitsStorage,
			"dedup_hit_rate":     res.Dedup.HitRate(),
		}))

	// 8. Append a backup.create record to the hash-chained audit
	//    log. Best-effort: an audit-write failure must NOT fail the
	//    backup (the manifest is already committed and visible). We
	//    emit a structured warning so monitoring sees the gap, and
	//    a post-hoc `audit verify-chain` will surface a missing event
	//    if the head pointer's sequence skips. Same pattern as the
	//    Sink fan-out — the audit log is observability, not critical
	//    path.
	// WORM-aware audit store: when the repo carries a retention
	// policy, the backup.create event picks it up just like the
	// chunks + manifest did at the top of this run.
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	auditEv := &audit.Event{
		Action: "backup.create",
		Actor:  opts.Actor, // "" when CLI didn't pass it
		Tenant: opts.Tenant,
		Subject: audit.Subject{
			Deployment: opts.Deployment,
			BackupID:   backupID,
			Tenant:     opts.Tenant,
			Repo:       opts.RepoURL,
		},
		Body: map[string]any{
			"start_lsn":      bbRes.StartLSN,
			"stop_lsn":       bbRes.StopLSN,
			"timeline":       bbRes.StopTimeline,
			"duration_ms":    res.Duration.Milliseconds(),
			"file_count":     res.FileCount,
			"logical_bytes":  res.LogicalBytes,
			"unique_chunks":  res.UniqueChunkCount,
			"chunks_written": res.Dedup.Misses,
			"chunks_deduped": res.Dedup.HitsInMemory + res.Dedup.HitsStorage,
		},
	}
	if err := auditStore.Append(ctx, auditEv); err != nil {
		emit(output.NewEvent(output.SeverityWarning, "backup", "audit_append_failed").
			WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: backupID}).
			WithBody(map[string]any{"error": err.Error()}).
			WithSuggestion(&output.Suggestion{
				Human: "the backup committed cleanly but the audit-chain append failed; running `pg_hardstorage audit verify-chain --repo <url>` will show whether the chain is otherwise intact",
			}))
	}

	_ = repoMeta // captured for potential future use (ID logging, attestation chain)
	return res, nil
}

// validateOptions checks required fields and fills in defaults. Errors
// are returned as structured *output.Error so the CLI can map exit codes.
func validateOptions(o *TakeOptions) error {
	if o.PGConnString == "" {
		return output.NewError("usage.missing_pg_conn_string",
			"backup: PGConnString is required").Wrap(output.ErrUsage)
	}
	if o.RepoURL == "" {
		return output.NewError("usage.missing_repo_url",
			"backup: RepoURL is required").Wrap(output.ErrUsage)
	}
	if o.Deployment == "" {
		return output.NewError("usage.missing_deployment",
			"backup: Deployment is required").Wrap(output.ErrUsage)
	}
	// The deployment is interpolated into storage keys
	// (manifests/<dep>/backups/..., wal/<dep>/...) and the backup ID.
	// Reject path separators and control characters here — the single
	// chokepoint every backup path (CLI, agent, programmatic Take)
	// flows through. A '/' or '\' would splinter the deployment across
	// key levels so deployment-enumeration / GC / retention parse it
	// under the wrong name; a NUL/newline/CR corrupts keys and log
	// lines. This is the storage-safety subset, deliberately narrower
	// than config.ValidDeploymentName's [a-zA-Z][a-zA-Z0-9_-] rule so
	// we don't newly reject digit-start or short names a programmatic
	// caller may already rely on; we only block the injection surface.
	if o.Deployment == "." || o.Deployment == ".." {
		// "." / ".." are path components: a deployment of ".." would
		// resolve manifests/../backups/... back out of the manifests/
		// tree.
		return output.NewError("usage.bad_deployment",
			fmt.Sprintf("backup: deployment %q is not a valid name (reserved path component)", o.Deployment)).
			Wrap(output.ErrUsage)
	}
	if i := strings.IndexFunc(o.Deployment, func(r rune) bool {
		return r == '/' || r == '\\' || r < 0x20 || r == 0x7f
	}); i >= 0 {
		return output.NewError("usage.bad_deployment",
			fmt.Sprintf("backup: deployment %q contains an illegal character (path separators and control characters are not allowed)", o.Deployment)).
			Wrap(output.ErrUsage)
	}
	if o.Signer == nil {
		return output.NewError("usage.missing_signer",
			"backup: Signer is required (we don't write unsigned manifests)").Wrap(output.ErrUsage)
	}
	if o.Tenant == "" {
		o.Tenant = "default"
	}
	if !o.IncludeManifest {
		// Default true. Users who really don't want it set it to false explicitly;
		// but the zero-value default for bool can't distinguish "unset" from
		// "false explicitly", so we always include it unless the caller is in
		// a future advanced API. For now: simplify and force on.
		o.IncludeManifest = true
	}
	if o.Verifier == nil {
		// Derive a verifier from the signer's public key so callers
		// don't need to pass both for the common case.
		pubPEM, err := o.Signer.PublicKeyPEM()
		if err != nil {
			return fmt.Errorf("backup: derive verifier: %w", err)
		}
		v, err := backup.LoadVerifier(pubPEM)
		if err != nil {
			return fmt.Errorf("backup: parse derived verifier: %w", err)
		}
		o.Verifier = v
	}
	return nil
}

// probeVersion opens a short-lived regular-mode connection just to read
// server_version. We close it before returning so the rest of the
// backup runs against a single replication-mode connection.
func probeVersion(ctx context.Context, dsn string) (pg.Version, error) {
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return pg.Version{}, err
	}
	defer c.Close(ctx)
	v, err := pg.QueryVersion(ctx, c)
	if err != nil {
		return pg.Version{}, fmt.Errorf("backup: probe version: %w", err)
	}
	return v, nil
}

// probeConfigLocations opens a short-lived regular-mode connection
// to read data_directory + config_file + hba_file + ident_file.
// Used by the essential-files gate to decide whether each config
// file lives inside PGDATA (and must therefore be present in the
// backup manifest) — see issue #84.
func probeConfigLocations(ctx context.Context, dsn string) (pg.ConfigFileLocations, error) {
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		return pg.ConfigFileLocations{}, err
	}
	defer c.Close(ctx)
	locs, err := pg.QueryConfigFileLocations(ctx, c)
	if err != nil {
		return pg.ConfigFileLocations{}, fmt.Errorf("backup: probe config locations: %w", err)
	}
	return locs, nil
}

// mapRepoErr classifies repo.Open errors for the CLI's exit-code contract.
func mapRepoErr(url string, err error) error {
	if errors.Is(err, repo.ErrNotARepo) {
		return output.NewError("notfound.repo",
			fmt.Sprintf("backup: no pg_hardstorage repository at %s (run `pg_hardstorage repo init`)", url)).
			Wrap(err)
	}
	if errors.Is(err, storage.ErrUnknownScheme) {
		return output.NewError("usage.unknown_scheme",
			fmt.Sprintf("backup: %v", err)).Wrap(output.ErrUsage)
	}
	return fmt.Errorf("backup: open repo: %w", err)
}

// generateBackupID synthesises an identifier of the form
//
//	<deployment>.<type>.<timestamp><random4>
//
// where timestamp is RFC 3339 compact UTC ("20060102T150405Z") and
// random4 is 4 random hex chars (16 bits) for collision resistance
// when two backups land in the same second.
func generateBackupID(deployment string, t backup.BackupType) (string, error) {
	now := time.Now().UTC().Format("20060102T150405Z")
	var rnd [2]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("backup: generate id: %w", err)
	}
	suffix := hex.EncodeToString(rnd[:])
	return fmt.Sprintf("%s.%s.%s.%s", deployment, t, now, suffix), nil
}

// base64Encode returns the standard-padding base64 of b. Used for
// the wrapped-DEK manifest field; standard padding (vs URL-safe)
// keeps the on-disk JSON readable when an operator opens a manifest
// in less.
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// base64Decode is the symmetric reader (used by restore).
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// buildManifest assembles a Manifest from the sink output and protocol
// metadata. It does NOT compute Attestation here; ManifestStore.Commit
// signs as part of the commit flow.
func buildManifest(opts TakeOptions, bb *basebackup.Result, sink *tarsink.Sink, backupID string, identity pg.SystemIdentity, pgVersion int) *backup.Manifest {
	files := sink.AllFiles()
	tablespaces := make([]backup.Tablespace, 0, len(bb.Tablespaces))
	for _, ts := range bb.Tablespaces {
		tablespaces = append(tablespaces, backup.Tablespace{
			OID:      ts.OID,
			Location: ts.Location,
		})
	}
	backupType := backup.BackupTypeFull
	parentID := ""
	if opts.Incremental != nil && len(opts.Incremental.ParentPGManifest) > 0 {
		backupType = backup.BackupTypeIncremental
		parentID = opts.Incremental.ParentBackupID
	}
	// PG's BASE_BACKUP surfaces the real start LSN — the checkpoint REDO
	// point, i.e. the backup_label "START WAL LOCATION", where WAL replay
	// must begin — as its first result set (bb.StartLSN). Record THAT:
	// it is what the WAL-retention frontier must protect and what restore
	// reports. Earlier code used identity.XLogPos (the IDENTIFY_SYSTEM
	// position taken BEFORE the backup), which is only a lower bound, so
	// it over-retained WAL and mis-reported the start. Fall back to it
	// only if BASE_BACKUP somehow didn't surface a start LSN.
	startLSN := bb.StartLSN
	if startLSN == "" {
		startLSN = identity.XLogPos
	}
	return &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       opts.Deployment,
		Tenant:           opts.Tenant,
		Type:             backupType,
		ParentBackupID:   parentID,
		PGVersion:        pgVersion,
		SystemIdentifier: identity.SystemID,
		// The checkpoint redo point (see startLSN above) — the LSN the
		// WAL-retention frontier protects and restore replays from.
		StartLSN:  startLSN,
		StopLSN:   bb.StopLSN,
		Timeline:  bb.StopTimeline,
		StartedAt: bb.StartedAt,
		StoppedAt: bb.StoppedAt,
		// Compression is what the runner ASKED the CAS to write with.
		// Per-chunk envelopes record the actual codec used (a chunk
		// shorter than the codec's break-even threshold may be
		// stored uncompressed even when this manifest says zstd).
		Compression: "zstd",
		Tablespaces: tablespaces,
		Files:       files,
		// Dirs records every TypeDir entry PG sent in the
		// BASE_BACKUP tar — needed so the restore step can
		// re-create empty directories like pg_wal/ that
		// would otherwise be missing.
		Dirs:          sink.AllDirs(),
		BackupLabel:   string(sink.BackupLabel()),
		TablespaceMap: string(sink.TablespaceMap()),
		// PGBackupManifest carries PG's own backup_manifest JSON
		// (verbatim from the BASE_BACKUP MANIFEST 'yes' CopyOut).
		// Required for PG 17+ incremental child backups; harmless
		// when the chain stays full-only.
		PGBackupManifest: bb.ManifestBytes,
		// WALRequired is empty in v0.1: WAL streaming lands in Slice 8.
		// Restore-time PITR will use this once the WAL service is in.
		WALRequired: nil,
		// SourceTDE propagates the operator's TDE declaration onto
		// the manifest so restore-time tooling (refusal of vanilla-PG
		// target, relaxed pg_verifybackup expectations) can read
		// the posture without re-consulting deployment config.
		// See docs/explanation/tde-awareness.md.
		SourceTDE: opts.SourceTDE,
	}
}

// summarize derives a Result from the assembled manifest. Counters are
// computed by walking the FileEntries; chunk uniqueness is a hash-map
// dedupe so two refs to the same chunk count as one unique chunk.
func summarize(opts TakeOptions, m *backup.Manifest, _ *tarsink.Sink, identity pg.SystemIdentity, pgVersion int, startedAt, stoppedAt time.Time) *Result {
	res := &Result{
		BackupID:         m.BackupID,
		Deployment:       opts.Deployment,
		Tenant:           opts.Tenant,
		PGVersion:        pgVersion,
		SystemIdentifier: identity.SystemID,
		StartLSN:         m.StartLSN,
		StopLSN:          m.StopLSN,
		Timeline:         m.Timeline,
		StartedAt:        startedAt,
		StoppedAt:        stoppedAt,
		Duration:         stoppedAt.Sub(startedAt),
		TablespaceCount:  len(m.Tablespaces),
		FileCount:        len(m.Files),
	}

	uniqueBytes := map[repo.Hash]int64{}
	for _, f := range m.Files {
		res.LogicalBytes += f.Size
		res.TotalChunkRefs += len(f.Chunks)
		for _, c := range f.Chunks {
			uniqueBytes[c.Hash] = c.Len
		}
	}
	res.UniqueChunkCount = len(uniqueBytes)
	for _, sz := range uniqueBytes {
		res.UniqueChunkBytes += sz
	}
	return res
}

// readGapsForManifest snapshots the deployment's persisted
// gap records into the manifest's []backup.WALGap form. Used
// at backup commit time so the manifest itself carries the
// gap metadata regardless of whether live gapstate is
// available at restore time.
//
// All gap records for the deployment are included (no TLI
// filter): a gap on any TLI matters for any restore that
// targets an LSN in that range, and PG's monotonic LSN means
// ranges don't overlap across TLIs.
//
// Empty result + nil error means "no gaps" — return nil so
// the manifest's omitempty keeps the JSON shape compact.
func readGapsForManifest(ctx context.Context, sp storage.StoragePlugin, deployment string) ([]backup.WALGap, error) {
	records, err := gapstate.New(sp).List(ctx, deployment)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	out := make([]backup.WALGap, 0, len(records))
	for _, r := range records {
		out = append(out, backup.WALGap{
			SlotName:    r.SlotName,
			SlotRole:    r.SlotRole,
			Timeline:    r.Timeline,
			GapStartLSN: r.GapStartLSN,
			GapEndLSN:   r.GapEndLSN,
			GapBytes:    r.GapBytes,
			DetectedAt:  r.DetectedAt,
		})
	}
	return out, nil
}

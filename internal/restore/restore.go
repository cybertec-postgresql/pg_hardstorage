// Package restore materialises a committed backup back onto a
// filesystem target. It is the mirror image of internal/backup/runner:
// where the runner streams PGDATA bytes through the chunker into the
// CAS, restore walks the manifest's FileEntries and reconstitutes
// each file from chunk references.
//
// Slice 7a scope: full-restore-only — given a backup ID, deployment,
// and target directory, recreate every file. PITR (replaying WAL up
// to a target) and "preview without mutation" land in 7c, after the
// CLI shim in 7b.
//
// Failure model:
//
//   - Pre-flight refuses non-empty target dirs unless AllowOverwrite.
//   - Manifest is signature-verified before any file write.
//   - Each chunk's bytes are SHA-256-verified at fetch time (the CAS
//     contract via GetChunkBytes).
//   - On error, partial files on disk are NOT cleaned up — the user
//     can inspect, fix, and retry. We don't pretend a half-finished
//     restore looks clean.
package restore

import (
	"bytes"
	"context"
	"encoding/binary"
	stdjson "encoding/json"
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/verifybackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/tracing"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/combine"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

// Options configures one restore run.
type Options struct {
	// RepoURL is the source repository (file://, s3://, ...).
	// Must already exist (validated by repo.Open).
	RepoURL string

	// Deployment + BackupID identify the manifest to restore.
	// "latest" semantics live in the CLI; this function takes an
	// explicit ID.
	Deployment string
	BackupID   string

	// TargetDir is where the data directory will be materialised.
	// We refuse to write into a non-empty directory unless
	// AllowOverwrite is true.
	TargetDir string

	// Verifier verifies the manifest signature before any file is
	// written. Required: we won't restore from an unverified manifest.
	Verifier *backup.Verifier

	// AllowOverwrite permits writing into a non-empty TargetDir. The
	// CLI gates this behind --force; programmatic callers set it
	// explicitly.
	AllowOverwrite bool

	// AllowForeignCluster permits an AllowOverwrite restore to proceed
	// even when the target is a valid PGDATA whose pg_control system
	// identifier differs from the backup's — i.e. a DIFFERENT cluster.
	// Without it (the default), such a target is refused even under
	// --force, on the assumption the operator pointed --target at the
	// wrong, still-wanted cluster. The CLI gates this behind
	// --force-foreign. See issue #100.
	AllowForeignCluster bool

	// Recovery configures PITR. When non-nil and Recovery.Enable is
	// true, after file materialization we drop recovery.signal and
	// append the recovery_* GUCs to postgresql.auto.conf in
	// TargetDir. The cluster will then enter recovery on first start
	// and replay WAL via the configured restore_command.
	Recovery *Recovery

	// VerifyMode controls the post-restore "does the cluster
	// actually start?" smoke test.  Defence layer L3.
	//
	//   off       — skip the smoke test entirely
	//   auto      — try; soft-skip with WARN if no host PG
	//   required  — hard-fail if no host PG
	//   dump      — auto + run pg_dumpall (L4)
	//
	// Empty defaults to "auto".  Operators that want zero
	// silent data-loss risk flip to "required" in CI.
	VerifyMode string

	// KEKForRef resolves a manifest's EncryptionInfo.KEKRef to the
	// matching 32-byte KEK. Required when restoring an encrypted
	// backup; ignored for unencrypted backups.
	//
	// Why a callback rather than a single KEK? Operators may have
	// multiple KEKs (per-tenant, rotated, KMS-backed); the manifest
	// records WHICH one wrapped the DEK and the resolver lets the
	// caller plug in their key-management strategy. v0.1's CLI
	// supplies a simple file-system-based resolver; KMS resolvers
	// drop in via the same callback shape.
	KEKForRef func(ref string) ([encryption.KeyLen]byte, error)

	// UnwrapDEK resolves the DEK for a cloud-KMS-encrypted backup by
	// unwrapping manifest.Encryption.WrappedDEK server-side (the cloud KEK
	// never leaves the HSM, so KEKForRef — which returns raw KEK bytes —
	// cannot resolve it). Required to restore a backup whose KEKRef is a
	// cloud scheme (aws-kms / gcp-kms / azure-kv / vault-transit / pkcs11);
	// ignored for local-custody and unencrypted backups. The CLI wires this
	// to keystore.UnwrapDEK.
	UnwrapDEK func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error)

	// OnEvent receives progress events. Optional. Events fire on
	// Restore's goroutine and must return promptly.
	OnEvent func(*output.Event)

	// Actor identifies who initiated the restore, for the hash-
	// chained audit log. Free-form (operator email, agent ID, etc.).
	// Empty when the caller doesn't track an explicit principal.
	Actor string

	// TablespaceRemap, when non-nil, redirects tablespace paths
	// recorded in the manifest (or the chain's leaf manifest)
	// to operator-supplied target paths. Plain restores rewrite
	// the manifest's `tablespace_map` body before writing it
	// into the target dir. Chain restores pass the mapping
	// through to pg_combinebackup as `--tablespace-mapping=OLD=NEW`
	// flags.
	//
	// Empty / nil = no remap; the manifest's recorded paths are
	// used verbatim (the previous+ behaviour).
	TablespaceRemap TablespaceRemap

	// ChainStagingRoot lets the operator pin the directory chain
	// restores use to materialise each link before merging via
	// pg_combinebackup.  Default: a path derived from os.TempDir
	// keyed on (deployment, leaf_backup_id) — stable across
	// retries so a re-run after a pg_combinebackup failure or a
	// mid-link crash skips already-materialised links via per-
	// link completion markers. .
	//
	// Empty = use the default derived path.  Set to a custom
	// value when staging needs to live on a specific volume
	// (e.g. an SSD with capacity for a 100 TB chain).  Cleaned
	// up only on successful completion of pg_combinebackup;
	// failures preserve staging so the next attempt resumes.
	ChainStagingRoot string

	// ResetChainStaging forces a fresh chain-restore staging
	// directory, removing any persisted staging from a previous
	// attempt.  Use when the prior attempt crashed in a way that
	// left staging in an unknown state, or when the operator
	// wants to start over from scratch. .
	ResetChainStaging bool
}

// chainLinkCompleteFilename is the marker file written into a
// chain-link staging directory once materialisation has finished
// successfully.  Its presence (plus a matching backup_id +
// chunk_count) lets a subsequent re-run skip the link.  Audit
// v23 #8.
const chainLinkCompleteFilename = ".pg_hardstorage_link_complete.json"

// chainLinkMarker is the JSON body of chainLinkCompleteFilename.
// Schema-versioned so a future format change can be rejected
// gracefully (force a re-materialise).
type chainLinkMarker struct {
	Schema       string `json:"schema"`
	BackupID     string `json:"backup_id"`
	ChunkCount   int    `json:"chunk_count"`
	BytesWritten int64  `json:"bytes_written"`
}

const chainLinkMarkerSchema = "pg_hardstorage.restore.chain_link_marker.v1"

// Result is the structured outcome of a successful restore.
//
// Duration serializes as WHOLE MILLISECONDS under the frozen key
// duration_ms (MarshalJSON below): a raw time.Duration under a _ms key
// would emit nanoseconds, inflating every consumer's reading 1e6x.
type Result struct {
	BackupID          string        `json:"backup_id"`
	Deployment        string        `json:"deployment"`
	TargetDir         string        `json:"target_dir"`
	FileCount         int           `json:"file_count"`
	BytesWritten      int64         `json:"bytes_written"`
	ChunksFetched     int           `json:"chunks_fetched"`
	BackupLabelSize   int           `json:"backup_label_size"`
	TablespaceMapSize int           `json:"tablespace_map_size"`
	StartedAt         time.Time     `json:"started_at"`
	StoppedAt         time.Time     `json:"stopped_at"`
	Duration          time.Duration `json:"-"`
}

// MarshalJSON emits duration_ms as whole milliseconds (see Result doc).
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result // no methods: avoids recursing into MarshalJSON
	return stdjson.Marshal(struct {
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
	if err := stdjson.Unmarshal(b, &aux); err != nil {
		return err
	}
	r.Duration = time.Duration(aux.DurationMS) * time.Millisecond
	return nil
}

// Restore materialises the named backup at TargetDir. See package
// docs for the failure model and pre-flight semantics.
func Restore(ctx context.Context, opts Options) (res *Result, err error) {
	if verr := validateOptions(&opts); verr != nil {
		return nil, verr
	}
	emit := opts.OnEvent
	if emit == nil {
		emit = func(*output.Event) {}
	}

	// Restore metrics (observability audit #2): emit started + a deferred
	// completed{result}/duration so the /metrics endpoint tracks restore
	// success-rate and latency, the way backup already does. Registered
	// after validation so a usage error isn't counted as a restore
	// attempt; the defer wraps both the plain and chain (delegated) paths.
	metrics.RestoreStarted(opts.Deployment)
	metricStart := time.Now()
	defer func() {
		result := "success"
		if err != nil {
			result = "failure"
		}
		metrics.RestoreCompleted(opts.Deployment, result)
		metrics.ObserveRestoreDuration(opts.Deployment, time.Since(metricStart).Seconds())
	}()

	// Top-level span. Child spans for materialise + recovery files
	// attach to it. The span's status flips to Error on any failed
	// return path so a tracing UI shows the run as red without the
	// caller having to recover the error.
	ctx, span := tracing.Tracer().Start(ctx, "pg_hardstorage.restore",
		trace.WithAttributes(
			attribute.String("deployment", opts.Deployment),
			attribute.String("backup_id", opts.BackupID),
			attribute.String("target_dir", opts.TargetDir),
		))
	defer span.End()

	startedAt := time.Now().UTC()

	// 1. Open the repo.
	repoMeta, sp, err := repo.Open(ctx, opts.RepoURL)
	if err != nil {
		return nil, mapRepoErr(opts.RepoURL, err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)

	// 2. Read + verify the manifest.
	m, err := store.Read(ctx, opts.Deployment, opts.BackupID, opts.Verifier)
	if err != nil {
		return nil, fmt.Errorf("restore: read manifest %s/%s: %w",
			opts.Deployment, opts.BackupID, err)
	}

	// 2pre. WAL-gap pre-flight. Refuse if the operator's PITR
	// target lands inside a known gap. Runs BEFORE chain
	// dispatch so chain restores get the same protection.
	// Storage-layer warnings degrade rather than refuse (a
	// transient List error shouldn't tank a legitimate
	// restore); a target_lsn that's actually in a gap range
	// surfaces a structured restore.target_in_wal_gap error.
	if err := preflightWALGap(ctx, sp, opts.Deployment, opts.Recovery, m.WALGaps, emit); err != nil {
		return nil, err
	}
	// Physical, warning-only backstop for an LSN target: surface a WAL
	// segment that is MISSING from the archive between the backup and the
	// target but that no gap record describes (pruning/corruption/manual
	// deletion). Never refuses — it only warns so a false positive can't
	// block a legitimate restore.
	preflightWALContiguity(ctx, sp, opts.Deployment, m, opts.Recovery, emit)

	// 2pre-b. Reachability gate (issue #99).  A --to-lsn target
	// BEFORE the backup's stop_lsn cannot be reached by forward
	// WAL replay; without this gate PG would silently recover
	// to end-of-WAL and the operator would get a database at the
	// wrong point in time with no error in sight.  The same
	// check runs in Preview so the operator catches it before
	// any disk write.
	if err := CheckTargetReachable(m.StopLSN, opts.Recovery); err != nil {
		return nil, err
	}

	// 2a. Incremental leaf? Divert to the chain-restore pipeline.
	// Chain restore materialises every link in the chain into its
	// own staging dir, runs pg_combinebackup, and finalises the
	// merged output at TargetDir. The full-restore code below is
	// untouched — chains are a layered upgrade, not a rewrite.
	if m.Type == backup.BackupTypeIncremental {
		return restoreIncrementalChain(ctx, opts, sp, repoMeta, m, emit, startedAt)
	}

	// 3. Build the CAS. If the manifest is encrypted, look up the
	//    KEK via the caller's resolver and unwrap the DEK before
	//    constructing an encryption-aware CAS.
	cas := casdefault.New(sp)
	if m.Encryption != nil {
		dec, err := buildEncryptedCAS(ctx, sp, m.Encryption, opts.KEKForRef, opts.UnwrapDEK)
		if err != nil {
			return nil, err
		}
		cas = dec
	}
	emit(output.NewEvent(output.SeverityInfo, "restore", "manifest_loaded").
		WithSubject(output.Subject{
			Deployment: opts.Deployment,
			BackupID:   opts.BackupID,
			Tenant:     m.Tenant,
			Timeline:   m.Timeline,
			LSN:        m.StopLSN,
		}).
		WithBody(map[string]any{
			"file_count":  len(m.Files),
			"tablespaces": len(m.Tablespaces),
			"pg_version":  m.PGVersion,
		}))

	// L1 — manifest self-consistency.  Hard-fail before
	// touching the target directory: a malformed manifest
	// cannot produce a correct restore, and we'd rather
	// surface that as "manifest invalid" than as a partial
	// datadir.  See manifest.Validate's docstring for the
	// invariant list.
	if err := m.Validate(); err != nil {
		return nil, output.NewError("manifest.invalid",
			"restore: "+err.Error()).Wrap(err)
	}

	// 3. Pre-flight target directory checks.
	if err := preflightTarget(opts.TargetDir, opts.AllowOverwrite, m.SystemIdentifier, opts.AllowForeignCluster); err != nil {
		return nil, err
	}
	if err := preflightTablespaceTargets(opts.TablespaceRemap, opts.AllowOverwrite); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.TargetDir, 0o700); err != nil {
		return nil, output.NewError("internal",
			fmt.Sprintf("restore: mkdir target: %v", err)).Wrap(err)
	}

	// 3a. Checkpoint resume — if a previous restore into this same
	// target dir crashed mid-way, a `.pg_hardstorage_restore_state.json`
	// file lists the files it had already fully materialised. Skip
	// those during this run; the chunks are already on disk.
	//
	// The checkpoint must match the current backup ID — resuming into
	// a target that holds bytes from a different backup would corrupt
	// the data dir. We refuse loudly when that mismatch shows up;
	// "wrong target dir" is what the operator wants to see, not a
	// silent half-from-X half-from-Y mix.
	cp, err := LoadCheckpoint(opts.TargetDir)
	if err != nil {
		return nil, output.NewError("restore.checkpoint_load_failed",
			fmt.Sprintf("restore: load checkpoint: %v", err)).Wrap(err)
	}
	resumed := cp != nil
	if resumed && cp.BackupID != "" && cp.BackupID != m.BackupID {
		return nil, output.NewError("conflict.checkpoint_mismatch",
			fmt.Sprintf("restore: checkpoint at %s belongs to backup %q, not %q",
				opts.TargetDir, cp.BackupID, m.BackupID)).
			WithSuggestion(&output.Suggestion{
				// The safe default is RESUMING the interrupted restore
				// (its backup ID is the positional argument) — never
				// steer an operator or automation toward `rm -rf` of a
				// directory that may hold their partially-restored data.
				Human: fmt.Sprintf("to resume the interrupted restore, re-run with its backup ID: `pg_hardstorage restore %s %s --target %s`; only if you deliberately want a FRESH restore of a different backup, remove the target directory first",
					m.Deployment, cp.BackupID, opts.TargetDir),
				Command: fmt.Sprintf("pg_hardstorage restore %s %s --target %s",
					m.Deployment, cp.BackupID, opts.TargetDir),
			})
	}
	completed := map[string]struct{}{}
	var resumedBytes int64
	var resumedChunks int
	if resumed {
		completed = cp.CompletedSet()
		resumedBytes = cp.BytesWritten
		resumedChunks = cp.ChunksFetched
		emit(output.NewEvent(output.SeverityNotice, "restore", "resumed").
			WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: opts.BackupID}).
			WithBody(map[string]any{
				"target":          opts.TargetDir,
				"completed_files": len(cp.CompletedFiles),
				"resumed_bytes":   resumedBytes,
				"resumed_chunks":  resumedChunks,
				"started_at":      cp.StartedAt.Format(time.RFC3339),
			}))
	}
	// --force into a non-empty target means "replace": the flag's help
	// promises "the existing contents will be deleted irrecoverably."
	// Honor that literally by clearing the target before materializing,
	// so a previous occupant's stale files can't be left mixed with the
	// restored backup (a datadir PG could start as silently corrupt).
	// Only when NOT resuming — a resume's partial files + checkpoint are
	// ours to keep, and the checkpoint-mismatch gate above already
	// refused a different backup's leftovers. preflightTarget's
	// running-PG / foreign-cluster gates already ran. See round-3 #1.
	if opts.AllowOverwrite && !resumed {
		if err := clearDirContents(opts.TargetDir); err != nil {
			return nil, output.NewError("restore.target_clear_failed",
				fmt.Sprintf("restore: clear --force target %q: %v", opts.TargetDir, err)).Wrap(err)
		}
	}

	cw := NewCheckpointWriter(opts.TargetDir, Checkpoint{
		Schema:         SchemaCheckpoint,
		BackupID:       m.BackupID,
		Deployment:     m.Deployment,
		TargetDir:      opts.TargetDir,
		StartedAt:      orResumeTime(resumed, cp, startedAt),
		CompletedFiles: completedSlice(cp),
		BytesWritten:   resumedBytes,
		ChunksFetched:  resumedChunks,
	}, 0)

	emit(output.NewEvent(output.SeverityInfo, "restore", "started").
		WithSubject(output.Subject{Deployment: opts.Deployment, BackupID: opts.BackupID}).
		WithBody(map[string]any{"target": opts.TargetDir, "resumed": resumed}))

	// 4. Materialise every file. Skip files already in the checkpoint.
	matCtx, matSpan := tracing.Tracer().Start(ctx, "restore.materialise",
		trace.WithAttributes(
			attribute.Int("file_count_total", len(m.Files)),
			attribute.Int("file_count_already_done", len(completed)),
		))
	var bytesWritten int64 = resumedBytes
	var chunksFetched int = resumedChunks
	// Resolve each non-default tablespace's on-disk destination once so
	// tablespace files land at their real location (consistent with the
	// tablespace_map we write below), not flattened under PGDATA root.
	tsDests := tablespaceDestRoots(m, opts.TablespaceRemap)
	for i := range m.Files {
		f := &m.Files[i]
		// Checkpoint identity is (tablespace, path): two tablespaces
		// can carry the same relative path, so keying resume on the
		// bare path would wrongly skip a not-yet-written file.
		fkey := checkpointKey(f)
		if _, alreadyDone := completed[fkey]; alreadyDone {
			continue
		}
		destRoot, err := fileDestRoot(opts.TargetDir, tsDests, f.TablespaceOID)
		if err != nil {
			matSpan.SetStatus(codes.Error, err.Error())
			matSpan.End()
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		n, k, err := materializeFile(matCtx, cas, destRoot, f)
		if err != nil {
			// Best-effort flush of what we did manage so the next
			// resume picks up here. A flush failure is non-fatal —
			// the original error is what the caller needs to see.
			_ = cw.Flush()
			matSpan.SetStatus(codes.Error, err.Error())
			matSpan.End()
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("restore: file %s: %w", f.Path, err)
		}
		bytesWritten += n
		chunksFetched += k
		if err := cw.MarkFileDone(fkey, n, k); err != nil {
			return nil, output.NewError("restore.checkpoint_write_failed",
				fmt.Sprintf("restore: write checkpoint: %v", err)).Wrap(err)
		}
		if err := ctx.Err(); err != nil {
			_ = cw.Flush()
			matSpan.End()
			return nil, err
		}
	}
	matSpan.SetAttributes(
		attribute.Int64("bytes_written", bytesWritten),
		attribute.Int("chunks_fetched", chunksFetched),
	)
	matSpan.End()
	// Final flush so any post-1GiB-but-pre-end progress lands.
	if err := cw.Flush(); err != nil {
		return nil, output.NewError("restore.checkpoint_write_failed",
			fmt.Sprintf("restore: final checkpoint write: %v", err)).Wrap(err)
	}

	// 4b. Re-create every directory PG emitted as a tar
	// TypeDir entry.  Required so EMPTY dirs (pg_wal/,
	// pg_dynshmem/, pg_notify/, pg_replslot/, pg_serial/,
	// pg_snapshots/, pg_stat/, pg_stat_tmp/, pg_subtrans/,
	// pg_tblspc/, pg_twophase/) come back from a backup;
	// MkdirAll-from-file-parent in step 4 only creates dirs
	// that contain at least one regular file, so without this
	// the restored datadir is missing pg_wal/ and PG refuses
	// to start.  Idempotent: MkdirAll is a no-op on existing
	// dirs.  Pre-fix manifests have m.Dirs == nil and the
	// loop is a no-op for them — newer agents can read older
	// manifests cleanly, just without the empty-dir fix's
	// benefit (matches the regression the user hit).
	for _, d := range m.Dirs {
		if d.Path == "" {
			continue
		}
		// Same tablespace-aware resolution as files: a non-default
		// tablespace's (possibly empty) dirs live under the
		// tablespace's real location, not under PGDATA root.
		dirRoot, err := fileDestRoot(opts.TargetDir, tsDests, d.TablespaceOID)
		if err != nil {
			return nil, fmt.Errorf("restore: dir %s: %w", d.Path, err)
		}
		full, err := safeJoinTarget(dirRoot, d.Path)
		if err != nil {
			return nil, fmt.Errorf("restore: dir %s: %w", d.Path, err)
		}
		mode := stdfs.FileMode(d.Mode)
		if mode == 0 {
			mode = 0o700
		}
		if err := os.MkdirAll(full, mode); err != nil {
			return nil, fmt.Errorf("restore: mkdir %s: %w", d.Path, err)
		}
		// MkdirAll honours umask + only creates with the
		// supplied mode for fresh dirs; an existing dir keeps
		// its current permissions.  Apply explicit chmod so a
		// resumed restore over a directory with the wrong
		// mode (e.g. group-readable PGDATA root) is corrected.
		if err := os.Chmod(full, mode); err != nil {
			// Non-fatal: filesystems that don't support modes
			// (rare in PG datadir hosting) shouldn't fail the
			// restore.  Log later; for now just continue.
			_ = err
		}
	}

	// 5. Materialise the special files (backup_label always; tablespace_map
	//    only when populated).
	if m.BackupLabel != "" {
		if err := writeSpecial(opts.TargetDir, "backup_label", []byte(m.BackupLabel)); err != nil {
			return nil, err
		}
	}
	if m.TablespaceMap != "" {
		// Apply operator-supplied tablespace remap to the
		// manifest's recorded paths, if any. Empty remap
		// returns the body unchanged.
		body := opts.TablespaceRemap.Apply(m.TablespaceMap)
		if err := writeSpecial(opts.TargetDir, "tablespace_map", []byte(body)); err != nil {
			return nil, err
		}
	}

	// Materialise PG's own backup_manifest so external tools
	// (operator-run pg_verifybackup, recovery drill's docker
	// sandbox, pg_combinebackup) can validate the restored data
	// dir without needing the pg_hardstorage manifest blob.
	// Surfaced by L8_recovery_drill_latest in the 2026-05-15
	// CLI scenario sweep — the drill restored cleanly but its
	// sandboxed pg_verifybackup then failed with
	//   pg_verifybackup: error: could not open file
	//   "/var/lib/postgresql/data/backup_manifest":
	//   No such file or directory.
	// We've been carrying the PGBackupManifest field in the
	// manifest since v0.5 specifically for incremental-chain
	// pg_combinebackup, but the standalone-restore path never
	// wrote it back to disk.  Backups taken pre-PGBackupManifest
	// have an empty field; skipping the write is safe — external
	// tools will then report the same not-found error the
	// operator would see in production, which is the honest
	// signal.
	if len(m.PGBackupManifest) > 0 {
		if err := writeSpecial(opts.TargetDir, "backup_manifest", m.PGBackupManifest); err != nil {
			return nil, err
		}
	}

	// L2 — in-process pg_verifybackup.  Hash every file PG
	// recorded in its backup_manifest and compare to the
	// recorded checksum.  Catches missing / truncated /
	// silently-corrupted files that L1's manifest invariants
	// don't (those are about OUR manifest's shape; this is
	// about source-vs-restored byte equality).  Soft-skips
	// only when the backup didn't carry PG's manifest at all
	// (pre-backups), in which case we record a notice
	// so operators see the gap in the audit log.
	//
	// TDE skip: PG's backup_manifest records SHA-256 checksums
	// over PLAINTEXT bytes (the in-buffer-cache view at
	// pg_basebackup time).  Under TDE the bytes ON DISK are
	// ciphertext until a TDE-capable target PG reads them.  An
	// in-process hash over the restored ciphertext bytes can
	// NEVER match the manifest's plaintext hash; running the
	// gate would only produce a misleading "verify failed"
	// against a perfectly-correct restore.  We skip and log a
	// notice that names the reason so the audit trail still
	// shows the operator made an informed choice.  The honest
	// validation under TDE happens later: boot the restored
	// data dir against a TDE-capable PG instance with key
	// access; that PG runs ITS pg_verifybackup over decrypted
	// pages.  See docs/explanation/tde-awareness.md.
	if m.SourceTDE != nil {
		emit(output.NewEvent(output.SeverityNotice, "restore", "verifybackup_skipped_tde").
			WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
			WithBody(map[string]any{
				"reason": "source PG had TDE enabled at backup time; restored bytes are ciphertext and PG's plaintext checksums cannot match in-process. Boot the restored datadir under a TDE-capable PG with key access to run a meaningful pg_verifybackup.",
				"engine": m.SourceTDE.Engine,
			}))
	} else if vbRes, err := verifybackup.Verify(ctx, m.PGBackupManifest, opts.TargetDir); err != nil {
		if errors.Is(err, verifybackup.ErrNoManifest) {
			emit(output.NewEvent(output.SeverityNotice, "restore", "verifybackup_skipped_no_manifest").
				WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
				WithBody(map[string]any{
					"reason": "backup pre-dates Manifest.PGBackupManifest field; in-process verifybackup unavailable",
				}))
		} else {
			return nil, output.NewError("restore.verifybackup_failed",
				fmt.Sprintf("restore: %v", err)).Wrap(err)
		}
	} else if vbRes != nil {
		emit(output.NewEvent(output.SeverityInfo, "restore", "verifybackup_ok").
			WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
			WithBody(map[string]any{
				"files_checked": vbRes.FilesChecked,
				"bytes_hashed":  vbRes.BytesHashed,
				"algorithm":     vbRes.Algorithm,
			}))
	}

	// 5a. Checkpoint clear — the data dir is now fully materialised,
	// so the bookkeeping file is no longer useful. Clear it so the
	// next restore into this dir doesn't think it's resuming.
	// Recovery files write AFTER this; a crash between the clear and
	// the recovery write leaves the operator with a fully-
	// materialised but not-yet-armed-for-PITR data dir — easy to
	// recover from manually.
	if err := cw.Clear(); err != nil {
		emit(output.NewEvent(output.SeverityWarning, "restore", "checkpoint_clear_failed").
			WithBody(map[string]any{"error": err.Error()}))
	}

	// 6. PITR plumbing. recovery.signal + recovery_* GUCs in
	//    postgresql.auto.conf go in last so the data dir is fully
	//    materialised before PG sees it.
	if opts.Recovery != nil && opts.Recovery.Enable {
		if err := WriteRecoveryFiles(opts.TargetDir, *opts.Recovery); err != nil {
			return nil, output.NewError("restore.recovery_write",
				fmt.Sprintf("restore: write recovery files: %v", err)).Wrap(err)
		}
		emit(output.NewEvent(output.SeverityInfo, "restore", "recovery_armed").
			WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
			WithBody(map[string]any{
				"target_lsn":  opts.Recovery.TargetLSN,
				"target_time": formatRecoveryTime(opts.Recovery.TargetTime),
				"target_name": opts.Recovery.TargetName,
				"action":      defaultIfEmpty(opts.Recovery.Action, "pause"),
				"timeline":    defaultIfEmpty(opts.Recovery.Timeline, "latest"),
				"inclusive":   opts.Recovery.Inclusive,
			}))
	} else {
		// 6b. NON-PITR ("restore latest known good") still needs
		// a signal file + restore_command so the cluster can reach
		// consistency on startup.  A pg_hardstorage backup snapshots
		// PGDATA live, so the restored pg_control records a
		// checkpoint LSN whose trailing WAL segment isn't bundled
		// into the restored pg_wal/.  Without restore_command, PG
		// sits in standby waiting forever; this was the dominant
		// failure cluster in soak testing (10 of 16 soak cells + the
		// test-wal-stream-suite assert).  Writing standby.signal +
		// recovery_target='immediate' + a `<agent> wal fetch` restore_command
		// here means downstream cluster-start gates — postverify,
		// the testkit's sandbox, the operator's own `pg_ctl start`
		// after `pg_hardstorage restore` — all just work.
		if err := WriteAutoRecovery(opts.TargetDir, m.Deployment, opts.RepoURL); err != nil {
			return nil, output.NewError("restore.auto_recovery_write",
				fmt.Sprintf("restore: stage auto-recovery: %v", err)).Wrap(err)
		}
		emit(output.NewEvent(output.SeverityInfo, "restore", "auto_recovery_armed").
			WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
			WithBody(map[string]any{
				"signal":          "standby.signal",
				"recovery_target": "immediate",
				"restore_command": "wired",
			}))
	}

	// L3 — post-restore cluster-start smoke test.  Catches
	// the issue-#7-class bug class (empty PGDATA dirs missing,
	// permissions broken, tablespace symlinks dangling) that
	// L1 + L2 cannot see.  See postverify package docstring.
	//
	// Library default is OFF — callers building synthetic
	// manifests (most unit tests) shouldn't trigger a real
	// pg_ctl probe.  The CLI flips the default to "auto" so
	// operators get the gate without opting in.
	if opts.VerifyMode == "" {
		opts.VerifyMode = string(postverify.ModeOff)
	}
	if mode, perr := postverify.ParseMode(opts.VerifyMode); perr != nil {
		return nil, output.NewError("restore.verify_mode_invalid",
			"restore: "+perr.Error()).Wrap(perr)
	} else if mode != postverify.ModeOff {
		recoveryArmed := opts.Recovery != nil && opts.Recovery.Enable
		pvRes, err := postverify.Verify(ctx, postverify.Options{
			Mode:           mode,
			DataDir:        opts.TargetDir,
			PGMajorVersion: m.PGVersion,
			RecoveryArmed:  recoveryArmed,
			RepoURL:        opts.RepoURL,
			Deployment:     m.Deployment,
		})
		if err != nil {
			return nil, output.NewError("restore.postverify_failed",
				fmt.Sprintf("restore: %v", err)).Wrap(err)
		}
		if pvRes.Skipped {
			emit(output.NewEvent(output.SeverityWarning, "restore", "postverify_skipped").
				WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
				WithBody(map[string]any{
					"reason": pvRes.SkipReason,
					"mode":   string(mode),
					"hint":   "install postgresql-client/server on the runner host or pass --verify-restore=off to silence",
				}))
		} else {
			body := map[string]any{
				"mode":           string(mode),
				"start_ms":       pvRes.StartDuration.Milliseconds(),
				"queries_ran":    pvRes.QueriesRan,
				"recovery_armed": recoveryArmed,
			}
			if pvRes.DumpRan {
				body["dump_ran"] = true
				body["dump_ms"] = pvRes.DumpDuration.Milliseconds()
			}
			emit(output.NewEvent(output.SeverityInfo, "restore", "postverify_ok").
				WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
				WithBody(body))
		}
	}

	stoppedAt := time.Now().UTC()
	res = &Result{
		BackupID:          m.BackupID,
		Deployment:        m.Deployment,
		TargetDir:         opts.TargetDir,
		FileCount:         len(m.Files),
		BytesWritten:      bytesWritten,
		ChunksFetched:     chunksFetched,
		BackupLabelSize:   len(m.BackupLabel),
		TablespaceMapSize: len(m.TablespaceMap),
		StartedAt:         startedAt,
		StoppedAt:         stoppedAt,
		Duration:          stoppedAt.Sub(startedAt),
	}
	emit(output.NewEvent(output.SeverityInfo, "restore", "completed").
		WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
		WithBody(map[string]any{
			"file_count":     res.FileCount,
			"bytes_written":  res.BytesWritten,
			"chunks_fetched": res.ChunksFetched,
			"duration_ms":    res.Duration.Milliseconds(),
		}))

	// Append a restore.complete record to the hash-chained audit
	// log. Best-effort: a write failure here doesn't fail the
	// restore — the bytes are on disk, the operator's job is done.
	// We surface the warning through OnEvent so monitoring sees the
	// gap, and `audit verify-chain` will surface a sequence skip.
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	auditEv := &audit.Event{
		Action: "restore.complete",
		Actor:  opts.Actor,
		Tenant: m.Tenant,
		Subject: audit.Subject{
			Deployment: m.Deployment,
			BackupID:   m.BackupID,
			Tenant:     m.Tenant,
			Repo:       opts.RepoURL,
		},
		Body: map[string]any{
			"target_dir":     opts.TargetDir,
			"file_count":     res.FileCount,
			"bytes_written":  res.BytesWritten,
			"chunks_fetched": res.ChunksFetched,
			"duration_ms":    res.Duration.Milliseconds(),
			"resumed":        resumed,
			"recovery_armed": opts.Recovery != nil && opts.Recovery.Enable,
		},
	}
	if err := auditStore.Append(ctx, auditEv); err != nil {
		emit(output.NewEvent(output.SeverityWarning, "restore", "audit_append_failed").
			WithSubject(output.Subject{Deployment: m.Deployment, BackupID: m.BackupID}).
			WithBody(map[string]any{"error": err.Error()}))
	}
	return res, nil
}

// validateOptions enforces required fields and surfaces structured
// usage errors.
func validateOptions(o *Options) error {
	if o.RepoURL == "" {
		return output.NewError("usage.missing_repo_url",
			"restore: RepoURL is required").Wrap(output.ErrUsage)
	}
	if o.Deployment == "" {
		return output.NewError("usage.missing_deployment",
			"restore: Deployment is required").Wrap(output.ErrUsage)
	}
	if o.BackupID == "" {
		return output.NewError("usage.missing_backup_id",
			"restore: BackupID is required").Wrap(output.ErrUsage)
	}
	if o.TargetDir == "" {
		return output.NewError("usage.missing_target_dir",
			"restore: TargetDir is required").Wrap(output.ErrUsage)
	}
	if o.Verifier == nil {
		return output.NewError("usage.missing_verifier",
			"restore: Verifier is required (we don't restore from unverified manifests)").
			Wrap(output.ErrUsage)
	}
	return nil
}

// preflightTarget refuses to write into an existing non-empty
// directory unless AllowOverwrite, with one inviolable
// exception: a target whose `postmaster.pid` names a LIVE
// PostgreSQL process is refused unconditionally — `--force`
// over a running cluster's datadir corrupts the cluster
// instantly.  The operator must stop PG first.
//
// The triage produces five distinct outcomes (operator-
// visible structured error codes in parens):
//
//	empty / non-existent       → proceed (no error).
//	resume-eligible (only our
//	  checkpoint marker)       → proceed (resume of prior
//	                              attempt; the restore loop
//	                              re-validates the checkpoint
//	                              vs the requested backup).
//	postmaster.pid + live PID  → REFUSE always
//	                              (`preflight.target_running_postgres`).
//	PG_VERSION present         → REFUSE without --force, with
//	                              an upgraded message naming
//	                              the existing datadir
//	                              (`preflight.target_pg_datadir`).
//	other non-empty            → REFUSE without --force
//	                              (`preflight.target_not_empty`).
func preflightTarget(target string, allowOverwrite bool, expectedSystemID string, allowForeignCluster bool) error {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil
		}
		return output.NewError("preflight.target_stat",
			fmt.Sprintf("restore: stat target %q: %v", target, err)).Wrap(err)
	}
	if !info.IsDir() {
		return output.NewError("preflight.target_not_dir",
			fmt.Sprintf("restore: target %q exists but is not a directory", target))
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return output.NewError("preflight.target_read",
			fmt.Sprintf("restore: read target %q: %v", target, err)).Wrap(err)
	}
	if len(entries) == 0 {
		return nil
	}

	// Resume-eligible: dir contains our checkpoint marker.
	// The restore loop's checkpoint-match logic will refuse
	// loudly if the checkpoint disagrees with the requested
	// backup; silently allowing this case here is safe.
	hasOnlyCheckpoint, err := targetIsResumeEligible(target, entries)
	if err != nil {
		return output.NewError("preflight.checkpoint_check_failed",
			fmt.Sprintf("restore: cannot determine if %q is resume-eligible: %v", target, err)).
			Wrap(err).
			WithSuggestion(&output.Suggestion{
				Human: "fix the underlying error (often permissions on the target dir or its checkpoint file), then retry",
			})
	}
	if hasOnlyCheckpoint {
		return nil
	}

	// CRITICAL: refuse to overwrite a running cluster's
	// datadir even when --force is set.  postmaster.pid's
	// first line is the postmaster's PID; if that PID is
	// alive we'd be guaranteeing data loss.  No flag
	// overrides this — the operator stops PG first.
	pid, pmPresent := readPostmasterPID(target)
	if pid > 0 && processAlive(pid) {
		return output.NewError("preflight.target_running_postgres",
			fmt.Sprintf("restore: target %q has a running PostgreSQL (postmaster.pid PID=%d); refusing to overwrite", target, pid)).
			WithSuggestion(&output.Suggestion{
				Human:   fmt.Sprintf("stop PG first (e.g. `pg_ctl -D %s stop -m fast`), then retry. --force does NOT override this check.", target),
				Command: fmt.Sprintf("pg_ctl -D %s stop -m fast", target),
			})
	}
	// postmaster.pid is present but we couldn't extract a live/dead
	// verdict (unreadable / partially-written / corrupted first line).
	// We CANNOT prove PG is stopped, so we must not let --force
	// overwrite — that's the exact data-loss this gate exists to
	// prevent, and a malformed lockfile must not be a way around it.
	// A genuinely-stale file (PG really is down) is cleared by stopping
	// PG or removing the file, after which the restore proceeds.
	if pid == 0 && pmPresent {
		return output.NewError("preflight.target_postmaster_unverifiable",
			fmt.Sprintf("restore: target %q has a postmaster.pid we can't read or parse; cannot verify PostgreSQL is stopped, refusing to overwrite", target)).
			WithSuggestion(&output.Suggestion{
				Human:   fmt.Sprintf("confirm no PostgreSQL is running against %s and stop it if so (`pg_ctl -D %s stop -m fast`); if the cluster is definitely down, remove the stale postmaster.pid and retry. --force does NOT override this check.", target, target),
				Command: fmt.Sprintf("pg_ctl -D %s stop -m fast", target),
			})
	}

	if !allowOverwrite {
		// PG_VERSION + (stale) postmaster.pid + a directory
		// shaped like a real datadir → louder refusal so the
		// operator sees the consequence of --force before
		// they reach for it.
		if hasFile(entries, "PG_VERSION") {
			return output.NewError("preflight.target_pg_datadir",
				fmt.Sprintf("restore: target %q is an existing PostgreSQL datadir (PG_VERSION present); refusing to overwrite", target)).
				WithSuggestion(&output.Suggestion{
					Human: "verify this is the right path and that no service depends on the existing cluster, then either move the existing contents aside (`mv " + target + " " + target + ".pre-restore`) or pass --force to overwrite. The contents will be deleted irrecoverably.",
				})
		}
		return output.NewError("preflight.target_not_empty",
			fmt.Sprintf("restore: target %q is not empty (%d entries)", target, len(entries))).
			WithSuggestion(&output.Suggestion{
				Human: "either point --target at a fresh directory, move the existing contents aside, or pass --force to overwrite (the existing contents will be deleted irrecoverably).",
			})
	}

	// --force is set (we are about to overwrite). One last guard,
	// overridable only by the narrower --force-foreign: if the target is
	// itself a real datadir (PG_VERSION present) whose pg_control system
	// identifier differs from the backup's, the operator almost
	// certainly pointed --target at the WRONG, still-wanted cluster.
	// Destroying a different cluster is exactly the mistake --force
	// can't see, so refuse it here. See issue #100.
	if !allowForeignCluster && expectedSystemID != "" && hasFile(entries, "PG_VERSION") {
		if targetID, ok := readControlSystemIdentifier(target); ok && targetID != expectedSystemID {
			return output.NewError("preflight.target_foreign_cluster",
				fmt.Sprintf("restore: target %q is a DIFFERENT PostgreSQL cluster (pg_control system identifier %s != backup %s); refusing to overwrite even with --force", target, targetID, expectedSystemID)).
				WithSuggestion(&output.Suggestion{
					Human: "you likely pointed --target at the wrong, still-wanted cluster — double-check the path. If you really mean to overwrite a different cluster (e.g. repurposing hardware), pass --force-foreign.",
				})
		}
	}
	return nil
}

// readControlSystemIdentifier reads the database system identifier from
// <target>/global/pg_control. The identifier is ControlFileData's first
// field — a uint64 at offset 0 — which PostgreSQL deliberately keeps at
// the very front (pg_control_version sits 8 bytes in), so this is stable
// across PG majors and needs no version-specific layout knowledge. The
// control file is written in native byte order by the local server;
// pg_hardstorage targets little-endian platforms. Returns (decimal
// string, true) on success, ("", false) when the file is absent,
// unreadable, too short, or yields a zero identifier (not usable).
func readControlSystemIdentifier(target string) (string, bool) {
	body, err := os.ReadFile(filepath.Join(target, "global", "pg_control"))
	if err != nil || len(body) < 8 {
		return "", false
	}
	id := binary.LittleEndian.Uint64(body[:8])
	if id == 0 {
		return "", false
	}
	return strconv.FormatUint(id, 10), true
}

// clearDirContents removes every entry inside dir, leaving dir itself
// in place (so a mount point survives). Used to honor the documented
// --force contract — "the existing contents will be deleted
// irrecoverably" — by actually replacing the target rather than writing
// the backup over whatever was there (which would leave stale files
// from a previous occupant mixed with the restore: a silently-corrupt
// datadir). See data-loss audit round 3, path #1.
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// preflightTablespaceTargets guards the EXTERNAL tablespace
// directories a restore redirects data into (the New side of each
// --tablespace-mapping). preflightTarget only protects the main PGDATA;
// without this, a restore could overwrite another live/wanted cluster's
// tablespace at the remap target, and a chain restore's
// pg_combinebackup requires those dirs to be empty/absent anyway. A
// non-empty target is refused unless allowOverwrite; with --force the
// dir is cleared (same contract as the main target). Absent or empty
// dirs pass untouched. See data-loss audit round 3, path #2.
func preflightTablespaceTargets(remap TablespaceRemap, allowOverwrite bool) error {
	for _, dir := range remap.AppliedPaths() {
		info, err := os.Stat(dir)
		if err != nil {
			if errors.Is(err, stdfs.ErrNotExist) {
				continue // absent → pg_combinebackup / PG will create it
			}
			return output.NewError("preflight.tablespace_stat",
				fmt.Sprintf("restore: stat tablespace target %q: %v", dir, err)).Wrap(err)
		}
		if !info.IsDir() {
			return output.NewError("preflight.tablespace_not_dir",
				fmt.Sprintf("restore: tablespace target %q exists but is not a directory", dir))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return output.NewError("preflight.tablespace_read",
				fmt.Sprintf("restore: read tablespace target %q: %v", dir, err)).Wrap(err)
		}
		if len(entries) == 0 {
			continue
		}
		if !allowOverwrite {
			return output.NewError("preflight.tablespace_not_empty",
				fmt.Sprintf("restore: tablespace target %q is not empty (%d entries); refusing to overwrite", dir, len(entries))).
				WithSuggestion(&output.Suggestion{
					Human: "point --tablespace-mapping at a fresh directory, move the existing contents aside, or pass --force to overwrite (the existing contents will be deleted irrecoverably).",
				})
		}
		if err := clearDirContents(dir); err != nil {
			return output.NewError("restore.tablespace_clear_failed",
				fmt.Sprintf("restore: clear --force tablespace target %q: %v", dir, err)).Wrap(err)
		}
	}
	return nil
}

// readPostmasterPID returns the PID PG wrote to the first line of
// postmaster.pid plus whether the file was present at all.
//
//	pid > 0, present       — a usable PID (caller probes liveness)
//	pid == 0, present=true  — postmaster.pid EXISTS but couldn't be
//	                          read or parsed (permission, partial
//	                          write, corruption). The caller treats
//	                          this as "can't rule out a running
//	                          cluster" and refuses — distinguishing it
//	                          from absent is what makes the
//	                          running-PG gate fail CLOSED instead of
//	                          letting --force overwrite a live datadir
//	                          whose lockfile we merely failed to parse.
//	pid == 0, present=false — no postmaster.pid; not a locked datadir.
func readPostmasterPID(target string) (pid int, present bool) {
	body, err := os.ReadFile(filepath.Join(target, "postmaster.pid"))
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return 0, false
		}
		// Present but unreadable (permission, transient I/O error).
		return 0, true
	}
	// First line is the PID; subsequent lines are
	// data-dir / port / socket-dir / start-time / shmem-key.
	lines := strings.SplitN(string(body), "\n", 2)
	if len(lines) == 0 {
		return 0, true
	}
	pid, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return 0, true // present but unparseable
	}
	return pid, true
}

// processAlive reports whether pid names a process that
// currently exists.  Uses Unix kill(2) with signal 0, which
// reports liveness without delivering a real signal.  On any
// platform without signal-0 semantics the call returns false;
// a true positive (live PG) is the case we care about — a
// false negative (live PG misclassified as dead) only allows
// `--force` to corrupt a running cluster, which is at most
// the pre-fix behaviour the operator already lived with.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is the documented "is the process there?"
	// query on POSIX.  No signal is delivered; the only
	// effect is the kernel's reachability check.
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		return true
	}
	// On EPERM the process exists but we don't have
	// permission to signal it — still alive, still must
	// refuse.  ESRCH means it's gone.
	return errors.Is(err, syscall.EPERM)
}

// hasFile reports whether the entries list contains a file
// (not a dir) named `name` at the top level of the target
// directory.  Used by the PG_VERSION sniff above.
func hasFile(entries []os.DirEntry, name string) bool {
	for _, e := range entries {
		if e.Name() == name && !e.IsDir() {
			return true
		}
	}
	return false
}

// targetIsResumeEligible reports whether the target dir's contents
// look like a previous restore that crashed mid-way: at minimum a
// checkpoint file is present, and every other entry is something
// the checkpoint says we already wrote.
//
// We're permissive: any presence of the checkpoint file is treated
// as resume-eligible, because the restore loop's own checkpoint-
// match logic will refuse loudly if the contents disagree with the
// requested backup. False positives here just defer the loud
// refusal to the restore loop, which is fine.
func targetIsResumeEligible(target string, entries []os.DirEntry) (bool, error) {
	for _, e := range entries {
		if e.Name() == CheckpointFilename {
			return true, nil
		}
	}
	return false, nil
}

// materializeFile creates the FileEntry's path under its resolved
// destination root and writes the chunks in order. The destination is
// opened with O_TRUNC so a retry over a non-empty target overwrites
// cleanly.
//
// destRoot is the directory the FileEntry's (root-relative) Path is
// joined onto: TargetDir for a default-tablespace file (OID 0), or the
// non-default tablespace's real on-disk location (from tablespace_map,
// after any operator remap) for a tablespace file. The join is
// escape-guarded relative to destRoot, so a tablespace file's Path
// still cannot climb out of its tablespace root.
//
// Returns (bytesWritten, chunksFetched, error). Named returns are
// load-bearing — the deferred Close-error capture writes back into
// `err` when Close itself fails.
func materializeFile(ctx context.Context, cas *repo.CAS, destRoot string, f *backup.FileEntry) (bytesWritten int64, chunksFetched int, err error) {
	full, err := safeJoinTarget(destRoot, f.Path)
	if err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, 0, fmt.Errorf("mkdir parent: %w", err)
	}

	mode := stdfs.FileMode(f.Mode)
	if mode == 0 {
		mode = 0o600 // PG default for data files
	}
	dst, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, 0, fmt.Errorf("open destination: %w", err)
	}
	// Close-error capture: a deferred plain `dst.Close()` swallows
	// any error Close itself raises (rare on Linux for plain files,
	// but possible on NFS / FUSE / quota-exhausted volumes — and
	// guaranteed if the kernel flushes a delayed write at Close
	// time). Wrap so a Close failure becomes the function's return
	// when nothing else has gone wrong.
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close destination: %w", cerr)
		}
	}()

	// bytesWritten is the named return; the loop accumulates into it
	// directly so the deferred Close-error capture doesn't observe a
	// stale value through a shadowed local.
	for i, ref := range f.Chunks {
		if err := ctx.Err(); err != nil {
			return bytesWritten, i, err
		}
		body, err := cas.GetChunkBytes(ctx, ref.Hash)
		if err != nil {
			return bytesWritten, i, fmt.Errorf("fetch chunk %s: %w", ref.Hash, err)
		}
		if int64(len(body)) != ref.Len {
			return bytesWritten, i, fmt.Errorf("chunk %s len mismatch: got %d, manifest says %d",
				ref.Hash, len(body), ref.Len)
		}
		if _, err := dst.Write(body); err != nil {
			return bytesWritten, i, fmt.Errorf("write chunk %s: %w", ref.Hash, err)
		}
		bytesWritten += int64(len(body))
	}

	// Concatenated chunks must equal the declared file size. A
	// mismatch means the manifest is internally inconsistent — surface
	// loudly rather than silently produce a wrong-size file.
	if bytesWritten != f.Size {
		return bytesWritten, len(f.Chunks),
			fmt.Errorf("size mismatch: chunks total %d, manifest says %d", bytesWritten, f.Size)
	}

	if err := dst.Sync(); err != nil {
		return bytesWritten, len(f.Chunks), fmt.Errorf("fsync: %w", err)
	}
	return bytesWritten, len(f.Chunks), nil
}

// tablespaceDestRoots maps each non-default tablespace OID to the
// ABSOLUTE directory its files must be materialised under.  The
// mapping starts from the manifest's recorded Tablespace.Location
// (where the tablespace lived on the source cluster) and applies the
// operator's --tablespace-mapping remap so restore writes to the
// target-cluster path the operator chose — the SAME path the rewritten
// tablespace_map records, so PG's recovery-time pg_tblspc/ symlinks
// point at the bytes we actually wrote.
//
// A tablespace is "non-default" — and thus gets an entry here — only
// when its Location is an ABSOLUTE path.  PG reports the main data
// directory's archive with OID 0 and an EMPTY Location; some tooling
// records the implicit pg_default tablespace with its catalog OID
// (1663) and a non-absolute location like "pg_default".  Neither is a
// real external tablespace: their files belong under PGDATA root.
// Keying on "absolute Location" is the reliable discriminator (a real
// CREATE TABLESPACE always has an absolute directory) and keeps those
// PGDATA-root files routed to TargetDir via fileDestRoot's fallback.
func tablespaceDestRoots(m *backup.Manifest, remap TablespaceRemap) map[uint32]string {
	out := make(map[uint32]string, len(m.Tablespaces))
	for _, ts := range m.Tablespaces {
		if ts.OID == 0 || !filepath.IsAbs(ts.Location) {
			continue
		}
		dest := ts.Location
		if !remap.Empty() {
			// Reuse the tablespace_map rewrite logic so the path we
			// write to and the path PG reads from tablespace_map stay
			// identical.  Apply operates on "<oid> <path>" lines; feed
			// it a single synthetic line and read the path back.
			rewritten := remap.Apply(fmt.Sprintf("%d %s\n", ts.OID, ts.Location))
			if p := parseTablespaceMapLinePath(rewritten); p != "" {
				dest = p
			}
		}
		out[ts.OID] = dest
	}
	return out
}

// parseTablespaceMapLinePath extracts the PATH from a single
// "<oid> <path>\n" tablespace_map line.  Returns "" when the line is
// malformed.  Used by tablespaceDestRoots to read back a remapped
// location without duplicating TablespaceRemap.Apply's line grammar.
func parseTablespaceMapLinePath(line string) string {
	line = strings.TrimRight(line, "\n")
	i := strings.Index(line, " ")
	if i <= 0 || i == len(line)-1 {
		return ""
	}
	return line[i+1:]
}

// fileDestRoot returns the directory a FileEntry's (root-relative)
// Path is joined onto.
//
//   - A file whose OID resolves to a real external tablespace (one
//     with an ABSOLUTE Location, present in dests) is materialised
//     under that tablespace's on-disk location — NOT flattened under
//     PGDATA root.  This is the core of the bug-3 fix.
//   - Every other file — OID 0 (the main data directory's archive) or
//     an OID that maps only to a PGDATA-root pseudo-tablespace like
//     pg_default (non-absolute location, absent from dests) — is
//     materialised under TargetDir, because its Path is already
//     PGDATA-relative.
func fileDestRoot(target string, dests map[uint32]string, oid uint32) (string, error) {
	if oid == 0 {
		return target, nil
	}
	if dest, ok := dests[oid]; ok {
		return dest, nil
	}
	// Non-zero OID with no absolute-location tablespace: PGDATA-root.
	return target, nil
}

// checkpointKey is the resume-checkpoint identity for a FileEntry.
// It composes the tablespace OID and the path so two tablespaces that
// carry the same relative path (their tars are both rooted at
// PG_<ver>_<cat>/...) get distinct checkpoint entries.  OID 0 (the
// default tablespace) yields "0\x00<path>" — stable and back-compat
// safe because pre-tablespace-fix restores only ever had OID-0 files.
func checkpointKey(f *backup.FileEntry) string {
	return fmt.Sprintf("%d\x00%s", f.TablespaceOID, f.Path)
}

// writeSpecial materialises one of the manifest's special files
// (backup_label, tablespace_map). Mode 0600 — PG runs as a single
// user and these files don't need group / world readability.
//
// Durability: these files determine PG's recovery behaviour on the
// next start.  fsutil.WriteFileSync flushes the file inode AND
// fsyncs the parent dir — without the parent-dir flush a power
// loss between WriteFile() returning and the dentry being flushed
// could erase the file's existence even though we believe we
// committed it.
func writeSpecial(target, name string, body []byte) error {
	full := filepath.Join(target, name)
	if err := fsutil.WriteFileSync(full, body, 0o600); err != nil {
		return fmt.Errorf("restore: write %s: %w", name, err)
	}
	return nil
}

// safeJoinTarget joins target + rel and refuses any result that escapes
// target via ".." or an absolute path.  Defence-in-depth against a
// malicious manifest whose FileEntry.Path contains traversal tokens —
// manifests are signed, so the threat is a malicious operator (or a key
// compromise) rather than an outside attacker, but the cost of the check
// is trivial and a refusal is much better than a silent write to /etc.
//
// SCOPE — this is a purely LEXICAL check (filepath.Clean + filepath.Rel);
// it does NOT resolve symlinks (no Lstat / EvalSymlinks).  It is sufficient
// today ONLY because restore creates no symlinks from manifest data
// (FileEntry/DirEntry carry no link target) and clears the target before
// materialising, so there is never an in-target symlink to write through.
// If a future change starts materialising symlinks from the manifest, this
// guard becomes INSUFFICIENT on its own — a symlink pointing outside the
// target plus a later write through it escapes the lexical check.  Such a
// change must additionally O_NOFOLLOW / lstat every path component, or
// validate symlink targets, before trusting this function.
//
// Implementation notes:
//
//   - filepath.Clean reduces ".." sequences against the literal join
//     before we compare; so "base/../foo" canonicalises to "foo"
//     under target — that's safe.  "../etc/passwd" canonicalises to
//     "../etc/passwd" relative to target, which is detected by the
//     prefix check.
//   - We reject absolute rel paths up front (Windows uses "C:\…",
//     POSIX uses "/…"); both would otherwise overwrite filepath.Join
//     and write outside target.
//   - filepath.Rel is the canonical "is X under Y?" tool: when rel
//     starts with ".." the path escapes; when err is non-nil the
//     paths are on different volumes (Windows) and we refuse.
func safeJoinTarget(target, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("restore: empty file path in manifest")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("restore: refusing absolute path %q in manifest", rel)
	}
	full := filepath.Join(target, rel)
	cleanTarget := filepath.Clean(target)
	cleanFull := filepath.Clean(full)
	relCheck, err := filepath.Rel(cleanTarget, cleanFull)
	if err != nil {
		return "", fmt.Errorf("restore: path %q is on a different volume than target %q", rel, target)
	}
	if relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("restore: refusing path %q that escapes target %q", rel, target)
	}
	return cleanFull, nil
}

// formatRecoveryTime renders r as an empty string if zero, RFC3339
// otherwise. Used for event-body display only; the on-disk GUC value
// uses PG's preferred space-separated format.
func formatRecoveryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// defaultIfEmpty returns def when s is "".
func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// mapRepoErr classifies repo.Open errors with the same taxonomy the
// backup runner uses. Keeping them consistent matters: scripts compare
// exit codes across both subcommands.
func mapRepoErr(url string, err error) error {
	if errors.Is(err, repo.ErrNotARepo) {
		return output.NewError("notfound.repo",
			fmt.Sprintf("restore: no pg_hardstorage repository at %s", url)).Wrap(err)
	}
	return fmt.Errorf("restore: open repo: %w", err)
}

// restoreIncrementalChain implements PG 17+ incremental-chain
// restore. The leaf manifest's parent_backup_id walks back to the
// full anchor via combine.Build; each link is materialised into its
// own staging dir; pg_combinebackup merges them into TargetDir.
//
// The flow is intentionally distinct from the regular full-restore
// path:
//
//   - Pre-flight pg_combinebackup BEFORE any I/O so a missing binary
//     fails fast instead of after a long materialisation.
//
//   - Each chain link materialises in full (file content + backup_label
//
//   - tablespace_map + PG's own backup_manifest); pg_combinebackup
//     refuses any input dir without the latter.
//
//   - TargetDir must NOT pre-exist when pg_combinebackup runs (the
//     binary creates it). When the operator passed an empty existing
//     dir we remove it post-preflight; non-empty + AllowOverwrite=false
//     was already refused by preflightTarget.
//
//   - Per-link checkpoint resume IS supported (audit).
//     Each chain link materialises into a stable staging
//     directory under ChainStagingRoot (default: os.TempDir
//     keyed on deployment + leaf_backup_id).  After each
//     successful materialise we drop a chainLinkCompleteFilename
//     marker that records the link's BackupID + chunk count.
//     On re-entry, links whose marker matches the current
//     manifest are skipped — only links whose marker is missing
//     or mismatched re-run.
//
//     Cleanup happens only on full success (pg_combinebackup
//     completes).  Failures preserve staging for the next
//     attempt.  Operators force a fresh start with
//     ResetChainStaging=true / --reset-chain-staging.
//
//     pg_combinebackup itself remains end-to-end (the merge
//     step is atomic from PG's perspective); resume only
//     applies to the materialisation phase, which is the
//     dominant cost on a long chain.
func restoreIncrementalChain(ctx context.Context, opts Options, sp storage.StoragePlugin, repoMeta *repo.Metadata, leaf *backup.Manifest, emit func(*output.Event), startedAt time.Time) (*Result, error) {
	chainCtx, chainSpan := tracing.Tracer().Start(ctx, "restore.chain",
		trace.WithAttributes(
			attribute.String("deployment", opts.Deployment),
			attribute.String("leaf_backup_id", opts.BackupID),
		))
	defer chainSpan.End()

	// 1. Pre-flight pg_combinebackup. Cheap; fails fast.
	//
	// pg_combinebackup is version-locked: PG 18's binary running on
	// PG 17 backup data produces "CRC is incorrect" on every chain
	// link's pg_control because the control-file layout differs
	// between majors. Pick the binary whose major matches the leaf
	// manifest's pg_version so a host that happens to have multiple
	// PG installations (or wants to restore an older major's chain
	// after the cluster has been upgraded) gets the right tool.
	combineBin, err := combine.DiscoverPGCombineBackupForMajor(leaf.PGVersion)
	if err != nil {
		return nil, output.NewError("preflight.pg_combinebackup_missing",
			fmt.Sprintf("restore: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: fmt.Sprintf("install the PostgreSQL %d server/client package for your distro (PGDG: postgresql%d on RHEL/Fedora, postgresql-%d on Debian/Ubuntu) so /usr/pgsql-%d/bin/pg_combinebackup (or /usr/lib/postgresql/%d/bin/pg_combinebackup) is present; OR set PG_COMBINEBACKUP_%d=<path> to point at a specific binary. The pg_combinebackup utility is shipped with the matching PostgreSQL major and is NOT compatible across majors.",
					leaf.PGVersion, leaf.PGVersion, leaf.PGVersion, leaf.PGVersion, leaf.PGVersion, leaf.PGVersion),
			}).
			Wrap(err)
	}

	// 2. Pre-flight target dir (non-empty + !AllowOverwrite refused).
	if err := preflightTarget(opts.TargetDir, opts.AllowOverwrite, leaf.SystemIdentifier, opts.AllowForeignCluster); err != nil {
		return nil, err
	}
	// External tablespace targets pg_combinebackup writes into must be
	// empty/absent too — and must not silently clobber a foreign
	// cluster's tablespace. See round-3 #2.
	if err := preflightTablespaceTargets(opts.TablespaceRemap, opts.AllowOverwrite); err != nil {
		return nil, err
	}

	// 3. Build the chain: [full, inc1, ..., leaf].
	chain, err := combine.Build(chainCtx, sp, opts.Deployment, opts.BackupID, opts.Verifier)
	if err != nil {
		return nil, err
	}
	chainSpan.SetAttributes(
		attribute.Int("chain_length", len(chain)),
		attribute.String("anchor_backup_id", chain[0].BackupID),
	)
	emit(output.NewEvent(output.SeverityInfo, "restore", "chain_resolved").
		WithSubject(output.Subject{
			Deployment: opts.Deployment,
			BackupID:   opts.BackupID,
			Tenant:     leaf.Tenant,
		}).
		WithBody(map[string]any{
			"chain_length":     len(chain),
			"anchor_backup_id": chain[0].BackupID,
			"leaf_backup_id":   leaf.BackupID,
		}))

	// 4. Defensive: a chain of length 1 means the leaf had its
	//    Type=incremental but no parent (Build refuses this; we
	//    arrive here only when the chain is honest). Single-link
	//    "chains" should fall through to the full-restore path
	//    instead of running pg_combinebackup (which requires ≥ 2
	//    inputs).
	if !chain.IsIncremental() {
		return nil, output.NewError("chain.degenerate",
			fmt.Sprintf("restore: leaf %q is marked incremental but the resolved chain has only one entry", leaf.BackupID))
	}

	// L1 — manifest self-consistency for EVERY link in the chain.
	// The full-restore path validates its single manifest before
	// touching disk (see Restore ~L334); the chain path validated
	// none, so a malformed anchor or intermediate manifest surfaced
	// only as a downstream materialise/pg_combinebackup failure (or
	// worse, a subtly-wrong merged datadir).  Fail fast, before any
	// I/O, naming the offending link.  See issue #16.
	for i := range chain {
		if verr := chain[i].Validate(); verr != nil {
			return nil, output.NewError("manifest.invalid",
				fmt.Sprintf("restore chain: link %d (%s): %v", i, chain[i].BackupID, verr)).Wrap(verr)
		}
	}

	// 5. Allocate a staging area.  Stable across retries (keyed on
	//    deployment + leaf_backup_id) so a re-run after a crash or
	//    pg_combinebackup failure can skip already-materialised
	//    links via per-link completion markers. .
	//
	//    Cleanup is now CONDITIONAL on full success — failures
	//    preserve staging for the next attempt.  Operators force a
	//    fresh start with ResetChainStaging=true.
	stagingRoot, err := chainStagingPath(opts)
	if err != nil {
		return nil, output.NewError("internal",
			fmt.Sprintf("restore: resolve chain staging: %v", err)).Wrap(err)
	}
	if opts.ResetChainStaging {
		if rmErr := os.RemoveAll(stagingRoot); rmErr != nil && !errors.Is(rmErr, stdfs.ErrNotExist) {
			return nil, output.NewError("internal",
				fmt.Sprintf("restore: reset chain staging %q: %v", stagingRoot, rmErr)).Wrap(rmErr)
		}
		emit(output.NewEvent(output.SeverityNotice, "restore", "chain_staging_reset").
			WithBody(map[string]any{"staging_root": stagingRoot}))
	}
	// Create the staging root securely.  The DEFAULT path lives under
	// the world-writable os.TempDir() at a name derived from
	// (deployment, leaf_backup_id) — predictable, so a hostile local
	// user could pre-create it (and its per-link completion markers)
	// before we run.  A plain MkdirAll SUCCEEDS on such a pre-existing
	// dir, exposing our staged bytes and letting a forged
	// .pg_hardstorage_link_complete.json trick resume into skipping a
	// link (serving attacker-controlled datadir bytes into the merged
	// output).  secureStagingDir refuses any pre-existing staging dir
	// we don't own or whose mode isn't 0700, and creates a fresh one
	// with 0700 otherwise — preserving legitimate resume (a dir WE
	// created and still own) while closing the tamper window.  See
	// data-loss/security audit #44.
	if err := secureStagingDir(stagingRoot); err != nil {
		return nil, err
	}
	// Track success so the deferred cleanup only fires when the
	// whole chain restore (including pg_combinebackup) succeeded.
	// A returned-with-error path leaves staging intact so the next
	// attempt resumes.
	var chainSucceeded bool
	defer func() {
		if !chainSucceeded {
			emit(output.NewEvent(output.SeverityNotice, "restore", "chain_staging_preserved").
				WithBody(map[string]any{
					"staging_root": stagingRoot,
					"hint":         "re-run the same restore command to resume from the last completed link; pass --reset-chain-staging to start over",
				}))
			return
		}
		// Best-effort cleanup; staging is throwaway after success.
		if rerr := os.RemoveAll(stagingRoot); rerr != nil {
			emit(output.NewEvent(output.SeverityWarning, "restore", "chain_staging_leak").
				WithBody(map[string]any{"staging_root": stagingRoot, "error": rerr.Error()}))
		}
	}()

	// 6. Materialise every chain link, skipping any whose
	//    completion marker matches the current manifest.  Audit
	//    v23 #8 — resume after a mid-chain crash or pg_combinebackup
	//    failure without re-fetching every chunk.
	inputDirs := make([]string, 0, len(chain))
	var totalBytes int64
	var totalChunks int
	var resumedLinks int
	for i, m := range chain {
		if err := chainCtx.Err(); err != nil {
			return nil, err
		}
		linkDir := filepath.Join(stagingRoot, fmt.Sprintf("%02d-%s", i, m.BackupID))

		// Resume check: if the link's directory already carries a
		// matching completion marker, skip the materialise.  The
		// marker records BackupID + chunk count; a mismatch (e.g.
		// staging from an unrelated previous restore) invalidates
		// the marker and we re-materialise after wiping.
		expectedChunks := totalChunkCountForManifest(m)
		if marker, ok := readChainLinkMarker(linkDir); ok && marker.BackupID == m.BackupID && marker.ChunkCount == expectedChunks {
			totalBytes += marker.BytesWritten
			totalChunks += marker.ChunkCount
			resumedLinks++
			inputDirs = append(inputDirs, linkDir)
			emit(output.NewEvent(output.SeverityInfo, "restore", "chain_link_resumed").
				WithSubject(output.Subject{
					Deployment: opts.Deployment,
					BackupID:   m.BackupID,
					Tenant:     m.Tenant,
				}).
				WithBody(map[string]any{
					"index":          i,
					"chain_length":   len(chain),
					"chunks_fetched": marker.ChunkCount,
					"bytes_written":  marker.BytesWritten,
					"is_anchor":      i == 0,
				}))
			continue
		}

		// No marker (or stale) → wipe + re-materialise.  Wiping
		// is necessary because a previous crash may have left a
		// partially-written link; pg_combinebackup would then
		// see corrupt input.
		if err := os.RemoveAll(linkDir); err != nil && !errors.Is(err, stdfs.ErrNotExist) {
			return nil, output.NewError("internal",
				fmt.Sprintf("restore chain: clean stale link dir %q: %v", linkDir, err)).Wrap(err)
		}
		if err := os.MkdirAll(linkDir, 0o700); err != nil {
			return nil, output.NewError("internal",
				fmt.Sprintf("restore chain: mkdir link dir: %v", err)).Wrap(err)
		}

		// Per-link CAS: encryption parameters live on each
		// manifest, so we resolve KEK / DEK link-by-link.
		linkCAS := casdefault.New(sp)
		if m.Encryption != nil {
			dec, err := buildEncryptedCAS(ctx, sp, m.Encryption, opts.KEKForRef, opts.UnwrapDEK)
			if err != nil {
				return nil, err
			}
			linkCAS = dec
		}

		bw, ck, merr := materializeManifestInto(chainCtx, linkCAS, m, linkDir)
		if merr != nil {
			return nil, fmt.Errorf("restore chain: materialize %s: %w", m.BackupID, merr)
		}
		// Drop the completion marker AFTER materialise succeeds,
		// so a crash mid-link leaves no marker and the next run
		// re-materialises this link from scratch.
		if err := writeChainLinkMarker(linkDir, chainLinkMarker{
			Schema:       chainLinkMarkerSchema,
			BackupID:     m.BackupID,
			ChunkCount:   ck,
			BytesWritten: bw,
		}); err != nil {
			return nil, output.NewError("internal",
				fmt.Sprintf("restore chain: write completion marker for %s: %v", m.BackupID, err)).Wrap(err)
		}
		totalBytes += bw
		totalChunks += ck
		inputDirs = append(inputDirs, linkDir)
		emit(output.NewEvent(output.SeverityInfo, "restore", "chain_link_materialized").
			WithSubject(output.Subject{
				Deployment: opts.Deployment,
				BackupID:   m.BackupID,
				Tenant:     m.Tenant,
			}).
			WithBody(map[string]any{
				"index":          i,
				"chain_length":   len(chain),
				"file_count":     len(m.Files),
				"bytes_written":  bw,
				"chunks_fetched": ck,
				"is_anchor":      i == 0,
			}))
	}
	if resumedLinks > 0 {
		emit(output.NewEvent(output.SeverityInfo, "restore", "chain_links_resumed_total").
			WithBody(map[string]any{
				"resumed":      resumedLinks,
				"chain_length": len(chain),
				"hint":         "the previous attempt's staging was reused; pass --reset-chain-staging to force a fresh materialise",
			}))
	}

	// 7. pg_combinebackup expects OutputDir not to exist. preflightTarget
	//    already accepted "non-existent OR empty"; if the dir is
	//    present and empty, remove it so pg_combinebackup can create
	//    it from scratch. Non-empty + AllowOverwrite=true is the only
	//    case we allow: in that case we error out structurally
	//    because pg_combinebackup will refuse, and the operator
	//    needs to know.
	if info, statErr := os.Stat(opts.TargetDir); statErr == nil {
		if !info.IsDir() {
			return nil, output.NewError("preflight.target_not_dir",
				fmt.Sprintf("restore chain: target %q exists but is not a directory", opts.TargetDir))
		}
		entries, _ := os.ReadDir(opts.TargetDir)
		if len(entries) > 0 {
			return nil, output.NewError("preflight.chain_target_not_empty",
				fmt.Sprintf("restore chain: pg_combinebackup requires the target dir to not pre-exist or to be empty; %q is non-empty", opts.TargetDir)).
				WithSuggestion(&output.Suggestion{
					Human: "remove the target dir entirely (`rm -rf <dir>`) and re-run; chain restore is atomic and cannot merge into an existing data dir",
				})
		}
		// Empty existing dir → remove so pg_combinebackup can mkdir.
		if rmErr := os.Remove(opts.TargetDir); rmErr != nil {
			return nil, output.NewError("restore.target_remove_failed",
				fmt.Sprintf("restore chain: cannot remove empty target dir %q: %v", opts.TargetDir, rmErr)).Wrap(rmErr)
		}
	}

	// 8. Run pg_combinebackup. Captured stderr is surfaced verbatim
	//    on failure so operators can copy/paste the underlying error.
	//
	// Atomic output (data-loss audit path #5): pg_combinebackup writes
	// into a SIBLING staging dir, which we rename into place only after
	// it succeeds. Writing straight into TargetDir would, on a mid-merge
	// failure or crash, leave a partial, plausible-looking datadir an
	// operator might mistakenly start PG against. The staging dir shares
	// TargetDir's parent (same filesystem) so the finalizing rename is
	// atomic; a failed/crashed merge leaves TargetDir absent.
	combineOut := opts.TargetDir + ".pgcombine-staging"
	_ = os.RemoveAll(combineOut) // clear any leftover from a prior crash
	var stderr bytes.Buffer
	combineCtx, combineSpan := tracing.Tracer().Start(chainCtx, "pg_combinebackup",
		trace.WithAttributes(
			attribute.Int("input_dirs", len(inputDirs)),
			attribute.String("output_dir", opts.TargetDir),
		))
	if err := combine.Run(combineCtx, combine.CombineOptions{
		// Pin the version-matched binary discovered above so a
		// later PATH change between preflight and now can't slip
		// a wrong-major binary in.
		PGCombineBackupPath: combineBin,
		InputDirs:           inputDirs,
		OutputDir:           combineOut,
		// ExtraArgs flows the operator's tablespace remap
		// through to pg_combinebackup as
		// --tablespace-mapping=OLD=NEW. Empty when no
		// remap was requested. pg_combinebackup itself
		// rewrites the OUTPUT dir's tablespace_map AND
		// creates symlinks under pg_tblspc/, so we don't
		// need to pre-rewrite the staging dirs.
		ExtraArgs: opts.TablespaceRemap.ToCombineArgs(),
		Stderr:    &stderr,
	}); err != nil {
		combineSpan.SetStatus(codes.Error, err.Error())
		combineSpan.End()
		_ = os.RemoveAll(combineOut) // never leave a partial datadir behind
		if stderr.Len() > 0 {
			emit(output.NewEvent(output.SeverityError, "restore", "pg_combinebackup_stderr").
				WithBody(map[string]any{"stderr": stderr.String()}))
		}
		return nil, err
	}
	combineSpan.End()

	// Finalize: atomically move the completed merge into place. Only
	// now does TargetDir come into existence with a COMPLETE datadir.
	if err := os.Rename(combineOut, opts.TargetDir); err != nil {
		_ = os.RemoveAll(combineOut)
		return nil, output.NewError("restore.combine_finalize",
			fmt.Sprintf("restore chain: finalize merged output into %q: %v", opts.TargetDir, err)).Wrap(err)
	}

	// pg_combinebackup succeeded — flag chainSucceeded so the
	// deferred staging-cleanup runs.  Failures past this point
	// (recovery-file write, audit emission) leave staging in
	// place, but those operations don't need staging anyway, so
	// preserving it on those failure modes is a harmless cost in
	// exchange for never accidentally wiping a successful merge's
	// inputs.
	chainSucceeded = true

	// L2 — in-process pg_verifybackup over the MERGED output.
	// pg_combinebackup writes a fresh backup_manifest into its output
	// describing the flattened datadir; we hash every file it lists
	// and compare, catching a missing / truncated / corrupted file
	// the merge produced.  The full-restore path runs the same gate
	// (see Restore ~L584); the chain path ran none.  Same TDE skip as
	// the full path: under source-TDE the on-disk bytes are
	// ciphertext and PG's plaintext checksums cannot match in-process.
	// Read the merged manifest from disk (we don't carry it in a
	// manifest struct for the chain).  See issue #16.
	if leaf.SourceTDE != nil {
		emit(output.NewEvent(output.SeverityNotice, "restore", "verifybackup_skipped_tde").
			WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
			WithBody(map[string]any{
				"reason": "source PG had TDE enabled at backup time; merged bytes are ciphertext and PG's plaintext checksums cannot match in-process. Boot the restored datadir under a TDE-capable PG with key access to run a meaningful pg_verifybackup.",
				"engine": leaf.SourceTDE.Engine,
			}))
	} else if mergedManifest, rerr := os.ReadFile(filepath.Join(opts.TargetDir, "backup_manifest")); rerr == nil {
		if vbRes, vberr := verifybackup.Verify(chainCtx, mergedManifest, opts.TargetDir); vberr != nil {
			if errors.Is(vberr, verifybackup.ErrNoManifest) {
				emit(output.NewEvent(output.SeverityNotice, "restore", "verifybackup_skipped_no_manifest").
					WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
					WithBody(map[string]any{"reason": "merged output has no backup_manifest; in-process verifybackup unavailable"}))
			} else {
				return nil, output.NewError("restore.verifybackup_failed",
					fmt.Sprintf("restore chain: %v", vberr)).Wrap(vberr)
			}
		} else if vbRes != nil {
			emit(output.NewEvent(output.SeverityInfo, "restore", "verifybackup_ok").
				WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
				WithBody(map[string]any{
					"files_checked": vbRes.FilesChecked,
					"bytes_hashed":  vbRes.BytesHashed,
					"algorithm":     vbRes.Algorithm,
				}))
		}
	} else {
		emit(output.NewEvent(output.SeverityNotice, "restore", "verifybackup_skipped_no_manifest").
			WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
			WithBody(map[string]any{"reason": fmt.Sprintf("could not read merged backup_manifest: %v", rerr)}))
	}

	// 9. PITR plumbing — same posture as full restore.
	if opts.Recovery != nil && opts.Recovery.Enable {
		if err := WriteRecoveryFiles(opts.TargetDir, *opts.Recovery); err != nil {
			return nil, output.NewError("restore.recovery_write",
				fmt.Sprintf("restore chain: write recovery files: %v", err)).Wrap(err)
		}
		emit(output.NewEvent(output.SeverityInfo, "restore", "recovery_armed").
			WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
			WithBody(map[string]any{
				"target_lsn":  opts.Recovery.TargetLSN,
				"target_time": formatRecoveryTime(opts.Recovery.TargetTime),
				"target_name": opts.Recovery.TargetName,
				"action":      defaultIfEmpty(opts.Recovery.Action, "pause"),
				"timeline":    defaultIfEmpty(opts.Recovery.Timeline, "latest"),
				"inclusive":   opts.Recovery.Inclusive,
			}))
	} else {
		// 9b. NON-PITR chain restore still needs standby.signal +
		// recovery_target='immediate' + a restore_command, exactly
		// like the full-restore path (see WriteAutoRecovery ~L649).
		// A chain's merged datadir has the same "trailing WAL segment
		// not bundled" property as a full snapshot, so without this
		// PG FATALs at startup or waits forever in standby.  The full
		// path grew this else-branch; the chain path had no else at
		// all, so a plain chain restore shipped an unbootable datadir.
		// See issue #15.
		if err := WriteAutoRecovery(opts.TargetDir, leaf.Deployment, opts.RepoURL); err != nil {
			return nil, output.NewError("restore.auto_recovery_write",
				fmt.Sprintf("restore chain: stage auto-recovery: %v", err)).Wrap(err)
		}
		emit(output.NewEvent(output.SeverityInfo, "restore", "auto_recovery_armed").
			WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
			WithBody(map[string]any{
				"signal":          "standby.signal",
				"recovery_target": "immediate",
				"restore_command": "wired",
			}))
	}

	// L3 — post-restore cluster-start smoke test, honouring
	// Options.VerifyMode exactly like the full-restore path (see
	// Restore ~L671).  The chain path previously ran no L3 and never
	// honoured VerifyMode="required", so a merged datadir that PG
	// refuses to start (dangling tablespace symlink, missing empty
	// dir, broken perms) shipped as "success".  Default OFF for the
	// library; the CLI flips it to "auto".  See issue #16.
	if opts.VerifyMode == "" {
		opts.VerifyMode = string(postverify.ModeOff)
	}
	if mode, perr := postverify.ParseMode(opts.VerifyMode); perr != nil {
		return nil, output.NewError("restore.verify_mode_invalid",
			"restore chain: "+perr.Error()).Wrap(perr)
	} else if mode != postverify.ModeOff {
		recoveryArmed := opts.Recovery != nil && opts.Recovery.Enable
		pvRes, pverr := postverify.Verify(chainCtx, postverify.Options{
			Mode:           mode,
			DataDir:        opts.TargetDir,
			PGMajorVersion: leaf.PGVersion,
			RecoveryArmed:  recoveryArmed,
			RepoURL:        opts.RepoURL,
			Deployment:     leaf.Deployment,
		})
		if pverr != nil {
			return nil, output.NewError("restore.postverify_failed",
				fmt.Sprintf("restore chain: %v", pverr)).Wrap(pverr)
		}
		if pvRes.Skipped {
			emit(output.NewEvent(output.SeverityWarning, "restore", "postverify_skipped").
				WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
				WithBody(map[string]any{
					"reason": pvRes.SkipReason,
					"mode":   string(mode),
					"hint":   "install postgresql-client/server on the runner host or pass --verify-restore=off to silence",
				}))
		} else {
			body := map[string]any{
				"mode":           string(mode),
				"start_ms":       pvRes.StartDuration.Milliseconds(),
				"queries_ran":    pvRes.QueriesRan,
				"recovery_armed": recoveryArmed,
			}
			if pvRes.DumpRan {
				body["dump_ran"] = true
				body["dump_ms"] = pvRes.DumpDuration.Milliseconds()
			}
			emit(output.NewEvent(output.SeverityInfo, "restore", "postverify_ok").
				WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
				WithBody(body))
		}
	}

	stoppedAt := time.Now().UTC()
	res := &Result{
		BackupID:          leaf.BackupID,
		Deployment:        leaf.Deployment,
		TargetDir:         opts.TargetDir,
		FileCount:         sumFileCount(chain),
		BytesWritten:      totalBytes,
		ChunksFetched:     totalChunks,
		BackupLabelSize:   len(leaf.BackupLabel),
		TablespaceMapSize: len(leaf.TablespaceMap),
		StartedAt:         startedAt,
		StoppedAt:         stoppedAt,
		Duration:          stoppedAt.Sub(startedAt),
	}
	emit(output.NewEvent(output.SeverityInfo, "restore", "completed").
		WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
		WithBody(map[string]any{
			"chain_length":   len(chain),
			"file_count":     res.FileCount,
			"bytes_written":  res.BytesWritten,
			"chunks_fetched": res.ChunksFetched,
			"duration_ms":    res.Duration.Milliseconds(),
			"incremental":    true,
		}))

	// 10. Audit (best-effort, mirrors the full-restore path).
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	auditEv := &audit.Event{
		Action: "restore.complete",
		Actor:  opts.Actor,
		Tenant: leaf.Tenant,
		Subject: audit.Subject{
			Deployment: leaf.Deployment,
			BackupID:   leaf.BackupID,
			Tenant:     leaf.Tenant,
			Repo:       opts.RepoURL,
		},
		Body: map[string]any{
			"target_dir":     opts.TargetDir,
			"chain_length":   len(chain),
			"file_count":     res.FileCount,
			"bytes_written":  res.BytesWritten,
			"chunks_fetched": res.ChunksFetched,
			"duration_ms":    res.Duration.Milliseconds(),
			"incremental":    true,
			"recovery_armed": opts.Recovery != nil && opts.Recovery.Enable,
		},
	}
	if err := auditStore.Append(chainCtx, auditEv); err != nil {
		emit(output.NewEvent(output.SeverityWarning, "restore", "audit_append_failed").
			WithSubject(output.Subject{Deployment: leaf.Deployment, BackupID: leaf.BackupID}).
			WithBody(map[string]any{"error": err.Error()}))
	}
	return res, nil
}

// materializeManifestInto materialises every FileEntry of m into
// dir, plus the special files pg_combinebackup needs to recognise
// the dir as a valid backup input: backup_label, tablespace_map,
// and PG's own backup_manifest blob (PGBackupManifest).
//
// Returns (bytesWritten, chunksFetched, error). Used only by the
// chain-restore path; the regular full-restore loop materialises
// inline because it interleaves checkpointing.
func materializeManifestInto(ctx context.Context, cas *repo.CAS, m *backup.Manifest, dir string) (int64, int, error) {
	var totalBytes int64
	var totalChunks int

	// Re-create every empty PGDATA dir the manifest captured
	// (pg_tblspc/, pg_dynshmem/, pg_replslot/, pg_serial/,
	// pg_snapshots/, pg_subtrans/, pg_twophase/, pg_wal/, ...).
	// pg_combinebackup REQUIRES these dirs to be present in
	// every input (full + each incremental) — without
	// pg_tblspc/ it bails out with
	//   could not open directory ".../pg_tblspc": No such file
	//   or directory
	// The full-restore path runs the same loop after its tar
	// extraction (see restore.go ~L412); the chain-staging
	// path used to skip it, which broke every incremental
	// restore against PG 17 silently — issue surfaced when the
	// incremental-lifecycle integration test first ran end-to-
	// end against a real PG.  Idempotent: MkdirAll on an
	// existing dir is a no-op.
	for _, d := range m.Dirs {
		if d.Path == "" {
			continue
		}
		full, err := safeJoinTarget(dir, d.Path)
		if err != nil {
			return totalBytes, totalChunks,
				fmt.Errorf("chain materialise: dir %s: %w", d.Path, err)
		}
		mode := os.FileMode(d.Mode)
		if mode == 0 {
			mode = 0o700 // PG's default datadir mode
		}
		if err := os.MkdirAll(full, mode); err != nil {
			return totalBytes, totalChunks,
				fmt.Errorf("chain materialise: mkdir %s: %w", d.Path, err)
		}
	}

	for i := range m.Files {
		if err := ctx.Err(); err != nil {
			return totalBytes, totalChunks, err
		}
		bw, ck, err := materializeFile(ctx, cas, dir, &m.Files[i])
		if err != nil {
			return totalBytes, totalChunks, err
		}
		totalBytes += bw
		totalChunks += ck
	}
	if m.BackupLabel != "" {
		if err := writeSpecial(dir, "backup_label", []byte(m.BackupLabel)); err != nil {
			return totalBytes, totalChunks, err
		}
	}
	if m.TablespaceMap != "" {
		if err := writeSpecial(dir, "tablespace_map", []byte(m.TablespaceMap)); err != nil {
			return totalBytes, totalChunks, err
		}
	}
	// pg_combinebackup hard-requires backup_manifest (PG's own JSON
	// shape) in every input dir — both for the full anchor and for
	// every incremental link. Manifests committed before+ do
	// not carry PGBackupManifest; the runner refuses to take a child
	// incremental without it, so by the time we're walking a chain
	// here we expect every link to carry one.
	if len(m.PGBackupManifest) == 0 {
		return totalBytes, totalChunks, output.NewError("chain.missing_pg_manifest",
			fmt.Sprintf("restore chain: link %q has no pg_backup_manifest field; pg_combinebackup will refuse it", m.BackupID)).
			WithSuggestion(&output.Suggestion{
				Human: "this backup pre-dates the+ PGBackupManifest field. Restoring it as a standalone backup still works; it cannot anchor or appear in an incremental chain. Take a fresh full backup to enable incrementals from this point forward.",
			})
	}
	if err := writeSpecial(dir, "backup_manifest", m.PGBackupManifest); err != nil {
		return totalBytes, totalChunks, err
	}
	return totalBytes, totalChunks, nil
}

// sumFileCount totals the FileEntry count across every chain link.
// Used as the "file_count" exposed to the operator: it represents
// the cumulative file work the chain materialised, not the merged
// output's file count (which we don't trivially know without
// re-walking the merged dir post-pg_combinebackup).
func sumFileCount(chain combine.Chain) int {
	var n int
	for _, m := range chain {
		n += len(m.Files)
	}
	return n
}

// chainStagingPath returns the directory the chain restore uses to
// materialise links.  When the operator pinned a custom path via
// Options.ChainStagingRoot we honour it verbatim; otherwise we
// derive a stable path from os.TempDir keyed on (deployment,
// leaf_backup_id) so every retry of the same restore lands on the
// same directory and its per-link completion markers.
//
// .
func chainStagingPath(opts Options) (string, error) {
	if opts.ChainStagingRoot != "" {
		// Operator-pinned path: use as-is.  We deliberately do NOT
		// suffix with deployment/backup_id when the operator
		// supplies an explicit path — they may want to point two
		// different restores at separate roots.
		return opts.ChainStagingRoot, nil
	}
	if opts.Deployment == "" || opts.BackupID == "" {
		return "", errors.New("chain staging: Deployment and BackupID required to derive default path")
	}
	// Filename-safe key: deployment IDs are well-formed; backup
	// IDs are timestamp-shaped (e.g. db1.full.20260428T0900Z) so
	// they're already filesystem-safe.  The path is derived (stable
	// across retries) so resume works — but that also makes it
	// predictable, so secureStagingDir enforces owner+mode before we
	// trust anything already at this path.  Nest under a per-uid
	// parent so a hostile user can't squat the exact path before us
	// (they'd have to own our uid-scoped parent, which they can't
	// create with our uid).
	dir := fmt.Sprintf("pg_hardstorage-restore-chain-%s-%s", opts.Deployment, opts.BackupID)
	parent := fmt.Sprintf("pg_hardstorage-restore-%d", os.Getuid())
	return filepath.Join(os.TempDir(), parent, dir), nil
}

// secureStagingDir makes stagingRoot exist as a private (0700)
// directory that the current user owns, without ever trusting a
// pre-existing directory created by someone else.
//
// The default staging path is predictable (see chainStagingPath), so
// on a shared host a hostile local user could pre-create it — a plain
// MkdirAll would happily adopt that dir, exposing the bytes we stage
// and letting the attacker plant forged completion markers.  We
// therefore:
//
//   - MkdirAll the PARENT with 0700 (the uid-scoped parent from
//     chainStagingPath; harmless for an operator-pinned path).
//   - Try to create the leaf with os.Mkdir + 0700 (Mkdir, unlike
//     MkdirAll, FAILS if the leaf already exists — so we notice a
//     squatted dir instead of silently adopting it).
//   - If the leaf already exists, lstat it and REFUSE unless it is a
//     real directory (not a symlink), owned by our uid, with mode
//     exactly 0700.  A dir that passes these checks is one WE created
//     on a previous attempt — legitimate resume — so we keep it.
func secureStagingDir(stagingRoot string) error {
	if parent := filepath.Dir(stagingRoot); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return output.NewError("internal",
				fmt.Sprintf("restore: mkdir staging parent %q: %v", parent, err)).Wrap(err)
		}
	}
	err := os.Mkdir(stagingRoot, 0o700)
	if err == nil {
		return nil
	}
	if !errors.Is(err, stdfs.ErrExist) {
		return output.NewError("internal",
			fmt.Sprintf("restore: mkdir staging %q: %v", stagingRoot, err)).Wrap(err)
	}
	// Pre-existing: verify it is ours and private before reusing it.
	info, lerr := os.Lstat(stagingRoot)
	if lerr != nil {
		return output.NewError("internal",
			fmt.Sprintf("restore: stat staging %q: %v", stagingRoot, lerr)).Wrap(lerr)
	}
	if info.Mode()&stdfs.ModeSymlink != 0 || !info.IsDir() {
		return output.NewError("restore.staging_insecure",
			fmt.Sprintf("restore: staging path %q exists and is not a plain directory (symlink or non-dir); refusing to stage into it", stagingRoot)).
			WithSuggestion(&output.Suggestion{
				Human: "remove the offending path or pass --chain-staging-root pointing at a directory only you can write, then retry (--reset-chain-staging forces a clean start)",
			})
	}
	if info.Mode().Perm() != 0o700 {
		return output.NewError("restore.staging_insecure",
			fmt.Sprintf("restore: staging dir %q has mode %#o, want 0700; refusing to reuse a world/group-accessible staging dir", stagingRoot, info.Mode().Perm())).
			WithSuggestion(&output.Suggestion{
				Human: "another process (possibly another user) may have created it; remove it or pass --chain-staging-root / --reset-chain-staging",
			})
	}
	if uid, foreign := stagingForeignOwner(info); foreign {
		return output.NewError("restore.staging_insecure",
			fmt.Sprintf("restore: staging dir %q is owned by uid %d, not the current uid %d; refusing to stage into another user's directory", stagingRoot, uid, os.Getuid())).
			WithSuggestion(&output.Suggestion{
				Human: "a different user created this staging dir; remove it or pass --chain-staging-root pointing at a directory only you own",
			})
	}
	return nil
}

// readChainLinkMarker attempts to load + parse a chain-link
// completion marker.  Returns (zero, false) on any failure
// (missing file, malformed JSON, schema mismatch) — the caller
// then re-materialises the link.  Best-effort by design: a stale
// or corrupt marker MUST NOT cause silent acceptance of a partial
// link.
func readChainLinkMarker(linkDir string) (chainLinkMarker, bool) {
	body, err := os.ReadFile(filepath.Join(linkDir, chainLinkCompleteFilename))
	if err != nil {
		return chainLinkMarker{}, false
	}
	var m chainLinkMarker
	if err := stdjson.Unmarshal(body, &m); err != nil {
		return chainLinkMarker{}, false
	}
	if m.Schema != chainLinkMarkerSchema {
		return chainLinkMarker{}, false
	}
	return m, true
}

// writeChainLinkMarker persists the completion marker for a
// successfully-materialised link.  fsync via fsutil.WriteFileSync
// so the marker survives a power loss between materialise and
// the parent's process exit.
func writeChainLinkMarker(linkDir string, m chainLinkMarker) error {
	body, err := stdjson.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileSync(filepath.Join(linkDir, chainLinkCompleteFilename), body, 0o600)
}

// totalChunkCountForManifest returns the count of chunk
// references across every file in the manifest.  Used as the
// freshness check for chain-link completion markers — a
// mismatched count means staging from a different chain (or a
// broken materialise) and forces re-materialise.
func totalChunkCountForManifest(m *backup.Manifest) int {
	if m == nil {
		return 0
	}
	var n int
	for i := range m.Files {
		n += len(m.Files[i].Chunks)
	}
	return n
}

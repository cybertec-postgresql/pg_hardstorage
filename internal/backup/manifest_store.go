// manifest_store.go — primary+replica Commit/Read/List for signed
// manifests on top of a StoragePlugin, with tombstone handling.

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"iter"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// ManifestStore writes and reads signed manifests against any StoragePlugin.
//
// Two on-repo locations per manifest:
//
//	manifests/<deployment>/backups/<id>/manifest.json   (primary)
//	manifests/_replicas/<id>.manifest.json              (redundancy)
//
// The primary is the authoritative existence indicator: a manifest
// is considered committed iff the primary key is present. The replica
// is best-effort redundancy — if the primary's prefix gets corrupted
// (a single misdirected `aws s3 rm`, say), the replica still has the
// bytes. We commit primary first; replica is written after, with a
// failure logged but not propagated upstream.
//
// Atomicity: each commit writes to a `<key>.tmp` then RenameIfNotExists
// to the final key, so a partial write never produces a visible
// half-manifest.
type ManifestStore struct {
	sp storage.StoragePlugin
}

// NewManifestStore wraps sp. The caller retains ownership of sp.
func NewManifestStore(sp storage.StoragePlugin) *ManifestStore {
	if sp == nil {
		panic("backup: NewManifestStore requires a non-nil StoragePlugin")
	}
	return &ManifestStore{sp: sp}
}

// validateStorageID rejects a deployment / backup ID that would break out
// of, or splinter, the storage-key hierarchy it is interpolated into
// (manifests/<dep>/backups/<id>/...). It mirrors the backup-runner's
// create-path rule so the READ / DELETE / RESTORE paths — which used to
// trust the API and CLI inputs verbatim (input-validation audit #1) —
// reject the same injection surface. Deliberately narrow: only path
// separators, control characters, and the reserved "."/".." components,
// so it never newly rejects an otherwise-legal id that contains dots
// (backup IDs are "<dep>.<type>.<ts>.<hex>").
func validateStorageID(kind, s string) error {
	if s == "" {
		return fmt.Errorf("backup: %s is required", kind)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("backup: %s %q is a reserved path component", kind, s)
	}
	if i := strings.IndexFunc(s, func(r rune) bool {
		return r == '/' || r == '\\' || r < 0x20 || r == 0x7f
	}); i >= 0 {
		return fmt.Errorf("backup: %s %q contains an illegal character (path separators and control characters are not allowed)", kind, s)
	}
	return nil
}

// ValidateDeployment / ValidateBackupID expose the storage-safety check so
// the API and CLI boundaries can reject a bad identifier up front (and
// return a clean 4xx) rather than relying solely on the store chokepoint.
func ValidateDeployment(deployment string) error { return validateStorageID("deployment", deployment) }
func ValidateBackupID(backupID string) error     { return validateStorageID("backup ID", backupID) }

// validateRef checks both halves of a (deployment, backupID) reference.
func validateRef(deployment, backupID string) error {
	if err := validateStorageID("deployment", deployment); err != nil {
		return err
	}
	return validateStorageID("backup ID", backupID)
}

// MaxManifestBytes caps how many bytes any single manifest/tombstone read
// pulls into memory. A manifest that declares a huge files/chunks array,
// or a corrupt/oversized object, would otherwise OOM the reader via an
// unbounded io.ReadAll before Validate ever runs (input-validation audit
// #2). 1 GiB is far above any real manifest yet bounds the blast radius.
const MaxManifestBytes = 1 << 30

// readAllLimited reads up to max bytes from rc, returning an error rather
// than allocating unboundedly when the source exceeds it.
func readAllLimited(rc stdio.Reader, max int64) ([]byte, error) {
	body, err := stdio.ReadAll(stdio.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("backup: manifest exceeds the %d-byte limit (refusing to load an oversized or malformed manifest)", max)
	}
	return body, nil
}

// PrimaryPath returns the key of the primary manifest for the given
// deployment + backup ID.
func PrimaryPath(deployment, backupID string) string {
	return "manifests/" + deployment + "/backups/" + backupID + "/manifest.json"
}

// defaultReplicaErrorLoggerMu guards the package-level
// defaultReplicaErrorLogger so SetDefaultReplicaErrorLogger callers
// (tests, agent startup) don't race with the Commit goroutine.
var defaultReplicaErrorLoggerMu sync.RWMutex

// defaultReplicaErrorLogger is invoked when CommitOptions.OnReplicaError
// is nil and the replica write fails.  Without this, the v23 audit
// flagged that replica failures were silently swallowed (#4): the
// operator could lose cross-prefix corruption survivability without
// any warning.  Default writes a single line to log.Default(); tests
// override via SetDefaultReplicaErrorLogger.
var defaultReplicaErrorLogger = func(err error) {
	log.Printf("backup.manifest_store: replica commit failed (cross-prefix redundancy degraded): %v", err)
}

// SetDefaultReplicaErrorLogger replaces the package-level fallback
// invoked when CommitOptions.OnReplicaError is nil.  Pass nil to
// disable the fallback (for callers who genuinely want silent
// behaviour and accept the audit consequence).  Returns the prior
// logger so callers can restore it.
func SetDefaultReplicaErrorLogger(fn func(error)) func(error) {
	defaultReplicaErrorLoggerMu.Lock()
	defer defaultReplicaErrorLoggerMu.Unlock()
	prior := defaultReplicaErrorLogger
	defaultReplicaErrorLogger = fn
	return prior
}

func currentReplicaErrorLogger() func(error) {
	defaultReplicaErrorLoggerMu.RLock()
	defer defaultReplicaErrorLoggerMu.RUnlock()
	return defaultReplicaErrorLogger
}

// ReplicaPath returns the key of the redundant manifest copy.
func ReplicaPath(backupID string) string {
	return "manifests/_replicas/" + backupID + ".manifest.json"
}

// CommitOptions tunes Commit's behavior. The zero value is fine for
// production; tests use it to substitute alternatives.
type CommitOptions struct {
	// SkipReplica disables the replica copy. Only used by tests that
	// want to verify primary-only behavior.
	SkipReplica bool

	// OnReplicaError, when non-nil, is invoked if the redundant replica
	// write fails after the primary has already committed. The primary
	// is authoritative — replica failure is non-fatal — but the caller
	// may want to surface a warning event so the operator knows the
	// redundancy guarantee is degraded for this manifest. The callback
	// runs synchronously on the Commit goroutine and must return
	// promptly; it is NOT called when the replica write succeeds.
	OnReplicaError func(error)

	// RetainUntil propagates a WORM retention deadline to the
	// underlying storage Put on commit. Zero disables. When set,
	// both the primary and replica copies pick up the same
	// deadline. Backends without WORM ignore this.
	RetainUntil time.Time

	// RetentionMode selects the WORM lock posture (Compliance or
	// Governance) when RetainUntil is set. Empty implies
	// Compliance (regulatory-grade default).
	RetentionMode storage.WORMMode
}

// Commit signs m (if not already signed), writes it atomically to the
// primary and replica paths, and returns nil on success.
//
// Race semantics: ErrAlreadyCommitted is returned if the primary key
// already exists. Concurrent Commit() calls for the same backup_id
// resolve to exactly one winner; the others see ErrAlreadyCommitted.
//
// Replica failures do NOT cause Commit to fail once the primary is in
// place — the manifest is committed. They are surfaced via
// CommitOptions.OnReplicaError so the caller can emit a warning event;
// without that callback the failure is swallowed (the manifest is
// still readable, but cross-prefix corruption survivability for this
// backup is degraded until the next successful commit of the same body).
func (ms *ManifestStore) Commit(ctx context.Context, m *Manifest, signer *Signer, opts CommitOptions) error {
	if m == nil {
		return errors.New("backup: Commit nil manifest")
	}
	if m.Deployment == "" || m.BackupID == "" {
		return errors.New("backup: Commit requires Manifest.Deployment and Manifest.BackupID")
	}
	if err := validateRef(m.Deployment, m.BackupID); err != nil {
		return err
	}
	// Manifest-invariants gate (issue #91).  Every code path that
	// commits a manifest goes through here; running Validate() at
	// this single chokepoint guarantees the repo can never contain
	// a manifest that fails its own invariant check.  Without this,
	// a manifest with (e.g.) an empty BackupLabel would commit
	// cleanly, pass `verify` (which only re-hashes chunks), and
	// fail at `verify --full` / restore with a structured but
	// late `manifest.invalid` error — exactly the bug shape the
	// reporter hit.
	if err := m.Validate(); err != nil {
		return fmt.Errorf("backup: Commit refuses invalid manifest %s: %w", m.BackupID, err)
	}
	if m.Attestation == nil {
		if signer == nil {
			return errors.New("backup: Commit needs a Signer when manifest is not pre-signed")
		}
		if err := m.Sign(signer); err != nil {
			return fmt.Errorf("backup: sign manifest %s: %w", m.BackupID, err)
		}
	}

	body, err := m.MarshalToBytes()
	if err != nil {
		return fmt.Errorf("backup: marshal manifest %s: %w", m.BackupID, err)
	}

	primaryKey := PrimaryPath(m.Deployment, m.BackupID)
	if err := ms.commitAtomic(ctx, primaryKey, body, opts); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return ErrAlreadyCommitted
		}
		return fmt.Errorf("backup: commit primary %q: %w", primaryKey, err)
	}

	// Chain-safety (write-then-verify). An incremental whose parent is
	// soft-deleted CONCURRENTLY with this commit would be an orphan:
	// un-restorable, because the parent's chunks are slated for GC once
	// its tombstone ages out. Now that our manifest is visible at its
	// primary key, re-check the parent — if it's been tombstoned (or
	// vanished), roll THIS commit back and refuse. Paired with
	// SoftDelete's post-tombstone re-scan, this closes the
	// delete-vs-incremental-backup race from both sides: the deleter
	// sees a child that committed first; a child committing second sees
	// the tombstone here.
	if m.Type == BackupTypeIncremental && m.ParentBackupID != "" {
		live, perr := ms.backupIsLive(ctx, m.Deployment, m.ParentBackupID)
		if perr != nil {
			return ms.rollbackPrimaryCommit(ctx, primaryKey,
				fmt.Errorf("backup: Commit: verify parent %s of incremental %s is live: %w",
					m.ParentBackupID, m.BackupID, perr))
		}
		if !live {
			return ms.rollbackPrimaryCommit(ctx, primaryKey, &OrphanedIncrementalError{
				Deployment:     m.Deployment,
				BackupID:       m.BackupID,
				ParentBackupID: m.ParentBackupID,
			})
		}
	}

	// Record the deployment in the index so Deployments() can enumerate
	// the fleet without scanning every manifest object. Best-effort and
	// idempotent; a genuine write failure self-heals by invalidating the
	// index sentinel so the next Deployments() rebuilds from a full scan.
	ms.indexDeployment(ctx, m.Deployment)

	if opts.SkipReplica {
		return nil
	}

	replicaKey := ReplicaPath(m.BackupID)
	// Replica failure is non-fatal. The primary is authoritative; the
	// replica is redundancy. We surface the error via the caller's
	// callback so a `warning` event can be emitted upstream, but we
	// always return nil for the commit — a missing replica must not
	// cause a successful primary commit to look like a failure.
	//
	// when OnReplicaError is nil, the failure used to
	// be completely swallowed.  We now fall back to the package-
	// level defaultReplicaErrorLogger (writes to log.Default by
	// default) so an operator running with the default config still
	// sees the warning.  Callers who genuinely want silent
	// behaviour can SetDefaultReplicaErrorLogger(nil).
	if rerr := ms.commitReplica(ctx, replicaKey, body, opts); rerr != nil {
		wrapped := fmt.Errorf("replica %q: %w", replicaKey, rerr)
		switch {
		case opts.OnReplicaError != nil:
			opts.OnReplicaError(wrapped)
		default:
			if logger := currentReplicaErrorLogger(); logger != nil {
				logger(wrapped)
			}
		}
	}
	return nil
}

// rollbackPrimaryCommit removes the just-written primary manifest after
// a post-write chain check failed, returning cause. If the rollback
// Delete ITSELF fails, the manifest is left orphaned at its primary key
// — rather than hide that (the old code dropped the Delete error), return
// a combined error naming the stray key so an operator can remove it
// before retrying (poor-error-handling audit #2). cause is wrapped with
// %w so callers' errors.Is/As against it (e.g. *OrphanedIncrementalError)
// keep working on either path.
func (ms *ManifestStore) rollbackPrimaryCommit(ctx context.Context, primaryKey string, cause error) error {
	if delErr := ms.sp.Delete(ctx, primaryKey); delErr != nil {
		return fmt.Errorf("%w; ALSO failed to roll back the just-written manifest at %q — a stray, orphaned manifest remains and must be removed before retrying: %v",
			cause, primaryKey, delErr)
	}
	return cause
}

// commitAtomic writes body to <key>.tmp and atomically renames it to
// key. The intermediate name is unique-per-write so concurrent Commit
// calls don't collide on the tmp slot.
//
// WORM retention is applied to the COMMITTED object (after the rename),
// never to the staging tmp.  Earlier revisions locked the tmp and
// relied on the rename to carry the lock forward — which was wrong on
// every WORM backend:
//
//   - s3 Put applies the Object Lock to the tmp, but RenameIfNotExists
//     is copy+delete and its source-delete CANNOT remove a
//     Compliance-locked tmp, so the commit failed after the copy had
//     already landed (broken half-committed state).  The copy itself
//     also doesn't carry the source's lock to the destination, so the
//     committed manifest wasn't reliably locked anyway.
//   - gcs/azblob Put ignore RetainUntil entirely, so the committed
//     manifest got NO retention from this path at all.
//
// Applying SetRetention to the committed key fixes all three: the
// requested deadline lands on the object that survives, and the tmp
// stays an ordinary, deletable staging object (reaped by the rename's
// source-delete on success, or by `repo gc`'s stale-staging sweep if a
// crash orphaned it).
func (ms *ManifestStore) commitAtomic(ctx context.Context, key string, body []byte, opts CommitOptions) error {
	tmp := key + ".tmp." + randSuffix()
	_, err := ms.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
	if err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := ms.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		// Best-effort cleanup of the tmp; never propagate a tmp-cleanup
		// failure (the primary error is what matters).
		_ = ms.sp.Delete(ctx, tmp)
		return err
	}
	// Apply WORM retention to the committed manifest itself.  A backend
	// without WORM returns ErrUnsupported, which is expected and
	// ignored ("backends without WORM ignore this"); any other failure
	// means the operator-requested lock did NOT apply, which must
	// surface.
	if !opts.RetainUntil.IsZero() {
		mode := opts.RetentionMode
		if mode == "" {
			mode = storage.WORMCompliance
		}
		if err := ms.sp.SetRetention(ctx, key, opts.RetainUntil, mode); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			return fmt.Errorf("apply retention to %s: %w", key, err)
		}
	}
	return nil
}

// replicaCommitAttempts / replicaCommitBackoff bound how hard Commit
// retries a failed replica write. The replica is the redundancy copy that
// survives loss/corruption of the primary; a single transient blip (a
// rate-limit, a brief network error) right after a successful primary
// write must not strand it permanently — nothing self-heals a missing
// replica afterward (KMS rotation and `repo replicate` only touch an
// EXISTING one; there is no rebuild path), so the redundancy would be
// silently gone for that backup until the primary fails and the operator
// discovers it. Retry like persistGap/gapstate do for their write-once
// records, but stay non-fatal: after the budget is exhausted the primary
// commit still succeeds and the failure is logged.
const (
	replicaCommitAttempts = 3
	replicaCommitBackoff  = 200 * time.Millisecond
)

// commitReplica writes the redundancy copy with a bounded retry. An
// already-present replica (ErrAlreadyExists — a concurrent commit, or a
// prior attempt whose rename actually landed) counts as success. Returns
// the last error if every attempt failed; the caller keeps it non-fatal.
func (ms *ManifestStore) commitReplica(ctx context.Context, replicaKey string, body []byte, opts CommitOptions) error {
	var lastErr error
	for attempt := 0; attempt < replicaCommitAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(replicaCommitBackoff * time.Duration(attempt)):
			}
		}
		err := ms.commitAtomic(ctx, replicaKey, body, opts)
		if err == nil || errors.Is(err, storage.ErrAlreadyExists) {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// ReadAttestationless is the no-verify variant of Read: it loads
// the primary (or replica fallback) manifest and parses via
// ParseAttestationless instead of ParseAndVerify.  Same trust
// posture as ListAttestationless: read-only inspection paths only.
func (ms *ManifestStore) ReadAttestationless(ctx context.Context, deployment, backupID string) (*Manifest, error) {
	if dead, err := ms.IsTombstoned(ctx, deployment, backupID); err != nil {
		return nil, fmt.Errorf("backup: tombstone check %s: %w", backupID, err)
	} else if dead {
		return nil, ErrTombstoned
	}
	primaryKey := PrimaryPath(deployment, backupID)
	rc, gerr := ms.sp.Get(ctx, primaryKey)
	if gerr == nil {
		body, rerr := readAllLimited(rc, MaxManifestBytes)
		_ = rc.Close()
		if rerr == nil {
			if m, perr := ParseAttestationless(body); perr == nil {
				return m, nil
			}
		}
	}
	// Replica fallback.
	replicaKey := ReplicaPath(backupID)
	rc2, gerr2 := ms.sp.Get(ctx, replicaKey)
	if gerr2 != nil {
		return nil, gerr2
	}
	defer rc2.Close()
	body2, rerr2 := readAllLimited(rc2, MaxManifestBytes)
	if rerr2 != nil {
		return nil, rerr2
	}
	return ParseAttestationless(body2)
}

// Read fetches the primary manifest for deployment + backupID,
// verifies the signature against verifier, and returns the parsed
// value.
//
// Tombstoned manifests are NOT returned — a soft-deleted manifest
// is treated as not-present, so callers see a clean ErrTombstoned
// rather than silently consuming a backup that retention has
// scheduled for deletion.
//
// On any verification failure, it falls through to the replica path
// before returning the error. That gives us survivability against a
// single corrupted primary.
func (ms *ManifestStore) Read(ctx context.Context, deployment, backupID string, verifier *Verifier) (*Manifest, error) {
	if err := validateRef(deployment, backupID); err != nil {
		return nil, err
	}
	if dead, err := ms.IsTombstoned(ctx, deployment, backupID); err != nil {
		return nil, fmt.Errorf("backup: tombstone check %s: %w", backupID, err)
	} else if dead {
		return nil, ErrTombstoned
	}
	primaryKey := PrimaryPath(deployment, backupID)
	m, primaryErr := ms.readVerified(ctx, primaryKey, verifier)
	if primaryErr == nil {
		return m, nil
	}

	// Try the replica only when the primary is not visible OR fails
	// signature verification. Other errors (a parse failure on the
	// primary) probably mean the replica has the same content too, but
	// we try anyway — failure to load primary is rare.
	replicaKey := ReplicaPath(backupID)
	replicaM, replicaErr := ms.readVerified(ctx, replicaKey, verifier)
	if replicaErr == nil {
		// Replicas are keyed by backupID alone (ReplicaPath ignores
		// deployment), so a replica for the same backupID under a
		// DIFFERENT deployment would otherwise be served here — bypassing
		// this deployment's tombstone gate (checked above against the
		// requested deployment). Confirm identity before trusting it,
		// mirroring EnsureReplica's primary-identity guard.
		if replicaM.Deployment == deployment && replicaM.BackupID == backupID {
			return replicaM, nil
		}
		replicaErr = fmt.Errorf("backup: replica identity %s/%s != requested %s/%s",
			replicaM.Deployment, replicaM.BackupID, deployment, backupID)
	}

	// Common case: the backup just doesn't exist. Both primary and
	// replica returned ErrNotFound. Return ErrNotFound directly so
	// callers can errors.Is the result cleanly without wading
	// through a forensic two-path message that says nothing more
	// than "yep, it's not there."
	if errors.Is(primaryErr, storage.ErrNotFound) && errors.Is(replicaErr, storage.ErrNotFound) {
		return nil, fmt.Errorf("backup: manifest %s/%s: %w",
			deployment, backupID, storage.ErrNotFound)
	}

	// Mixed-failure case (corruption, signature mismatch, etc.): keep
	// both errors visible. errors.Is(err, primaryErr's chain) still
	// works via the %w on primaryErr.
	return nil, fmt.Errorf("backup: read %s: primary failed (%w) and replica failed (%v)",
		backupID, primaryErr, replicaErr)
}

// ReadIncludingTombstoned is the variant of Read that surfaces
// tombstoned manifests instead of refusing them. Returns the
// manifest body alongside a Tombstoned bool indicating whether
// the marker is present. Used by `backup show --include-deleted`
// so an operator considering an undelete can inspect the full
// manifest body (file count, sizes, signatures) before committing
// to the resurrection.
//
// Same primary→replica fallback as Read; the only difference is
// the tombstone check no longer short-circuits to ErrTombstoned.
// All real errors (signature failure, both copies missing,
// backend error) propagate identically.
//
// Live manifests round-trip through this function exactly like
// they do through Read — Tombstoned is false, the manifest body
// is the same. So callers that don't care about the live/dead
// distinction can use ReadIncludingTombstoned uniformly.
func (ms *ManifestStore) ReadIncludingTombstoned(ctx context.Context, deployment, backupID string, verifier *Verifier) (*Manifest, bool, error) {
	if deployment == "" || backupID == "" {
		return nil, false, errors.New("backup: ReadIncludingTombstoned requires deployment and backupID")
	}
	dead, err := ms.IsTombstoned(ctx, deployment, backupID)
	if err != nil {
		return nil, false, fmt.Errorf("backup: tombstone check %s: %w", backupID, err)
	}
	primaryKey := PrimaryPath(deployment, backupID)
	m, primaryErr := ms.readVerified(ctx, primaryKey, verifier)
	if primaryErr == nil {
		return m, dead, nil
	}
	replicaKey := ReplicaPath(backupID)
	replicaM, replicaErr := ms.readVerified(ctx, replicaKey, verifier)
	if replicaErr == nil {
		return replicaM, dead, nil
	}
	if errors.Is(primaryErr, storage.ErrNotFound) && errors.Is(replicaErr, storage.ErrNotFound) {
		return nil, false, fmt.Errorf("backup: manifest %s/%s: %w",
			deployment, backupID, storage.ErrNotFound)
	}
	return nil, false, fmt.Errorf("backup: read %s: primary failed (%w) and replica failed (%v)",
		backupID, primaryErr, replicaErr)
}

func (ms *ManifestStore) readVerified(ctx context.Context, key string, verifier *Verifier) (*Manifest, error) {
	rc, err := ms.sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := readAllLimited(rc, MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return ParseAndVerify(body, verifier)
}

// Deployments enumerates the deployment names visible in the repo by
// listing the `manifests/<deployment>/` prefixes. The returned slice
// is sorted; duplicate names are coalesced.
//
// Cost: O(N) over every primary-manifest object in the repo. For
// small fleets (dozens of deployments, hundreds of backups each) this
// is fast; for very large fleets a top-level index lands in a later
// slice. Today's status command is the only caller — operators with
// thousands of deployments will hit this before the index, and the
// fix is mechanical when they do.
// Deployment index.
//
// Deployments() needs the set of deployment names, but a naive walk of
// the manifests/ prefix is O(total manifest objects across the whole
// fleet) — millions of LIST entries to recover a few thousand names at
// scale. Instead we keep one tiny marker per deployment under
// deploymentIndexPrefix; Deployments() enumerates those in O(num
// deployments). A marker has the same lifecycle as the deployment's
// manifests: it's written at Commit and never removed (manifests are
// soft-deleted, never hard-deleted, so a deployment that ever had a
// backup stays listed — matching the pre-index behavior exactly).
//
// deploymentIndexSentinel marks the index as fully backfilled (i.e.
// authoritative). Until it exists — an upgraded repo that predates the
// index — Deployments() falls back to the full manifests/ scan and
// then best-effort backfills the index so subsequent calls are fast.
const (
	deploymentIndexPrefix   = "deployments/names/"
	deploymentIndexSentinel = "deployments/_initialized"
	deploymentIndexSchema   = "pg_hardstorage.deployment.index.v1"
)

type deploymentMarker struct {
	Schema     string `json:"schema"`
	Deployment string `json:"deployment,omitempty"`
}

func deploymentMarkerKey(name string) string { return deploymentIndexPrefix + name }

// Deployments returns every deployment that has at least one backup,
// sorted. Fast path (index authoritative): O(num deployments). Slow
// path (index not yet built): a full manifests/ scan that also
// backfills the index.
func (ms *ManifestStore) Deployments(ctx context.Context) ([]string, error) {
	if ms.deploymentIndexReady(ctx) {
		if names, err := ms.listDeploymentIndex(ctx); err == nil {
			return names, nil
		}
		// Index read failed — fall through to the authoritative scan.
	}
	names, err := ms.scanDeployments(ctx)
	if err != nil {
		return nil, err
	}
	ms.backfillDeploymentIndex(ctx, names) // best-effort
	return names, nil
}

// scanDeployments is the authoritative (but O(N)) walk of the
// manifests/ prefix, extracting the distinct deployment names.
func (ms *ManifestStore) scanDeployments(ctx context.Context) ([]string, error) {
	const prefix = "manifests/"
	seen := map[string]struct{}{}
	for info, err := range ms.sp.List(ctx, prefix) {
		if err != nil {
			return nil, fmt.Errorf("backup: list deployments: %w", err)
		}
		// info.Key is repo-relative ("manifests/db1/backups/.../manifest.json"
		// or "manifests/_replicas/...manifest.json"). We exclude the
		// "_replicas" pseudo-deployment (it's the redundancy slot, not a real
		// deployment).
		rel := strings.TrimPrefix(info.Key, prefix)
		// Skip if Key didn't start with the prefix (defensive — should
		// never happen given List's contract).
		if rel == info.Key {
			continue
		}
		i := strings.IndexByte(rel, '/')
		if i <= 0 {
			continue
		}
		name := rel[:i]
		if name == "_replicas" {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// deploymentIndexReady reports whether the index sentinel is present.
func (ms *ManifestStore) deploymentIndexReady(ctx context.Context) bool {
	_, err := ms.sp.Stat(ctx, deploymentIndexSentinel)
	return err == nil
}

// listDeploymentIndex enumerates deployment names from the index
// markers (the fast path). Sorted.
func (ms *ManifestStore) listDeploymentIndex(ctx context.Context) ([]string, error) {
	var out []string
	for info, err := range ms.sp.List(ctx, deploymentIndexPrefix) {
		if err != nil {
			return nil, fmt.Errorf("backup: list deployment index: %w", err)
		}
		name := strings.TrimPrefix(info.Key, deploymentIndexPrefix)
		// Only direct children are deployment markers; defend against
		// any nested key the backend might surface.
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// indexDeployment records a deployment in the index (Commit path).
// Best-effort + idempotent. On a genuine write failure it invalidates
// the sentinel so the next Deployments() rebuilds from a full scan,
// rather than leaving this deployment invisible to the fast path.
func (ms *ManifestStore) indexDeployment(ctx context.Context, name string) {
	if err := ms.putDeploymentMarker(ctx, name); err != nil {
		_ = ms.sp.Delete(ctx, deploymentIndexSentinel)
	}
}

// putDeploymentMarker writes one deployment marker with IfNotExists.
// An already-present marker is success (idempotent). Returns a non-nil
// error only on a genuine write failure. No sentinel side effects —
// the caller decides how to react.
func (ms *ManifestStore) putDeploymentMarker(ctx context.Context, name string) error {
	if name == "" || name == "_replicas" {
		return nil
	}
	body, err := json.Marshal(deploymentMarker{Schema: deploymentIndexSchema, Deployment: name})
	if err != nil {
		return err
	}
	_, err = ms.sp.Put(ctx, deploymentMarkerKey(name), bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	})
	if err == nil || errors.Is(err, storage.ErrAlreadyExists) {
		return nil
	}
	return err
}

// backfillDeploymentIndex writes a marker for every name and, only if
// they ALL succeed, the sentinel that marks the index authoritative.
// Best-effort: on a write failure (e.g. a read-only repo) it leaves the
// sentinel absent so Deployments() keeps using the authoritative scan.
func (ms *ManifestStore) backfillDeploymentIndex(ctx context.Context, names []string) {
	for _, n := range names {
		if err := ms.putDeploymentMarker(ctx, n); err != nil {
			return // don't claim authoritative; next call retries
		}
	}
	body, err := json.Marshal(deploymentMarker{Schema: deploymentIndexSchema})
	if err != nil {
		return
	}
	_, _ = ms.sp.Put(ctx, deploymentIndexSentinel, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
}

// List streams primary-manifest keys for deployment, returning each
// loaded-and-verified Manifest. Tombstoned manifests (those soft-
// deleted by retention) are silently filtered out — see TombstonePath
// for the marker-file convention. Failures on individual entries are
// surfaced as (zero, err) iterations so the caller can decide whether
// to keep going.
//
// Implementation: a single List walk classifies every key as either
// a manifest, a tombstone marker, or unrelated. We collect tombstoned
// IDs first, then yield only manifests not in that set. Cost is O(N)
// memory in the number of manifests for the deployment — acceptable
// for the v0.1 fleet size; an indexed lookup lands later.

// ListAttestationless is the no-verify variant of List: it parses
// manifests via ParseAttestationless and yields them without
// requiring the local keyring's signing key.  Callers that only
// need the manifest body for scope / inspection (kms shred dry-run,
// partial inspect, scrub) use this to avoid provisioning a verifier
// just for read-only enumeration.
//
// Trust posture: this MUST NOT be used by paths that act on the
// manifest's claims (rotate, repair attestation, restore).  Those
// must use List + a real verifier so a forged manifest signed under
// the wrong key is rejected.
func (ms *ManifestStore) ListAttestationless(ctx context.Context, deployment string) iter.Seq2[*Manifest, error] {
	return func(yield func(*Manifest, error) bool) {
		if err := validateStorageID("deployment", deployment); err != nil {
			yield(nil, err)
			return
		}
		prefix := "manifests/" + deployment + "/backups/"
		const manifestSuffix = "/manifest.json"
		const tombstoneSuffix = "/manifest.json.tombstone"

		var manifestKeys []string
		tombstoned := map[string]struct{}{}
		for info, err := range ms.sp.List(ctx, prefix) {
			if err != nil {
				yield(nil, err)
				return
			}
			switch {
			case strings.HasSuffix(info.Key, tombstoneSuffix):
				rel := strings.TrimPrefix(info.Key, prefix)
				slash := strings.IndexByte(rel, '/')
				if slash > 0 {
					tombstoned[rel[:slash]] = struct{}{}
				}
			case strings.HasSuffix(info.Key, manifestSuffix):
				manifestKeys = append(manifestKeys, info.Key)
			}
		}
		sort.Strings(manifestKeys)
		for _, key := range manifestKeys {
			rel := strings.TrimPrefix(key, prefix)
			slash := strings.IndexByte(rel, '/')
			if slash > 0 {
				if _, dead := tombstoned[rel[:slash]]; dead {
					continue
				}
			}
			rc, err := ms.sp.Get(ctx, key)
			if err != nil {
				if !yield(nil, err) {
					return
				}
				continue
			}
			body, rerr := readAllLimited(rc, MaxManifestBytes)
			_ = rc.Close()
			if rerr != nil {
				if !yield(nil, rerr) {
					return
				}
				continue
			}
			m, perr := ParseAttestationless(body)
			if !yield(m, perr) {
				return
			}
		}
	}
}

// List streams every committed manifest for deployment in
// chronological order (lexicographic-on-backup-ID, which is
// timestamp-prefixed). Each manifest is signature-verified against
// verifier before being yielded; verification failures surface as
// the error half of the iter.Seq2 pair and the iterator continues
// to the next key.
//
// Tombstoned manifests are skipped silently. The walk reads from the
// primary location only — replica fallback is per-key in Read, not
// at list time.
func (ms *ManifestStore) List(ctx context.Context, deployment string, verifier *Verifier) iter.Seq2[*Manifest, error] {
	return func(yield func(*Manifest, error) bool) {
		prefix := "manifests/" + deployment + "/backups/"
		const manifestSuffix = "/manifest.json"
		const tombstoneSuffix = "/manifest.json.tombstone"

		var manifestKeys []string
		tombstoned := map[string]struct{}{}
		for info, err := range ms.sp.List(ctx, prefix) {
			if err != nil {
				yield(nil, err)
				return
			}
			switch {
			case strings.HasSuffix(info.Key, tombstoneSuffix):
				// Marker — extract the backup ID. Layout:
				// manifests/<dep>/backups/<id>/manifest.json.tombstone
				rel := strings.TrimPrefix(info.Key, prefix)
				slash := strings.IndexByte(rel, '/')
				if slash > 0 {
					tombstoned[rel[:slash]] = struct{}{}
				}
			case strings.HasSuffix(info.Key, manifestSuffix):
				manifestKeys = append(manifestKeys, info.Key)
			}
		}
		// Sort by full key so the yield order is deterministic
		// regardless of the backend's List ordering. Backup IDs
		// embed a UTC timestamp ("db1.full.20260428T1200Z") so
		// lexicographic sort = chronological order — which is what
		// every reasonable consumer (status, list CLI, retention)
		// expects.
		sort.Strings(manifestKeys)

		for _, key := range manifestKeys {
			rel := strings.TrimPrefix(key, prefix)
			slash := strings.IndexByte(rel, '/')
			if slash > 0 {
				if _, dead := tombstoned[rel[:slash]]; dead {
					continue
				}
			}
			m, err := ms.readVerified(ctx, key, verifier)
			if !yield(m, err) {
				return
			}
		}
	}
}

// ManifestEntry pairs a Manifest with its tombstoned/live status.
// Used by ListIncludingTombstoned so callers (the `backup list
// --include-deleted` CLI, audit/forensics tooling) can surface
// soft-deleted manifests alongside live ones without losing the
// distinction.
type ManifestEntry struct {
	Manifest   *Manifest
	Tombstoned bool
}

// ListIncludingTombstoned is the variant of List that surfaces
// tombstoned manifests too, paired with their live/dead status.
// Same prefix walk, same sort, but no filtering on the tombstone
// set — callers see every manifest in the deployment with a
// Tombstoned bool indicating whether the marker is in place.
//
// Used by `backup list --include-deleted` so an operator who
// needs to undelete can see exactly which manifests are still
// recoverable. Without this, the only way to discover tombstoned
// IDs was to grep the audit log — workable but unfriendly.
//
// Failures on individual entries surface as (nil, err) iterations
// so the caller can decide whether to keep going. The verifier
// argument is forwarded to the same readVerified path List uses;
// a signature-broken manifest's err is propagated, the
// Tombstoned-bool isn't reported (the caller has nothing to
// show anyway).
func (ms *ManifestStore) ListIncludingTombstoned(ctx context.Context, deployment string, verifier *Verifier) iter.Seq2[ManifestEntry, error] {
	return func(yield func(ManifestEntry, error) bool) {
		prefix := "manifests/" + deployment + "/backups/"
		const manifestSuffix = "/manifest.json"
		const tombstoneSuffix = "/manifest.json.tombstone"

		var manifestKeys []string
		tombstoned := map[string]struct{}{}
		for info, err := range ms.sp.List(ctx, prefix) {
			if err != nil {
				yield(ManifestEntry{}, err)
				return
			}
			switch {
			case strings.HasSuffix(info.Key, tombstoneSuffix):
				rel := strings.TrimPrefix(info.Key, prefix)
				slash := strings.IndexByte(rel, '/')
				if slash > 0 {
					tombstoned[rel[:slash]] = struct{}{}
				}
			case strings.HasSuffix(info.Key, manifestSuffix):
				manifestKeys = append(manifestKeys, info.Key)
			}
		}
		sort.Strings(manifestKeys)

		for _, key := range manifestKeys {
			rel := strings.TrimPrefix(key, prefix)
			slash := strings.IndexByte(rel, '/')
			id := ""
			if slash > 0 {
				id = rel[:slash]
			}
			_, dead := tombstoned[id]
			m, err := ms.readVerified(ctx, key, verifier)
			if err != nil {
				if !yield(ManifestEntry{}, err) {
					return
				}
				continue
			}
			if !yield(ManifestEntry{Manifest: m, Tombstoned: dead}, nil) {
				return
			}
		}
	}
}

// TombstonePath returns the marker key for a soft-deleted manifest.
// The marker sits next to the primary manifest at
// manifests/<dep>/backups/<id>/manifest.json.tombstone — same prefix,
// just a sibling file. List filters it out; Read refuses to return
// the manifest while the tombstone exists.
//
// Choice of layout: keeping the marker beside the manifest (rather
// than moving the manifest to a `_trash/` subprefix) avoids cross-
// prefix renames and lets a future "undelete" be a single Delete of
// the marker — no path bookkeeping. The same simplicity applies to
// the chunk-GC pass: it only needs to walk visible manifests.
func TombstonePath(deployment, backupID string) string {
	return PrimaryPath(deployment, backupID) + ".tombstone"
}

// Tombstone is the body of a marker file. Records who deleted the
// manifest and why so the audit log has a complete trail.
type Tombstone struct {
	Schema       string    `json:"schema"`
	BackupID     string    `json:"backup_id"`
	Deployment   string    `json:"deployment"`
	TombstonedAt time.Time `json:"tombstoned_at"`
	Policy       string    `json:"policy"`           // e.g. "gfs", "simple", "manual"
	Reason       string    `json:"reason,omitempty"` // human-readable note
}

// TombstoneSchema is the on-disk version tag for Tombstone bodies.
const TombstoneSchema = "pg_hardstorage.tombstone.v1"

// SoftDelete marks a manifest as deleted by writing a Tombstone
// marker beside it. Idempotent: re-marking an already-tombstoned
// manifest is a no-op (the rename uses RenameIfNotExists semantics).
//
// The manifest body and its replica copy are NOT removed — chunk-GC
// runs separately, sweeps unreferenced chunks, and only then
// (subject to a configurable grace period) reaps the manifest body
// itself. This split is what makes "undelete the backup I just
// rotated" a viable operation in the timeframe.
//
// Chain protection: SoftDelete refuses to tombstone a manifest that
// has live (non-tombstoned) incremental descendants — tombstoning
// an anchor would leave its children un-restorable
// (chain.broken_tombstoned). Operators wanting to delete a chain
// have to soft-delete leaf-first; the rotate command does this
// naturally because retention promotes anchors via
// retention.promoteChainParents. Direct callers (e.g. `pg_hardstorage
// backup delete`) get a structured ErrChainHasLiveDescendants
// error with the descendant IDs in its message.
func (ms *ManifestStore) SoftDelete(ctx context.Context, deployment, backupID, policy, reason string) error {
	if err := validateRef(deployment, backupID); err != nil {
		return err
	}
	if deployment == "" || backupID == "" {
		return errors.New("backup: SoftDelete requires deployment and backupID")
	}
	// Legal-hold protection: a held manifest must not be
	// tombstoned regardless of caller — operator action,
	// retention, or cascade. The hold marker carries the holder
	// + reason from `backup hold add`; surfacing them in the
	// error lets the CLI tell the operator who put it there.
	//
	// Expired holds (ExpiresAt set + in the past) are NOT
	// active — the marker is still on disk for the audit trail
	// but no longer protects the manifest. A SoftDelete past an
	// expired hold proceeds normally; the audit-chain entry
	// captures the marker's continued presence.
	if h, err := ms.GetHold(ctx, deployment, backupID); err == nil && h != nil && h.ActiveAt(time.Now().UTC()) {
		return &ManifestHeldError{
			Deployment: deployment,
			BackupID:   backupID,
			Holder:     h.Holder,
			Reason:     h.Reason,
			HeldAt:     h.HeldAt,
		}
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("backup: SoftDelete: hold check: %w", err)
	}
	descendants, err := ms.findLiveDescendants(ctx, deployment, backupID)
	if err != nil {
		return fmt.Errorf("backup: SoftDelete: scan descendants: %w", err)
	}
	if len(descendants) > 0 {
		return &ChainHasLiveDescendantsError{
			Deployment:  deployment,
			BackupID:    backupID,
			Descendants: descendants,
		}
	}
	installed, err := ms.softDeleteUnchecked(ctx, deployment, backupID, policy, reason)
	if err != nil {
		return err
	}
	if !installed {
		// Already tombstoned by a concurrent caller — idempotent
		// no-op, and not our tombstone to second-guess.
		return nil
	}
	// Write-then-verify (closes the soft-delete-vs-incremental-backup
	// race). Our pre-scan ran BEFORE the tombstone existed, so a child
	// committed in the window between that scan and the tombstone write
	// would be missed — and tombstoning its parent would orphan it
	// (chain.broken_tombstoned), an un-restorable chain. Re-scan now
	// that the tombstone is installed: any child whose manifest is
	// visible here must keep its parent live, so we roll the tombstone
	// back and refuse. A child committing AFTER this point sees the
	// tombstone via Commit's post-write parent check and rolls ITSELF
	// back, so no orphan can survive on either side.
	descendants, err = ms.findLiveDescendants(ctx, deployment, backupID)
	if err != nil {
		return fmt.Errorf("backup: SoftDelete: re-scan descendants: %w", err)
	}
	if len(descendants) > 0 {
		if _, rbErr := ms.removeTombstone(ctx, deployment, backupID); rbErr != nil {
			return fmt.Errorf("backup: SoftDelete: a child of %s committed concurrently and rolling back the tombstone failed (the chain may now be tombstoned-but-orphaned; run `backup undelete %s` to restore it): %w",
				backupID, backupID, rbErr)
		}
		return &ChainHasLiveDescendantsError{
			Deployment:  deployment,
			BackupID:    backupID,
			Descendants: descendants,
		}
	}
	// Write-then-verify for legal holds (race-condition audit #1, symmetric
	// to the descendant guard above). The hold pre-check ran BEFORE the
	// tombstone existed, so a hold installed in the window between that
	// check and the tombstone write would be missed — silently tombstoning
	// a held backup and defeating the regulatory guarantee. Re-check now
	// that the tombstone is visible: if the backup is held, roll the
	// tombstone back and refuse, exactly as the pre-check would have.
	h, herr := ms.GetHold(ctx, deployment, backupID)
	if herr != nil && !errors.Is(herr, storage.ErrNotFound) {
		return fmt.Errorf("backup: SoftDelete: hold re-check: %w", herr)
	}
	if h != nil && h.ActiveAt(time.Now().UTC()) {
		if _, rbErr := ms.removeTombstone(ctx, deployment, backupID); rbErr != nil {
			return fmt.Errorf("backup: SoftDelete: a hold was placed on %s concurrently and rolling back the tombstone failed (the held backup may now be tombstoned; run `backup undelete %s` to restore it): %w",
				backupID, backupID, rbErr)
		}
		return &ManifestHeldError{
			Deployment: deployment,
			BackupID:   backupID,
			Holder:     h.Holder,
			Reason:     h.Reason,
			HeldAt:     h.HeldAt,
		}
	}
	return nil
}

// SoftDeleteCascade tombstones every backup in the chain
// rooted at backupID, leaf-first, then backupID itself. Used
// by `backup delete --cascade` to drain an entire incremental
// chain in one operator action.
//
// Order: reverse BFS over descendants. Direct children of
// backupID get tombstoned LAST (just before backupID itself);
// deepest leaves get tombstoned FIRST. This matches the
// chain-protection invariant — at every step, the manifest
// being tombstoned has no live descendants because they've
// already been tombstoned.
//
// Returns the IDs of all backups that were tombstoned, in
// deletion order (so audit emitters can record the sequence
// faithfully). An error mid-cascade leaves the partial state
// visible: the descendants tombstoned BEFORE the failure
// stay tombstoned; the operator can re-run after fixing the
// underlying issue and the cascade is naturally idempotent
// (already-tombstoned descendants are skipped by
// findLiveDescendants on the next pass).
//
// Idempotent on already-tombstoned root: when backupID is
// itself tombstoned, the cascade returns ([], nil) — nothing
// to do.
func (ms *ManifestStore) SoftDeleteCascade(ctx context.Context, deployment, backupID, policy, reason string) ([]string, error) {
	if err := validateRef(deployment, backupID); err != nil {
		return nil, err
	}
	if deployment == "" || backupID == "" {
		return nil, errors.New("backup: SoftDeleteCascade requires deployment and backupID")
	}
	// Idempotent root check: an already-tombstoned root means
	// the cascade was already run (or the operator is asking
	// for a no-op). Skip cleanly.
	if dead, err := ms.IsTombstoned(ctx, deployment, backupID); err != nil {
		return nil, fmt.Errorf("backup: SoftDeleteCascade: tombstone check: %w", err)
	} else if dead {
		return nil, nil
	}

	descendants, err := ms.findLiveDescendants(ctx, deployment, backupID)
	if err != nil {
		return nil, fmt.Errorf("backup: SoftDeleteCascade: scan descendants: %w", err)
	}

	// Pre-flight hold check across the ENTIRE chain. A cascade
	// that's "all or nothing" must refuse before tombstoning a
	// single link if any link is held — a partial cascade
	// (some leaves tombstoned, the rest blocked by a hold) is
	// strictly worse than no cascade because it leaves the
	// chain torn. We collect every held link and surface them
	// all at once so the operator sees the full picture instead
	// of fixing one hold and re-running into the next refusal.
	candidates := make([]string, 0, len(descendants)+1)
	candidates = append(candidates, descendants...)
	candidates = append(candidates, backupID)
	var heldLinks []ManifestHeldLink
	now := time.Now().UTC()
	for _, id := range candidates {
		h, herr := ms.GetHold(ctx, deployment, id)
		if herr != nil {
			if errors.Is(herr, storage.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("backup: SoftDeleteCascade: hold check %s: %w", id, herr)
		}
		if h == nil {
			continue
		}
		// Skip expired holds — the marker exists for audit but
		// no longer protects the manifest. Cascade proceeds.
		if !h.ActiveAt(now) {
			continue
		}
		heldLinks = append(heldLinks, ManifestHeldLink{
			BackupID: id,
			Holder:   h.Holder,
			Reason:   h.Reason,
			HeldAt:   h.HeldAt,
		})
	}
	if len(heldLinks) > 0 {
		return nil, &ChainHasHeldLinksError{
			Deployment: deployment,
			BackupID:   backupID,
			Held:       heldLinks,
		}
	}

	// Reverse-BFS order: deepest first. findLiveDescendants
	// returns direct children at the front and deeper leaves
	// at the back; iterating backward gives us leaf-first.
	deleted := make([]string, 0, len(descendants)+1)
	for i := len(descendants) - 1; i >= 0; i-- {
		id := descendants[i]
		if _, err := ms.softDeleteUnchecked(ctx, deployment, id, policy, reason); err != nil {
			return deleted, fmt.Errorf("backup: cascade-delete descendant %s: %w", id, err)
		}
		deleted = append(deleted, id)
	}
	// Root last.
	if _, err := ms.softDeleteUnchecked(ctx, deployment, backupID, policy, reason); err != nil {
		return deleted, fmt.Errorf("backup: cascade-delete root %s: %w", backupID, err)
	}
	deleted = append(deleted, backupID)

	// Write-then-verify (closes the cascade-vs-concurrent-incremental race,
	// symmetric to SoftDelete ~1056 and SoftDeleteBatch ~1278). The
	// pre-flight scan above ran BEFORE any tombstone existed, so a child
	// incremental that committed against any link in the window between that
	// scan and these tombstone writes would be missed — leaving the chain
	// tombstoned-but-orphaned. Re-scan now that every tombstone is durable:
	// any still-live descendant of the root means a child slipped in, so we
	// roll back all tombstones we installed and refuse.
	if live, rescanErr := ms.findLiveDescendants(ctx, deployment, backupID); rescanErr != nil {
		return deleted, fmt.Errorf("backup: SoftDeleteCascade: re-scan descendants: %w", rescanErr)
	} else if len(live) > 0 {
		if rbErr := ms.rollbackTombstones(ctx, deployment, deleted); rbErr != nil {
			return nil, fmt.Errorf("backup: SoftDeleteCascade: a child of %s committed concurrently; rolling back the cascade's tombstones failed — those chains may now be tombstoned-but-orphaned (run `backup undelete` on the affected backups): %w",
				backupID, rbErr)
		}
		return nil, &ChainHasLiveDescendantsError{
			Deployment:  deployment,
			BackupID:    backupID,
			Descendants: live,
		}
	}

	// Write-then-verify for legal holds (race-condition audit #1, symmetric
	// to SoftDelete's post-write hold re-check ~1078). The pre-flight hold
	// check ran BEFORE any tombstone existed, so a hold placed on any link
	// in the window would be silently defeated. Re-check the whole chain now
	// that the tombstones are visible: if any link is actively held, roll
	// back all tombstones and refuse.
	now = time.Now().UTC()
	var heldAfter []ManifestHeldLink
	for _, id := range deleted {
		h, herr := ms.GetHold(ctx, deployment, id)
		if herr != nil {
			if errors.Is(herr, storage.ErrNotFound) {
				continue
			}
			return deleted, fmt.Errorf("backup: SoftDeleteCascade: hold re-check %s: %w", id, herr)
		}
		if h == nil || !h.ActiveAt(now) {
			continue
		}
		heldAfter = append(heldAfter, ManifestHeldLink{
			BackupID: id,
			Holder:   h.Holder,
			Reason:   h.Reason,
			HeldAt:   h.HeldAt,
		})
	}
	if len(heldAfter) > 0 {
		if rbErr := ms.rollbackTombstones(ctx, deployment, deleted); rbErr != nil {
			return nil, fmt.Errorf("backup: SoftDeleteCascade: a hold was placed on the chain rooted at %s concurrently; rolling back the cascade's tombstones failed — held backups may now be tombstoned (run `backup undelete` on the affected backups): %w",
				backupID, rbErr)
		}
		return nil, &ChainHasHeldLinksError{
			Deployment: deployment,
			BackupID:   backupID,
			Held:       heldAfter,
		}
	}

	return deleted, nil
}

// rollbackTombstones removes the tombstones installed for the given ids,
// joining any per-id removal failures. Shared by SoftDeleteCascade's
// write-then-verify rollback paths.
func (ms *ManifestStore) rollbackTombstones(ctx context.Context, deployment string, ids []string) error {
	var rbErrs []error
	for _, id := range ids {
		if _, rbErr := ms.removeTombstone(ctx, deployment, id); rbErr != nil {
			rbErrs = append(rbErrs, fmt.Errorf("%s: %w", id, rbErr))
		}
	}
	if len(rbErrs) > 0 {
		return errors.Join(rbErrs...)
	}
	return nil
}

// SoftDeleteBatch soft-deletes many backups with a SINGLE scan of the
// deployment's manifests, instead of SoftDelete's per-call scan. A
// retention sweep deleting K backups one-by-one via SoftDelete is
// O(K·N) manifest reads (each call re-walks all N manifests, twice);
// SoftDeleteBatch is O(N) regardless of K. (CPU-pathology audit #2.)
//
// Chain protection is preserved: a backup whose live incremental
// descendant is NOT also in the batch would orphan that chain, so the
// whole batch is refused with *ChainHasLiveDescendantsError (atomic
// posture, like SoftDeleteCascade). Deleting a parent together with its
// descendants in the same batch is fine.
//
// Race protection is preserved too: after tombstoning, ONE re-scan
// catches an incremental that committed concurrently against a member
// of the batch; if found, the batch's tombstones are rolled back and
// the call refuses — the batched form of SoftDelete's write-then-verify.
// Held backups are refused up-front. Returns the IDs actually
// tombstoned (already-tombstoned members are skipped, not errors).
func (ms *ManifestStore) SoftDeleteBatch(ctx context.Context, deployment string, ids []string, policy, reason string) ([]string, error) {
	if err := validateStorageID("deployment", deployment); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	batch := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if err := validateStorageID("backup ID", id); err != nil {
			return nil, err
		}
		batch[id] = struct{}{}
	}

	// Legal-hold protection: refuse if any member is actively held.
	now := time.Now().UTC()
	for _, id := range ids {
		if h, err := ms.GetHold(ctx, deployment, id); err == nil && h != nil && h.ActiveAt(now) {
			return nil, &ManifestHeldError{
				Deployment: deployment, BackupID: id,
				Holder: h.Holder, Reason: h.Reason, HeldAt: h.HeldAt,
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("backup: SoftDeleteBatch: hold check %s: %w", id, err)
		}
	}

	// One scan → chain-protection check for the whole batch.
	cs, err := ms.loadChainSnapshot(ctx, deployment)
	if err != nil {
		return nil, fmt.Errorf("backup: SoftDeleteBatch: scan: %w", err)
	}
	if id, dep, bad := firstOrphaningDelete(cs, batch, ids); bad {
		return nil, &ChainHasLiveDescendantsError{
			Deployment: deployment, BackupID: id, Descendants: []string{dep},
		}
	}

	// Tombstone every member.
	installed := make([]string, 0, len(ids))
	for _, id := range ids {
		ok, err := ms.softDeleteUnchecked(ctx, deployment, id, policy, reason)
		if err != nil {
			return installed, fmt.Errorf("backup: SoftDeleteBatch: tombstone %s: %w", id, err)
		}
		if ok {
			installed = append(installed, id)
		}
	}

	// Write-then-verify (batched): one re-scan catches a child that
	// committed against any member during the window. Roll back all of
	// the batch's tombstones if so.
	cs2, err := ms.loadChainSnapshot(ctx, deployment)
	if err != nil {
		return installed, fmt.Errorf("backup: SoftDeleteBatch: re-scan: %w", err)
	}
	if id, dep, bad := firstOrphaningDelete(cs2, batch, installed); bad {
		var rbErrs []error
		for _, undo := range installed {
			if _, rbErr := ms.removeTombstone(ctx, deployment, undo); rbErr != nil {
				rbErrs = append(rbErrs, fmt.Errorf("%s: %w", undo, rbErr))
			}
		}
		if len(rbErrs) > 0 {
			// A rollback Delete failed: tombstones we just installed
			// remain, so those chains are now tombstoned-but-orphaned.
			// Surface it (the single-delete path does the same) instead
			// of dropping the error (poor-error-handling audit #2).
			return nil, fmt.Errorf("backup: SoftDeleteBatch: a child of %s committed concurrently; rolling back %d of %d tombstone(s) failed — those chains may now be tombstoned-but-orphaned (run `backup undelete` on the affected backups): %w",
				id, len(rbErrs), len(installed), errors.Join(rbErrs...))
		}
		return nil, &ChainHasLiveDescendantsError{
			Deployment: deployment, BackupID: id, Descendants: []string{dep},
		}
	}

	// Write-then-verify for legal holds (symmetric to SoftDelete's
	// post-write hold re-check ~1078). The hold pre-check ran BEFORE any
	// tombstone existed, so a hold placed on a member in the window would
	// be silently defeated. Re-check the tombstoned members now that the
	// markers are durable: if any is actively held, roll back all of the
	// batch's tombstones and refuse.
	nowAfter := time.Now().UTC()
	for _, id := range installed {
		h, herr := ms.GetHold(ctx, deployment, id)
		if herr != nil {
			if errors.Is(herr, storage.ErrNotFound) {
				continue
			}
			return installed, fmt.Errorf("backup: SoftDeleteBatch: hold re-check %s: %w", id, herr)
		}
		if h == nil || !h.ActiveAt(nowAfter) {
			continue
		}
		if rbErr := ms.rollbackTombstones(ctx, deployment, installed); rbErr != nil {
			return nil, fmt.Errorf("backup: SoftDeleteBatch: a hold was placed on %s concurrently; rolling back the batch's tombstones failed — held backups may now be tombstoned (run `backup undelete` on the affected backups): %w",
				id, rbErr)
		}
		return nil, &ManifestHeldError{
			Deployment: deployment, BackupID: id,
			Holder: h.Holder, Reason: h.Reason, HeldAt: h.HeldAt,
		}
	}
	return installed, nil
}

// firstOrphaningDelete reports the first id in `ids` that has a live
// descendant NOT contained in `batch` (deleting it would orphan a chain
// member that survives). Returns (id, descendant, true) on the first
// such pair.
func firstOrphaningDelete(cs chainSnapshot, batch map[string]struct{}, ids []string) (string, string, bool) {
	for _, id := range ids {
		for _, d := range cs.descendants(id) {
			if _, inBatch := batch[d]; !inBatch {
				return id, d, true
			}
		}
	}
	return "", "", false
}

// softDeleteUnchecked installs a tombstone marker without
// running the chain-protection scan. Used by SoftDelete (after
// the scan confirms no descendants) AND by SoftDeleteCascade
// (which has already arranged for descendants to be tombstoned
// in the right order, so the per-step check would always pass
// — and skipping it avoids re-walking the manifest list once
// per cascade step, which makes the cascade O(N) rather than
// O(N²) on a deep chain).
// softDeleteUnchecked returns installed=true when it wrote a fresh
// tombstone, false when the manifest was already tombstoned (the
// idempotent no-op). The caller uses this to decide whether a
// rollback of "its" tombstone is its to make.
func (ms *ManifestStore) softDeleteUnchecked(ctx context.Context, deployment, backupID, policy, reason string) (bool, error) {
	t := Tombstone{
		Schema:       TombstoneSchema,
		BackupID:     backupID,
		Deployment:   deployment,
		TombstonedAt: time.Now().UTC(),
		Policy:       policy,
		Reason:       reason,
	}
	body, err := encodeJSON(&t)
	if err != nil {
		return false, fmt.Errorf("backup: encode tombstone: %w", err)
	}
	key := TombstonePath(deployment, backupID)
	tmp := key + ".tmp." + randSuffix()
	if _, err := ms.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return false, fmt.Errorf("backup: put tombstone tmp: %w", err)
	}
	if err := ms.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = ms.sp.Delete(ctx, tmp)
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Already tombstoned. Nothing to do.
			return false, nil
		}
		return false, fmt.Errorf("backup: install tombstone: %w", err)
	}
	return true, nil
}

// Undelete removes the tombstone marker for deployment/backupID,
// resurrecting a soft-deleted manifest. Mirror of SoftDelete: where
// SoftDelete writes the tombstone marker, Undelete deletes it. The
// manifest body itself is untouched on either side — it has been
// sitting on disk the whole time, just hidden from List/Read by the
// marker. So the resurrection is a single Delete call against the
// tombstone path.
//
// Returns true when the call actually removed a marker, false when
// the manifest was already live (no marker present). The bool lets
// the CLI report "restored" vs "already-live (no-op)" without an
// extra IsTombstoned round-trip.
//
// Idempotent: an Undelete of an already-live manifest is a no-op
// (false, nil). Race-tolerant: if another caller removes the marker
// between our Stat and Delete, we treat the resulting ErrNotFound
// from Delete as "already gone" rather than as an error.
//
// Pairs with `backup undelete <id>` on the CLI. The audit-chain
// entry from the original SoftDelete remains; an undelete adds its
// own entry (`backup.undelete`) so the trail captures both halves
// of the round-trip.
//
// Restorability pre-flight (safe by default): once `repo gc --apply`
// has reclaimed the chunks a manifest references, resurrecting it
// would hand back a healthy-LOOKING backup whose data is gone —
// restore fails only later, at the worst time. Undelete therefore
// Stats every referenced chunk BEFORE removing the tombstone and
// fails closed with *UndeleteChunksMissingError (matches
// errors.Is ErrUndeleteChunksMissing), LEAVING THE TOMBSTONE IN
// PLACE, if any chunk is gone. An operator who knows the data is
// gone and wants only the metadata back uses UndeleteForce.
func (ms *ManifestStore) Undelete(ctx context.Context, deployment, backupID string) (bool, error) {
	if err := validateRef(deployment, backupID); err != nil {
		return false, err
	}
	if deployment == "" || backupID == "" {
		return false, errors.New("backup: Undelete requires deployment and backupID")
	}
	dead, err := ms.IsTombstoned(ctx, deployment, backupID)
	if err != nil {
		return false, fmt.Errorf("backup: Undelete: tombstone check: %w", err)
	}
	if !dead {
		// Already live. Idempotent no-op — nothing to resurrect, so
		// no restorability check needed.
		return false, nil
	}
	// Read the still-tombstoned body without signature verification
	// (we only need Files/Chunks, mirroring findLiveDescendants) and
	// confirm every referenced chunk is still addressable in the repo.
	m, rerr := ms.readManifestUnverified(ctx, deployment, backupID)
	if rerr != nil {
		return false, fmt.Errorf("backup: Undelete: read manifest %s/%s: %w", deployment, backupID, rerr)
	}
	res, cerr := CheckChunkExistence(ctx, ms.sp, m)
	if cerr != nil {
		return false, fmt.Errorf("backup: Undelete: chunk pre-flight %s/%s: %w", deployment, backupID, cerr)
	}
	if !res.AllPresent() {
		missing := make([]string, 0, len(res.Missing))
		for _, h := range res.Missing {
			missing = append(missing, h.String())
		}
		return false, &UndeleteChunksMissingError{
			Deployment:  deployment,
			BackupID:    backupID,
			TotalUnique: res.TotalUnique,
			Missing:     missing,
		}
	}
	return ms.removeTombstone(ctx, deployment, backupID)
}

// UndeleteForce removes a backup's tombstone WITHOUT the chunk
// restorability pre-flight that Undelete performs. Reserved for the
// forensic case where an operator wants a soft-deleted manifest's
// metadata back even though chunk-GC has already reclaimed its data
// (e.g. to read its provenance, or to re-point it at re-uploaded
// chunks). The resurrected backup may be un-restorable — that is the
// caller's explicit, eyes-open choice. Prefer Undelete.
func (ms *ManifestStore) UndeleteForce(ctx context.Context, deployment, backupID string) (bool, error) {
	if deployment == "" || backupID == "" {
		return false, errors.New("backup: UndeleteForce requires deployment and backupID")
	}
	dead, err := ms.IsTombstoned(ctx, deployment, backupID)
	if err != nil {
		return false, fmt.Errorf("backup: UndeleteForce: tombstone check: %w", err)
	}
	if !dead {
		return false, nil
	}
	return ms.removeTombstone(ctx, deployment, backupID)
}

// backupIsLive reports whether deployment/backupID has a committed,
// non-tombstoned primary manifest. Used by Commit's chain-safety
// check (is an incremental's parent still restorable?). A backup is
// live iff it is NOT tombstoned AND its primary manifest object
// exists.
func (ms *ManifestStore) backupIsLive(ctx context.Context, deployment, backupID string) (bool, error) {
	dead, err := ms.IsTombstoned(ctx, deployment, backupID)
	if err != nil {
		return false, fmt.Errorf("tombstone check: %w", err)
	}
	if dead {
		return false, nil
	}
	if _, err := ms.sp.Stat(ctx, PrimaryPath(deployment, backupID)); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("stat primary: %w", err)
	}
	return true, nil
}

// readManifestUnverified loads a manifest body (primary, then
// replica) and parses it WITHOUT signature verification. Used by
// internal restorability checks (Undelete's chunk pre-flight) that
// only need Files/Chunks and must work on a manifest that is
// tombstoned. Mirrors ReadAttestationless but does NOT refuse a
// tombstoned manifest (the tombstone is a sidecar; manifest.json
// itself is still present until chunk-GC hard-deletes it).
func (ms *ManifestStore) readManifestUnverified(ctx context.Context, deployment, backupID string) (*Manifest, error) {
	primaryKey := PrimaryPath(deployment, backupID)
	if rc, err := ms.sp.Get(ctx, primaryKey); err == nil {
		body, rerr := readAllLimited(rc, MaxManifestBytes)
		_ = rc.Close()
		if rerr == nil {
			if m, perr := ParseAttestationless(body); perr == nil {
				return m, nil
			}
		}
	}
	replicaKey := ReplicaPath(backupID)
	rc2, err := ms.sp.Get(ctx, replicaKey)
	if err != nil {
		return nil, err
	}
	defer rc2.Close()
	body2, rerr2 := readAllLimited(rc2, MaxManifestBytes)
	if rerr2 != nil {
		return nil, rerr2
	}
	return ParseAttestationless(body2)
}

// readKeyBytes returns the raw body at key (Get + ReadAll).
func (ms *ManifestStore) readKeyBytes(ctx context.Context, key string) ([]byte, error) {
	rc, err := ms.sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readAllLimited(rc, MaxManifestBytes)
}

// EnsureReplica guarantees the redundant replica copy of a committed
// manifest exists and verifies. If the replica is missing or
// unverifiable, it is rebuilt from the (signature-verified) primary;
// returns rebuilt=true when a new replica was written.
//
// This heals the best-effort-replica gap (data-loss audit path #3):
// Commit treats the replica write as non-fatal, so a transient backend
// error — or an out-of-band deletion — can leave a live primary with
// NO fallback against later corruption. EnsureReplica restores that
// cross-prefix corruption survivability. It refuses to manufacture a
// replica from a primary that doesn't verify (no good copy to copy
// from). The `repair manifest` command calls this when the replica is
// the missing side.
//
// retainUntil + mode carry the destination repo's WORM policy so a rebuilt
// replica is locked exactly like the commit-time copy (commitReplica
// applies it via commitAtomic). Without this, a `repair manifest` on a
// compliance repo would resurrect the replica's redundancy but leave that
// copy freely deletable — a sibling of the replicate/azblob/gcs WORM gaps.
// A zero retainUntil means "no lock" (non-WORM repo).
func (ms *ManifestStore) EnsureReplica(ctx context.Context, deployment, backupID string, verifier *Verifier, retainUntil time.Time, mode storage.WORMMode) (bool, error) {
	if deployment == "" || backupID == "" {
		return false, errors.New("backup: EnsureReplica requires deployment and backupID")
	}
	replicaKey := ReplicaPath(backupID)

	// Replica already present AND verifies → redundancy intact.
	if rb, err := ms.readKeyBytes(ctx, replicaKey); err == nil {
		if _, perr := ParseAndVerify(rb, verifier); perr == nil {
			return false, nil
		}
		// Present but corrupt — fall through and rebuild it.
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, fmt.Errorf("backup: EnsureReplica: read replica: %w", err)
	}

	// Rebuild from the verified primary.
	pb, err := ms.readKeyBytes(ctx, PrimaryPath(deployment, backupID))
	if err != nil {
		return false, fmt.Errorf("backup: EnsureReplica: read primary (no good copy to rebuild from): %w", err)
	}
	pm, err := ParseAndVerify(pb, verifier)
	if err != nil {
		return false, fmt.Errorf("backup: EnsureReplica: primary does not verify, refusing to write an unverified replica: %w", err)
	}
	if pm.Deployment != deployment || pm.BackupID != backupID {
		return false, fmt.Errorf("backup: EnsureReplica: primary identity %s/%s != requested %s/%s", pm.Deployment, pm.BackupID, deployment, backupID)
	}

	tmp := replicaKey + ".tmp.ensure." + randSuffix()
	if _, err := ms.sp.Put(ctx, tmp, bytes.NewReader(pb), storage.PutOptions{ContentLength: int64(len(pb))}); err != nil {
		return false, fmt.Errorf("backup: EnsureReplica: stage replica tmp: %w", err)
	}
	_ = ms.sp.Delete(ctx, replicaKey) // idempotent on a missing key; clears a corrupt one
	if err := ms.sp.RenameIfNotExists(ctx, tmp, replicaKey); err != nil {
		_ = ms.sp.Delete(ctx, tmp)
		if errors.Is(err, storage.ErrAlreadyExists) {
			return false, nil // a concurrent rebuild won the race (it applies the lock)
		}
		return false, fmt.Errorf("backup: EnsureReplica: install replica: %w", err)
	}

	// Apply the repo's WORM retention to the rebuilt replica, matching what
	// commitAtomic does for the commit-time copy. Empty mode defaults to
	// Compliance; a backend without WORM returns ErrUnsupported, ignored.
	if !retainUntil.IsZero() {
		m := mode
		if m == "" {
			m = storage.WORMCompliance
		}
		if err := ms.sp.SetRetention(ctx, replicaKey, retainUntil, m); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			return false, fmt.Errorf("backup: EnsureReplica: lock rebuilt replica: %w", err)
		}
	}
	return true, nil
}

// removeTombstone deletes the tombstone marker. Race-tolerant: a
// concurrent removal surfaces as ErrNotFound from Delete, which we
// treat as "already gone" (the caller's intent — the marker should
// not exist — is satisfied) rather than as an error.
func (ms *ManifestStore) removeTombstone(ctx context.Context, deployment, backupID string) (bool, error) {
	if err := ms.sp.Delete(ctx, TombstonePath(deployment, backupID)); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("backup: delete tombstone: %w", err)
	}
	return true, nil
}

// findLiveDescendants returns the IDs of every visible (non-
// tombstoned) manifest in deployment whose parent_backup_id chain
// transitively descends from backupID. Used by SoftDelete to refuse
// breaking incremental chains.
//
// Scan-and-walk implementation: we read every visible manifest in
// the deployment once (O(N) reads) and build a parent-id index,
// then BFS forward from backupID. For the+ fleet sizes (hundreds
// of backups per deployment) this is fine. A future indexed lookup
// can drop in if profiling shows it matters.
//
// Reads use a nil verifier — we trust the manifest store's
// signature pipeline to have already vetted these on commit; for
// chain-protection we only need parent_backup_id, which sits in
// the JSON envelope and doesn't depend on signature validity. A
// signature-broken manifest on a chain is its own problem
// (`audit verify-chain` will surface it) — we don't want
// SoftDelete to refuse just because some unrelated manifest in the
// deployment has a broken signature.
func (ms *ManifestStore) findLiveDescendants(ctx context.Context, deployment, backupID string) ([]string, error) {
	cs, err := ms.loadChainSnapshot(ctx, deployment)
	if err != nil {
		return nil, err
	}
	return cs.descendants(backupID), nil
}

// chainKid is one parent-edge: a live backup and its parent_backup_id.
type chainKid struct{ id, parent string }

// chainSnapshot is the parent-edge view of a deployment's LIVE
// (non-tombstoned) manifests, captured by a single List+read pass. It
// is reused across many descendant queries so a BATCH delete builds it
// ONCE (O(N)) instead of re-scanning per backup (O(K·N)). See
// SoftDeleteBatch / CPU-pathology audit #2.
type chainSnapshot struct {
	live []chainKid
}

// loadChainSnapshot reads every live manifest's (backup_id,
// parent_backup_id) in one pass, using the partial-decode shape so
// chain protection doesn't depend on signature verification (see
// findLiveDescendants' doc above).
func (ms *ManifestStore) loadChainSnapshot(ctx context.Context, deployment string) (chainSnapshot, error) {
	prefix := "manifests/" + deployment + "/backups/"
	const manifestSuffix = "/manifest.json"
	const tombstoneSuffix = "/manifest.json.tombstone"

	tombstoned := map[string]struct{}{}
	manifestKeys := []string{}
	for info, err := range ms.sp.List(ctx, prefix) {
		if err != nil {
			return chainSnapshot{}, err
		}
		switch {
		case strings.HasSuffix(info.Key, tombstoneSuffix):
			rel := strings.TrimPrefix(info.Key, prefix)
			if slash := strings.IndexByte(rel, '/'); slash > 0 {
				tombstoned[rel[:slash]] = struct{}{}
			}
		case strings.HasSuffix(info.Key, manifestSuffix):
			manifestKeys = append(manifestKeys, info.Key)
		}
	}

	type slim struct {
		BackupID       string `json:"backup_id"`
		ParentBackupID string `json:"parent_backup_id"`
	}
	var live []chainKid
	for _, key := range manifestKeys {
		rel := strings.TrimPrefix(key, prefix)
		slash := strings.IndexByte(rel, '/')
		if slash <= 0 {
			continue
		}
		id := rel[:slash]
		if _, dead := tombstoned[id]; dead {
			continue // tombstoned manifests don't anchor anything
		}
		rc, err := ms.sp.Get(ctx, key)
		if err != nil {
			return chainSnapshot{}, fmt.Errorf("read %s: %w", key, err)
		}
		body, err := readAllLimited(rc, MaxManifestBytes)
		_ = rc.Close()
		if err != nil {
			return chainSnapshot{}, fmt.Errorf("read body %s: %w", key, err)
		}
		var s slim
		if err := json.Unmarshal(body, &s); err != nil {
			// A malformed manifest can't have a usable parent
			// reference; skip rather than failing the delete.
			continue
		}
		if s.ParentBackupID == "" {
			continue
		}
		live = append(live, chainKid{id: s.BackupID, parent: s.ParentBackupID})
	}
	return chainSnapshot{live: live}, nil
}

// descendants returns the IDs of every live backup whose parent chain
// transitively descends from backupID (BFS over the parent-edges),
// sorted. backupID itself need not be live (a just-tombstoned anchor
// can still have live children that committed before it was deleted).
func (cs chainSnapshot) descendants(backupID string) []string {
	descendants := []string{}
	seen := map[string]struct{}{backupID: {}}
	frontier := []string{backupID}
	for len(frontier) > 0 {
		next := frontier[0]
		frontier = frontier[1:]
		for _, k := range cs.live {
			if k.parent != next {
				continue
			}
			if _, ok := seen[k.id]; ok {
				continue
			}
			seen[k.id] = struct{}{}
			descendants = append(descendants, k.id)
			frontier = append(frontier, k.id)
		}
	}
	sort.Strings(descendants)
	return descendants
}

// ChainHasLiveDescendantsError is returned by SoftDelete when the
// caller asked to tombstone a manifest whose visible incremental
// chain still contains live descendants. The descendants are
// included so the caller can report them or cascade-delete.
type ChainHasLiveDescendantsError struct {
	Deployment  string
	BackupID    string
	Descendants []string
}

// Error implements error.
func (e *ChainHasLiveDescendantsError) Error() string {
	return fmt.Sprintf("backup: %s/%s has %d live incremental descendant(s) (%s); soft-delete leaf-first or rotate via retention",
		e.Deployment, e.BackupID, len(e.Descendants), strings.Join(e.Descendants, ", "))
}

// ErrChainHasLiveDescendants is the sentinel for errors.Is checks
// against ChainHasLiveDescendantsError. Callers wanting to gate on
// the chain-protection refusal use:
//
//	if errors.Is(err, backup.ErrChainHasLiveDescendants) { ... }
var ErrChainHasLiveDescendants = errors.New("backup: chain has live descendants")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *ChainHasLiveDescendantsError) Is(target error) bool {
	return target == ErrChainHasLiveDescendants
}

// UndeleteChunksMissingError is returned by Undelete when the
// manifest being resurrected references chunks that are no longer in
// the repo (chunk-GC reclaimed them, or they never wrote).
// Resurrecting it would yield an un-restorable backup, so Undelete
// fails closed and leaves the tombstone in place. The missing-hash
// list (hex, truncatable by the caller) and totals are surfaced so
// the operator can see what's gone. UndeleteForce bypasses the check.
type UndeleteChunksMissingError struct {
	Deployment  string
	BackupID    string
	TotalUnique int
	Missing     []string
}

// Error implements error.
func (e *UndeleteChunksMissingError) Error() string {
	return fmt.Sprintf("backup: %s/%s references %d chunk(s) no longer in the repo (%d of %d missing); resurrecting it would be un-restorable — use force to recover the metadata anyway",
		e.Deployment, e.BackupID, len(e.Missing), len(e.Missing), e.TotalUnique)
}

// ErrUndeleteChunksMissing is the sentinel for errors.Is checks
// against UndeleteChunksMissingError:
//
//	if errors.Is(err, backup.ErrUndeleteChunksMissing) { ... }
var ErrUndeleteChunksMissing = errors.New("backup: undelete chunks missing")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *UndeleteChunksMissingError) Is(target error) bool {
	return target == ErrUndeleteChunksMissing
}

// OrphanedIncrementalError is returned by Commit when an incremental
// backup's parent was soft-deleted (or vanished) concurrently with
// the commit, so completing it would leave an un-restorable orphan.
// Commit rolls its own just-written manifest back before returning
// this. The backup runner surfaces it as a failed run; re-running
// after picking a live parent (or undeleting the parent) succeeds.
type OrphanedIncrementalError struct {
	Deployment     string
	BackupID       string
	ParentBackupID string
}

// Error implements error.
func (e *OrphanedIncrementalError) Error() string {
	return fmt.Sprintf("backup: incremental %s/%s was orphaned — its parent %s was soft-deleted during the backup; the commit was rolled back (restore would have been impossible). Re-run against a live parent or undelete %s first",
		e.Deployment, e.BackupID, e.ParentBackupID, e.ParentBackupID)
}

// ErrOrphanedIncremental is the sentinel for errors.Is checks against
// OrphanedIncrementalError:
//
//	if errors.Is(err, backup.ErrOrphanedIncremental) { ... }
var ErrOrphanedIncremental = errors.New("backup: orphaned incremental")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *OrphanedIncrementalError) Is(target error) bool {
	return target == ErrOrphanedIncremental
}

// ManifestHeldError is returned by SoftDelete when the caller asked
// to tombstone a manifest that has an active legal hold. The hold
// metadata (holder + reason + when) is surfaced so the CLI can
// tell the operator who put the hold there and why — fixing a
// "I can't delete this" complaint usually starts with knowing who
// owns the hold.
//
// The hold-protection invariant is non-negotiable: a held manifest
// cannot be tombstoned by ANY caller (operator, retention,
// cascade) until the hold is released via `backup hold remove`.
// This is what makes "legal hold" actually mean "legal hold" —
// regulatory protection that survives over-eager cascades and
// wrong-policy retention runs.
type ManifestHeldError struct {
	Deployment string
	BackupID   string
	Holder     string
	Reason     string
	HeldAt     time.Time
}

// Error implements error.
func (e *ManifestHeldError) Error() string {
	bits := []string{}
	if e.Holder != "" {
		bits = append(bits, "holder="+e.Holder)
	}
	if e.Reason != "" {
		bits = append(bits, "reason="+e.Reason)
	}
	if !e.HeldAt.IsZero() {
		bits = append(bits, "held_at="+e.HeldAt.Format(time.RFC3339))
	}
	suffix := ""
	if len(bits) > 0 {
		suffix = " (" + strings.Join(bits, ", ") + ")"
	}
	return fmt.Sprintf("backup: %s/%s is on legal hold and cannot be deleted%s; release with `backup hold remove`",
		e.Deployment, e.BackupID, suffix)
}

// ErrManifestHeld is the sentinel for errors.Is checks. Callers
// wanting to gate on the hold-protection refusal use:
//
//	if errors.Is(err, backup.ErrManifestHeld) { ... }
var ErrManifestHeld = errors.New("backup: manifest on legal hold")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *ManifestHeldError) Is(target error) bool {
	return target == ErrManifestHeld
}

// ManifestTombstonedError is returned by PutHold when the operator tries
// to place a hold on an already-soft-deleted (tombstoned) backup. A hold
// there gives false protection: GC reaps a tombstoned backup's chunks
// after the grace window WITHOUT consulting holds, so the "legal hold"
// would be silently defeated. The symmetric refusal to SoftDelete's
// hold check — together they keep a backup from ever being both held and
// tombstoned.
type ManifestTombstonedError struct {
	Deployment string
	BackupID   string
}

// Error implements error.
func (e *ManifestTombstonedError) Error() string {
	return fmt.Sprintf("backup: %s/%s is soft-deleted (tombstoned) and cannot be placed on hold; `backup undelete %s` first, then add the hold",
		e.Deployment, e.BackupID, e.BackupID)
}

// ErrManifestTombstoned is the sentinel for errors.Is checks.
var ErrManifestTombstoned = errors.New("backup: manifest is tombstoned")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *ManifestTombstonedError) Is(target error) bool {
	return target == ErrManifestTombstoned
}

// ManifestHeldLink is one entry in a ChainHasHeldLinksError. The
// fields mirror Hold's so the CLI can list every held link with
// its holder + reason + held_at in one consolidated message —
// without forcing the operator to query each link individually.
type ManifestHeldLink struct {
	BackupID string
	Holder   string
	Reason   string
	HeldAt   time.Time
}

// ChainHasHeldLinksError is returned by SoftDeleteCascade when ANY
// link in the chain (root, descendant, or anywhere between) has an
// active legal hold. The cascade refuses up-front — a partial
// cascade that leaves some links tombstoned and others held would
// tear the incremental chain in a way no future operation cleanly
// recovers.
//
// The error carries the full list of held links so the operator
// fixes them all at once rather than learning them one
// hold-at-a-time across multiple re-runs.
type ChainHasHeldLinksError struct {
	Deployment string
	BackupID   string
	Held       []ManifestHeldLink
}

// Error implements error.
func (e *ChainHasHeldLinksError) Error() string {
	ids := make([]string, 0, len(e.Held))
	for _, l := range e.Held {
		ids = append(ids, l.BackupID)
	}
	return fmt.Sprintf("backup: cascade aborted — %s/%s chain has %d held link(s): %s; release each hold and re-run",
		e.Deployment, e.BackupID, len(e.Held), strings.Join(ids, ", "))
}

// ErrChainHasHeldLinks is the sentinel for errors.Is on
// ChainHasHeldLinksError.
var ErrChainHasHeldLinks = errors.New("backup: chain has held links")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *ChainHasHeldLinksError) Is(target error) bool {
	return target == ErrChainHasHeldLinks
}

// ReadTombstone reads and decodes the tombstone marker for
// deployment/backupID, returning the parsed body. Used by
// `list --include-deleted` and `show --include-deleted` to
// surface WHEN a manifest was deleted and WHY without making
// the operator chase down the audit log.
//
// Returns (nil, storage.ErrNotFound) if the manifest is not
// tombstoned. Schema-version mismatches surface as an error so
// future on-disk-format changes don't silently misparse old
// markers — callers that want to tolerate unknown schemas
// should branch on that condition explicitly.
func (ms *ManifestStore) ReadTombstone(ctx context.Context, deployment, backupID string) (*Tombstone, error) {
	if deployment == "" || backupID == "" {
		return nil, errors.New("backup: ReadTombstone requires deployment and backupID")
	}
	rc, err := ms.sp.Get(ctx, TombstonePath(deployment, backupID))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := readAllLimited(rc, MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("backup: read tombstone: %w", err)
	}
	var t Tombstone
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("backup: decode tombstone %s/%s: %w", deployment, backupID, err)
	}
	if t.Schema != TombstoneSchema {
		return nil, fmt.Errorf("backup: tombstone %s/%s has unsupported schema %q",
			deployment, backupID, t.Schema)
	}
	return &t, nil
}

// IsTombstoned reports whether deployment/backupID has a tombstone
// marker present. Returns (false, nil) for absent; (true, nil) for
// present; (false, err) on backend errors other than not-found.
func (ms *ManifestStore) IsTombstoned(ctx context.Context, deployment, backupID string) (bool, error) {
	if err := validateRef(deployment, backupID); err != nil {
		return false, err
	}
	_, err := ms.sp.Stat(ctx, TombstonePath(deployment, backupID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// encodeJSON is a small canonical-JSON helper for Tombstone bodies.
// Same shape as Manifest.MarshalToBytes (no HTML escape, no trailing
// newline).
func encodeJSON(v any) ([]byte, error) {
	var buf canonicalBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.TrimTrailingNewline(), nil
}

// ErrAlreadyCommitted is returned when Commit is called for a backup_id
// whose manifest is already present at the primary path.
var ErrAlreadyCommitted = errors.New("backup: manifest already committed")

// ErrTombstoned is returned by Read when the requested manifest has
// been soft-deleted. Callers wanting to inspect tombstoned manifests
// (e.g. an undelete operation) should read the body directly via the
// underlying StoragePlugin.
var ErrTombstoned = errors.New("backup: manifest tombstoned (soft-deleted)")

// randSuffix returns a short random hex suffix for tmp-file names so
// concurrent commits don't collide. Using crypto/rand keeps init() free
// of seed-management concerns; 8 random bytes (16 hex chars) is plenty.
func randSuffix() string {
	var b [8]byte
	_, _ = readRand(b[:])
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out)
}

// timetarget.go — ResolveBackupForTime: picks the latest backup old enough to seed a PITR rewind.
package restore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// ResolveBackupForTime returns the BackupID of the LATEST backup
// whose StoppedAt is at or before target — the right starting
// point for time-targeted PITR. PG's recovery replay marches
// forward from that backup's stop_lsn through WAL until it
// reaches target_time; a backup whose StoppedAt is AFTER target
// can't be the seed (the recovery would have to go backwards).
//
// Operationally this is the auto-resolve for `restore --to "5
// minutes ago"`: instead of always picking the most-recent
// backup (the prior `latest` semantics), pick the most-recent
// backup OLD ENOUGH to seed the requested rewind.
//
// Returns ErrNoBackupBeforeTime when every verifiable manifest
// for the deployment has StoppedAt > target. Returns
// ErrNoBackupsFound when there are no verifiable manifests at
// all (mirroring ResolveLatest).
//
// Tombstoned manifests are skipped — same posture as
// ResolveLatest. A tombstoned backup is unrestorable; auto-
// resolving onto one would surprise the operator.
//
// Per-entry verification failures are counted but don't abort
// the walk — a single corrupt manifest shouldn't shadow the
// rest. The function returns a structured "all manifests
// failed verification" error if no verifiable manifest survived.
//
// Prefer ResolveBackupForTimeDetailed in any path that can tell the
// operator something: this form drops the skipped-manifest count, and
// a skipped manifest may have been the BETTER seed.
func ResolveBackupForTime(ctx context.Context, sp storage.StoragePlugin, deployment string, target time.Time, verifier *backup.Verifier) (string, error) {
	id, _, err := ResolveBackupForTimeDetailed(ctx, sp, deployment, target, verifier)
	return id, err
}

// ResolveBackupForTimeDetailed is ResolveBackupForTime plus the number
// of manifests that could not be evaluated.
//
// This is the same gap ResolveLatestDetailed closes, and it bites
// harder here. The answer is "the LATEST backup at or before target",
// so the manifest that gets skipped may be precisely the one that would
// have won — the closest seed below the target. Falling back to an
// older one is not a neutral substitution: PG then has to replay every
// WAL segment between that older stop_lsn and the target, which is a
// longer span, more time, and more opportunity to run into a pruned or
// gap-recorded stretch of archive.
//
// A skipped manifest also corrupts the other side of the answer.
// laterCount feeds the "you have N more recent backups" hint in
// NoBackupBeforeTimeError, and a manifest that could not be read was
// never classified as earlier or later, so that count is a floor rather
// than a total.
//
// As in ResolveLatestDetailed, the count cannot be narrowed to "only
// the ones that might have mattered": deciding whether a manifest sits
// before or after the target means reading the manifest that just
// failed to read.
func ResolveBackupForTimeDetailed(ctx context.Context, sp storage.StoragePlugin, deployment string, target time.Time, verifier *backup.Verifier) (string, int, error) {
	if target.IsZero() {
		return "", 0, errors.New("restore: ResolveBackupForTime requires a non-zero target time")
	}
	store := backup.NewManifestStore(sp)
	var (
		bestID  string
		bestT   time.Time
		seen    int
		errored int
		// laterCount tracks manifests that exist but have
		// StoppedAt AFTER target — used to give a helpful
		// "you have N more recent backups" message when no
		// backup before target exists.
		laterCount int
	)
	for m, err := range store.List(ctx, deployment, verifier) {
		if err != nil {
			errored++
			continue
		}
		seen++
		stop := m.StoppedAt.UTC()
		if stop.After(target) {
			laterCount++
			continue
		}
		// Pick the LATEST among those at-or-before target.
		// Strict After (not !Before) so equal-StoppedAt
		// manifests are stable — first-wins by iteration
		// order, which is the deterministic-listing order.
		if bestID == "" || stop.After(bestT) {
			bestID = m.BackupID
			bestT = stop
		}
	}
	if bestID != "" {
		return bestID, errored, nil
	}
	if seen == 0 && errored == 0 {
		return "", 0, ErrNoBackupsFound
	}
	if errored > 0 && seen == 0 {
		return "", errored, fmt.Errorf("restore: %d manifests for %q all failed verification",
			errored, deployment)
	}
	// Manifests exist but every one we could READ is too new
	// (StoppedAt > target). Carry the skipped count into the error: an
	// unreadable manifest might have been the seed the operator needed,
	// so "there is no backup old enough" is only true of what could be
	// read.
	return "", errored, &NoBackupBeforeTimeError{
		Deployment: deployment,
		Target:     target,
		LaterCount: laterCount,
		Skipped:    errored,
	}
}

// TimeTargetSkippedWarning renders the operator-facing sentence for a
// non-zero skipped count on a time-targeted resolve. Sibling of
// LatestSkippedWarning; kept in one place so the wording does not drift
// between the two resolve paths.
func TimeTargetSkippedWarning(deployment, chosen string, skipped int, target time.Time) string {
	return fmt.Sprintf("resolving a backup for %q at target %s skipped %d manifest(s) that could not be "+
		"verified or read; %q is the closest seed among the ones that COULD be read, and a skipped "+
		"manifest may have been a closer one — which would mean less WAL to replay. Run "+
		"`pg_hardstorage repo check` and, if it matters, name an explicit backup ID.",
		deployment, target.UTC().Format(time.RFC3339), skipped, chosen)
}

// NoBackupBeforeTimeError is the typed error returned when
// `restore --to <target>` finds backups for the deployment but
// none whose StoppedAt is at or before target. The caller
// (CLI) maps it to a structured `notfound.backup_before_time`
// error with a Suggestion that explains the constraint.
type NoBackupBeforeTimeError struct {
	Deployment string
	Target     time.Time
	LaterCount int // number of manifests too new to seed
	// Skipped counts manifests that could not be verified or read
	// during the walk. Non-zero means "no backup old enough" is a
	// statement about the manifests that were readable, not about
	// the deployment.
	Skipped int
}

// Error implements error.
func (e *NoBackupBeforeTimeError) Error() string {
	return fmt.Sprintf("restore: no backup for %q with stop_time at or before %s (%d manifests are too new)",
		e.Deployment, e.Target.Format(time.RFC3339), e.LaterCount)
}

// ErrNoBackupBeforeTime is the sentinel for errors.Is on
// NoBackupBeforeTimeError. Callers gate on this for the
// "target is older than every backup" case (typically a typo
// or a brand-new fleet without history).
var ErrNoBackupBeforeTime = errors.New("restore: no backup with stop_time before target")

// Is implements errors.Is so the typed error matches the sentinel.
func (e *NoBackupBeforeTimeError) Is(target error) bool {
	return target == ErrNoBackupBeforeTime
}

// FormatNoBackupBeforeTimeError wraps a NoBackupBeforeTimeError
// into a structured CLI output.Error. Used by the CLI restore
// path; kept in the restore package so the CLI doesn't have to
// reach in for the formatting.
func FormatNoBackupBeforeTimeError(err *NoBackupBeforeTimeError) error {
	human := fmt.Sprintf("the deployment has %d manifest(s), but every one was taken AFTER your --to target. PITR replays forward from a backup, so the seed must be older than the rewind point. Either pick an earlier --to (closer to a backup you have), or take a fresh backup right now and re-target it.",
		err.LaterCount)
	// Do not claim this about the deployment when part of it could not
	// be read: an unreadable manifest was never classified as earlier
	// or later, so it may well be the seed the operator is looking for.
	if err.Skipped > 0 {
		human = fmt.Sprintf("every manifest that could be READ was taken after your --to target, but %d more could not be verified or read — one of those may be the seed you need, so this is not yet a statement about the deployment. Run `pg_hardstorage repo check` first. If the archive really is all newer: PITR replays forward from a backup, so pick an earlier --to or take a fresh backup and re-target it.",
			err.Skipped)
	}
	suggestion := &output.Suggestion{Human: human}
	return output.NewError("notfound.backup_before_time",
		fmt.Sprintf("restore: no backup for %q with stop_time at or before %s",
			err.Deployment, err.Target.Format(time.RFC3339))).
		WithSuggestion(suggestion).
		Wrap(err)
}

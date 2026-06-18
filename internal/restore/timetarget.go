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
func ResolveBackupForTime(ctx context.Context, sp storage.StoragePlugin, deployment string, target time.Time, verifier *backup.Verifier) (string, error) {
	if target.IsZero() {
		return "", errors.New("restore: ResolveBackupForTime requires a non-zero target time")
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
		return bestID, nil
	}
	if seen == 0 && errored == 0 {
		return "", ErrNoBackupsFound
	}
	if errored > 0 && seen == 0 {
		return "", fmt.Errorf("restore: %d manifests for %q all failed verification",
			errored, deployment)
	}
	// Manifests exist but every one is too new (StoppedAt > target).
	return "", &NoBackupBeforeTimeError{
		Deployment: deployment,
		Target:     target,
		LaterCount: laterCount,
	}
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
	suggestion := &output.Suggestion{
		Human: fmt.Sprintf("the deployment has %d manifest(s), but every one was taken AFTER your --to target. PITR replays forward from a backup, so the seed must be older than the rewind point. Either pick an earlier --to (closer to a backup you have), or take a fresh backup right now and re-target it.",
			err.LaterCount),
	}
	return output.NewError("notfound.backup_before_time",
		fmt.Sprintf("restore: no backup for %q with stop_time at or before %s",
			err.Deployment, err.Target.Format(time.RFC3339))).
		WithSuggestion(suggestion).
		Wrap(err)
}

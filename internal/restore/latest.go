// latest.go — ResolveLatest: picks the most-recent verifiable backup for a deployment.
package restore

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// ResolveLatest returns the BackupID of the most recent successful
// backup for deployment, where "most recent" is the highest StoppedAt
// among manifests that pass signature verification.
//
// Iterates ManifestStore.List, ignoring per-entry verification errors
// (a corrupted manifest shouldn't keep the user from finding a newer
// good one). Returns ErrNoBackupsFound when zero verified manifests
// exist for the deployment.
//
// CPU note: this is O(N) over the deployment's manifests. For
// deployments with thousands of backups we'll want a top-level index;
// not yet — the GC slice introduces it.
func ResolveLatest(ctx context.Context, sp storage.StoragePlugin, deployment string, verifier *backup.Verifier) (string, error) {
	store := backup.NewManifestStore(sp)
	var (
		bestID  string
		bestT   string // canonical time string from the manifest
		seen    int
		errored int
	)
	for m, err := range store.List(ctx, deployment, verifier) {
		if err != nil {
			errored++
			continue
		}
		seen++
		// StoppedAt comes back as a time.Time; format consistently and
		// compare lexicographically — RFC 3339 sorts correctly.
		t := m.StoppedAt.UTC().Format("20060102T150405.000000000Z")
		if bestT == "" || t > bestT {
			bestT = t
			bestID = m.BackupID
		}
	}
	if bestID == "" {
		if seen == 0 && errored == 0 {
			return "", ErrNoBackupsFound
		}
		// All manifests we found errored on verification. Surface that
		// distinctly so the user knows it's a verification problem,
		// not a "no backups" problem.
		return "", fmt.Errorf("restore: %d manifests for %q all failed verification",
			errored, deployment)
	}
	return bestID, nil
}

// ErrNoBackupsFound is returned by ResolveLatest when zero verifiable
// backups exist for the named deployment.
var ErrNoBackupsFound = errors.New("restore: no backups found for deployment")

// FormatNoBackupsError returns a structured error suitable for
// surfacing to the user when ErrNoBackupsFound is hit.
func FormatNoBackupsError(deployment string) error {
	return output.NewError("notfound.backup",
		fmt.Sprintf("restore: no backups found for deployment %q", deployment)).
		WithSuggestion(&output.Suggestion{
			Human: "take a backup first with `pg_hardstorage backup " + deployment + "`",
		}).Wrap(ErrNoBackupsFound)
}

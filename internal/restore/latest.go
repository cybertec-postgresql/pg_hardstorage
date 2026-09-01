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
// Prefer ResolveLatestDetailed in any path that can tell the operator
// something: this form drops the skipped-manifest count, and a skipped
// manifest may have been NEWER than the one returned.
//
// Returns ErrNoBackupsFound when zero verified manifests exist.
func ResolveLatest(ctx context.Context, sp storage.StoragePlugin, deployment string, verifier *backup.Verifier) (string, error) {
	id, _, err := ResolveLatestDetailed(ctx, sp, deployment, verifier)
	return id, err
}

// ResolveLatestDetailed is ResolveLatest plus the number of manifests
// that could not be evaluated.
//
// Skipping unreadable manifests is deliberate and stays: one corrupt
// old manifest must not stop an operator finding a newer good backup,
// least of all during a recovery. What was missing is that the skip was
// SILENT.
//
// "Latest" is a question about an ordering, and the answer is derived
// only from the manifests that could be read. A manifest that fails
// signature verification or cannot be fetched yields no StoppedAt, so
// there is no way to know whether it was newer than the winner — which
// means a non-zero skipped count turns "this is the latest backup" into
// "this is the latest backup I could read". Those are different claims,
// and the operator asking to restore the latest is entitled to the
// difference: they can name an explicit backup ID instead.
//
// The count is what callers must surface. It cannot be narrowed to
// "only the ones that might have been newer" — establishing that would
// require reading the manifest that just failed to read.
//
// CPU note: this is O(N) over the deployment's manifests. For
// deployments with thousands of backups we'll want a top-level index;
// not yet — the GC slice introduces it.
func ResolveLatestDetailed(ctx context.Context, sp storage.StoragePlugin, deployment string, verifier *backup.Verifier) (string, int, error) {
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
			return "", 0, ErrNoBackupsFound
		}
		// All manifests we found errored on verification. Surface that
		// distinctly so the user knows it's a verification problem,
		// not a "no backups" problem.
		return "", errored, fmt.Errorf("restore: %d manifests for %q all failed verification",
			errored, deployment)
	}
	return bestID, errored, nil
}

// LatestSkippedWarning renders the operator-facing sentence for a
// non-zero skipped count. Callers differ in how they surface it (a
// Warning event, a progress body, an error suffix), but the wording
// should not — the operator meets the same phrasing wherever it
// appears.
func LatestSkippedWarning(deployment, chosen string, skipped int) string {
	return fmt.Sprintf("resolving the latest backup for %q skipped %d manifest(s) that could not be "+
		"verified or read; %q is the newest of the ones that COULD be read, and a skipped manifest "+
		"may have been newer. Run `pg_hardstorage repo check` and, if it matters, name an explicit "+
		"backup ID instead of \"latest\".", deployment, skipped, chosen)
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

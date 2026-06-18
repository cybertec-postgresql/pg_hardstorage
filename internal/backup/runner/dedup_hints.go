// dedup_hints.go — seeds CAS with prior-backup chunk hashes so re-backups skip unchanged writes.
package runner

// dedup_hints.go — seeds the CAS with the chunk hashes of a
// deployment's most recent prior backup so a re-backup of a mostly-
// unchanged database skips re-compressing and re-uploading the
// unchanged chunks. See repo.WithDedupHints for how the CAS consumes
// the set, and the "Durability modes" / dedup-hint changelog entry
// for the rationale.

import (
	"context"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// loadDedupHints returns the set of plaintext chunk hashes referenced
// by the deployment's most recent prior backup manifest, for use as
// CAS dedup hints (see repo.WithDedupHints).
//
// "Most recent" is decided by the manifest object's ModTime, so the
// LISTING alone picks the winner and only ONE manifest is read +
// parsed. The returned set's size scales with the database size (its
// chunk count), not with the repo size — bounded and predictable.
//
// Best-effort by contract:
//   - (nil, nil)  — the deployment has no prior backup (a first
//     backup): the caller proceeds with no hints, i.e. unchanged
//     behaviour.
//   - (nil, err)  — a hard backend error: the caller surfaces it as a
//     warning event and proceeds WITHOUT hints. Hint loading must
//     never fail a backup; the hints only ever save work.
//
// Cross-deployment dedup (a chunk already in the repo via a different
// deployment's backup) is intentionally NOT covered here — those
// chunks still pay the full write path, exactly as before, so this is
// never a regression. A repo-wide bloom filter could extend coverage;
// it is left as a future enhancement.
func loadDedupHints(ctx context.Context, sp storage.StoragePlugin, deployment string) (map[repo.Hash]struct{}, error) {
	prefix := "manifests/" + deployment + "/"
	var (
		latestKey string
		latestMod time.Time
	)
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		if !looksLikePrimaryManifest(info.Key) {
			continue
		}
		// ModTime is the manifest commit time — a faithful proxy for
		// "newest backup". When a backend reports a zero ModTime the
		// first candidate simply wins; still a valid prior manifest.
		if latestKey == "" || info.ModTime.After(latestMod) {
			latestKey, latestMod = info.Key, info.ModTime
		}
	}
	if latestKey == "" {
		return nil, nil // first backup for this deployment
	}

	m, ok, err := readManifestNoVerify(ctx, sp, latestKey)
	if err != nil {
		return nil, err
	}
	if !ok || m == nil {
		return nil, nil
	}

	hints := make(map[repo.Hash]struct{})
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			hints[c.Hash] = struct{}{}
		}
	}
	return hints, nil
}

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
	"errors"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
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
//
// The set is returned as a Bloom filter plus the number of hashes it
// holds: a database with billions of chunks has a hint set of hundreds
// of GB as a map, and the hints are advisory (a false positive costs one
// Stat probe that falls through to the normal write path), so ~1.25 bytes
// per chunk is the right trade. The previous manifest is streamed twice —
// once to size the filter, once to fill it — and never held.
func loadDedupHints(ctx context.Context, sp storage.StoragePlugin, deployment string) (*spill.Bloom, int64, error) {
	prefix := "manifests/" + deployment + "/"
	var (
		latestKey string
		latestMod time.Time
	)
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return nil, 0, err
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
		return nil, 0, nil // first backup for this deployment
	}

	// Pass 1: count chunk references to size the filter.
	rc, err := sp.Get(ctx, latestKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	info, err := backup.StreamManifest(rc, backup.StreamOptions{}, nil)
	_ = rc.Close()
	if err != nil {
		return nil, 0, err
	}
	if info.ChunkRefs == 0 {
		return nil, 0, nil
	}
	// Pass 2: fill it. A manifest that changed between the passes only
	// makes the filter a little too small or too large — still advisory.
	rc2, err := sp.Get(ctx, latestKey)
	if err != nil {
		return nil, 0, err
	}
	defer rc2.Close()
	filter := spill.NewBloom(info.ChunkRefs)
	var n int64
	if _, err := backup.StreamManifest(rc2, backup.StreamOptions{}, func(f *backup.FileEntry) error {
		for _, c := range f.Chunks {
			filter.Add(c.Hash)
			n++
		}
		return ctx.Err()
	}); err != nil {
		return nil, 0, err
	}
	return filter, n, nil
}

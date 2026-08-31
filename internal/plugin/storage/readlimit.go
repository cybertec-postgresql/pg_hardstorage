// readlimit.go — bounded reads of repository objects.
//
// Why this lives here
// -------------------
// The project already refuses unbounded io.ReadAll on objects it pulls
// out of a repository, and says why: a manifest declaring a huge
// files/chunks array, a corrupt object, or a file someone dropped into
// the bucket by hand would otherwise OOM the reader before any
// validation runs. internal/backup states it for manifests
// (MaxManifestBytes, "input-validation audit #2"), internal/repo's CAS
// states it for chunk envelopes (MaxChunkEnvelopeBytes, "audit #3"),
// and internal/pg/basebackup states it for the streamed manifest.
//
// internal/repo, internal/wal/timeline and internal/wal/gapstate never
// got the treatment — not by decision, but because internal/backup
// imports internal/repo, so they cannot reach backup.ReadAllLimited
// without a cycle. Those are the packages that walk EVERY object in a
// repository: reference collection for gc, replication, bundle
// export/import, WAL pruning. A single oversized object under
// manifests/ — the `repair index` docs call out a misdirected
// `aws s3 cp` as a real way that happens — took the whole sweep down.
//
// internal/plugin/storage is below all of them, so the helper lives
// here and every repository reader can share one posture.
package storage

import (
	"fmt"
	"io"
)

// MaxMetadataBytes caps a single read of a repository METADATA object:
// manifests, tombstones, HSREPO, WAL segment manifests, timeline
// history, gap records, wrapped-DEK objects.
//
// 1 GiB matches backup.MaxManifestBytes, which is the project's
// established ceiling for the largest of these (a backup manifest for a
// very large cluster carries millions of chunk references). It is far
// above any real object while bounding the blast radius of a corrupt or
// planted one. Chunk BODIES are not metadata and keep their own,
// separate cap — see repo.MaxChunkEnvelopeBytes.
const MaxMetadataBytes = 1 << 30

// ReadAllLimited reads up to max bytes from r, returning an error
// rather than allocating unboundedly when the source exceeds it.
//
// It reads max+1 bytes so exceeding the limit is detected rather than
// silently truncated — a truncated read would be worse than a refusal,
// since the caller would then parse a prefix of the object and act on
// whatever it happened to contain.
func ReadAllLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("storage: object exceeds the %d-byte read limit "+
			"(refusing to load an oversized or malformed object)", max)
	}
	return body, nil
}

// gc.go — CollectReferences: tombstone-grace-aware mark-and-sweep of unreferenced chunks.
package repo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// DefaultTombstoneGracePeriod is the conservative default applied
// by CollectReferences when its options ask for the default grace.
// without a grace period, an `apply` GC running
// between SoftDelete and Undelete permanently deletes chunks an
// operator can no longer recover.  Twenty-four hours covers the
// "operator deleted by mistake, noticed in their morning standup,
// undeleted same day" common case.  Operators choose stricter
// retention via CollectReferencesOptions.TombstoneGrace.
const DefaultTombstoneGracePeriod = 24 * time.Hour

// CollectReferencesOptions controls how tombstoned manifests are
// treated when computing the live reference set.  The zero value
// uses DefaultTombstoneGracePeriod and time.Now as the clock.
type CollectReferencesOptions struct {
	// TombstoneGrace is the minimum age a tombstone marker must
	// reach before its manifest's chunks become GC candidates.
	// A young tombstone (mtime > now - TombstoneGrace) is treated
	// as if the manifest were still live: its chunks remain in
	// the reference set, so an Undelete within the grace window
	// recovers a fully-restorable backup.
	//
	// Zero means use DefaultTombstoneGracePeriod.  A negative
	// value disables the grace (treats every tombstone as
	// immediately eligible) — only useful for forensic / scrub
	// runs that explicitly want to see the orphans.
	TombstoneGrace time.Duration

	// Now is the clock used for grace-period comparisons.  Zero
	// means time.Now (real wallclock).  Tests inject a fixed
	// instant for deterministic behaviour.
	Now time.Time

	// RefSpill tunes the returned RefSet's heap budget and spill dir.
	RefSpill spill.Options
}

func (o CollectReferencesOptions) effectiveGrace() time.Duration {
	if o.TombstoneGrace == 0 {
		return DefaultTombstoneGracePeriod
	}
	if o.TombstoneGrace < 0 {
		return 0
	}
	return o.TombstoneGrace
}

func (o CollectReferencesOptions) effectiveNow() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now.UTC()
}

// RefSet collects every chunk hash referenced by visible manifests
// (backup + WAL segment + logical-stream segment). The output drives
// the GC's orphan-finder and the scrub's "is this chunk still
// referenced" check.
//
// Implementation note: we walk the repo's THREE manifest prefixes
// (`manifests/`, `wal/`, and `logical/`) without parsing per-deployment
// shape — a chunk is referenced iff its hash appears in any committed
// manifest's chunks list, regardless of which kind. Every writer that
// puts chunks in the shared CAS MUST have its manifests reachable from
// one of these prefixes, or GC will reap its chunks as orphans.
type RefSet struct {
	set *spill.HashSet
}

// NewRefSet returns an empty RefSet that spills to the system temp dir
// once it outgrows spill.DefaultMemEntries hashes.
func NewRefSet() *RefSet {
	return NewRefSetWithOptions(spill.Options{})
}

// NewRefSetWithOptions returns an empty RefSet with an explicit heap
// budget and spill directory. A repository whose backups reference
// billions of chunks needs far more than RAM to hold every hash, so the
// set is a spill.HashSet: exact, but bounded on the heap.
func NewRefSetWithOptions(o spill.Options) *RefSet {
	return &RefSet{set: spill.NewHashSet(o)}
}

// Add records hash as referenced.
func (r *RefSet) Add(h Hash) { r.set.Add(h) }

// Has reports whether hash is in the set. Callers that act on a
// negative answer (GC deletes) MUST check Err first: after a spill I/O
// failure a negative answer is not trustworthy.
func (r *RefSet) Has(h Hash) bool { return r.set.Has(h) }

// Len returns the number of distinct references.
func (r *RefSet) Len() int { return r.set.Len() }

// Err returns the first spill I/O error, or nil.
func (r *RefSet) Err() error { return r.set.Err() }

// Close removes any spill files. Safe to call more than once.
func (r *RefSet) Close() error { return r.set.Close() }

// ForEach visits every reference in ascending hash order.
func (r *RefSet) ForEach(fn func(Hash) error) error {
	return r.set.ForEach(func(k spill.Key) error { return fn(Hash(k)) })
}

// CollectReferences walks every visible manifest in the repo and
// returns the union of chunk hashes they reference.  Tombstoned
// manifests OLDER than DefaultTombstoneGracePeriod are SKIPPED —
// chunks only referenced by such manifests are GC candidates.
// Younger tombstones still count as live (their chunks stay in
// the reference set) so an Undelete within the grace window
// recovers a fully-restorable backup. .
//
// CollectReferencesWithOptions exposes the underlying knobs.
func CollectReferences(ctx context.Context, sp storage.StoragePlugin) (*RefSet, error) {
	return CollectReferencesWithOptions(ctx, sp, CollectReferencesOptions{})
}

// CollectReferencesWithOptions is the variant that accepts a
// custom tombstone-grace window and clock.  Passing a negative
// TombstoneGrace disables the grace entirely (every tombstone
// becomes immediately GC-eligible) — operators only want this for
// forensics / scrub runs that explicitly expect to see orphans.
//
// We deliberately don't decode manifests via the typed `backup` /
// `walsink` packages here: doing so would create import cycles and
// double the work for what's a simple "find every `hash:` field"
// problem. Instead we JSON-decode into a shallow shape that's
// stable across both manifest schemas — `{"chunks": [{"hash":...}],
// "files": [{"chunks":[{"hash":...}]}]}`.
//
// The two shapes are: backup manifests have `files[].chunks[].hash`,
// WAL segment manifests have `chunks[].hash`. We walk both.
func CollectReferencesWithOptions(ctx context.Context, sp storage.StoragePlugin, opts CollectReferencesOptions) (*RefSet, error) {
	// One List per prefix (manifests/ used to be listed twice — once for
	// tombstones, once for manifests), concurrent manifest reads. The
	// walk itself lives in manifestSnapshot.scan (gcsweep.go) so gc's
	// incremental rescans and this one-shot collection cannot drift.
	//
	// Tombstones: keyed on DEPLOYMENT + backup id, from
	// manifests/<dep>/backups/<id>/manifest.json.tombstone. The id alone
	// is not a safe key — nothing relates a manifest's BackupID to its
	// Deployment, so an id-only key let a tombstone in one deployment
	// suppress a LIVE manifest with the same id in another, and gc would
	// delete the data of a backup nobody deleted. A tombstone inside the
	// grace window (or with an unknown ModTime) keeps its manifest live.
	//
	// WAL (wal/<dep>/<TLI>/<seg>.json) and logical-stream
	// (logical/<dep>/<stream>/<lsn>.json) segment manifests are walked
	// too: both write their chunks into the SAME shared CAS, and a prefix
	// left out of this walk would have its chunks reaped as orphans.
	// Staging temps are skipped by BASENAME only (isStaleTempKey): a
	// committed segment under a deployment whose name contains
	// ".json.tmp." must still be harvested.
	snap := &manifestSnapshot{refs: NewRefSetWithOptions(opts.RefSpill), harvested: map[string]storage.ObjectInfo{}, opts: opts}
	if err := snap.scan(ctx, sp, false); err != nil {
		snap.refs.Close()
		return nil, err
	}
	if err := snap.refs.Err(); err != nil {
		snap.refs.Close()
		return nil, err
	}
	return snap.refs, nil
}

type harvestKind int

const (
	harvestBackup harvestKind = iota
	harvestWAL
)

// chunkRef is a partial-decode view of a chunk reference. Stable
// across both backup and walsink manifest schemas — both spell the
// hex-encoded SHA-256 as `"hash"`.
type chunkRef struct {
	Hash string `json:"hash"`
}

// backupManifestShape is the partial decode for a backup manifest.
// We only care about files[].chunks[].hash.
type backupManifestShape struct {
	Files []struct {
		Chunks []chunkRef `json:"chunks"`
	} `json:"files"`
}

// walManifestShape is the partial decode for a WAL segment manifest.
type walManifestShape struct {
	Chunks []chunkRef `json:"chunks"`
}

// harvestManifest reads key and adds every chunk-hash reference to refs.
func harvestManifest(ctx context.Context, sp storage.StoragePlugin, key string, refs *RefSet, kind harvestKind) error {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		// Deleted between our List and this Get — `wal prune`,
		// retention or `backup delete` running beside gc. A manifest
		// that no longer exists references nothing, so its absence is
		// the correct contribution to the live set. Aborting instead
		// failed every gc that overlapped a prune, which on a busy
		// shared repository meant gc never completed. Only ErrNotFound
		// is skipped: any other read failure still aborts, because an
		// unread manifest that DOES exist would under-count references.
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	defer rc.Close()
	if kind == harvestBackup {
		// Streamed: a 200 TB database's manifest is hundreds of GB, so it
		// is walked one chunk reference at a time, never held in memory.
		return streamBackupChunkRefs(rc, func(hash string) error {
			h, err := parseHexHash(hash)
			if err != nil {
				return unparseableRef(key, hash, err)
			}
			refs.Add(h)
			return nil
		})
	}
	body, err := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
	if err != nil {
		return err
	}
	switch kind {
	case harvestWAL:
		var m walManifestShape
		if err := json.Unmarshal(body, &m); err != nil {
			return err
		}
		for _, c := range m.Chunks {
			h, err := parseHexHash(c.Hash)
			if err != nil {
				return unparseableRef(key, c.Hash, err)
			}
			refs.Add(h)
		}
	}
	return nil
}

// unparseableRef fails reference collection CLOSED when a live manifest
// names a chunk hash GC can't parse. Skipping it would silently drop the
// chunk from the live set, so a later GC would delete a still-referenced
// chunk and render that backup unrestorable (poor-error-handling /
// data-loss audit #1). GC must refuse rather than under-count references.
func unparseableRef(key, hash string, err error) error {
	return fmt.Errorf("gc: manifest %q references an unparseable chunk hash %q: %w (refusing to collect references — a GC run from a partial set could delete live chunks)", key, hash, err)
}

// parseHexHash decodes a 64-char lowercase hex string into a Hash.
func parseHexHash(s string) (Hash, error) {
	if len(s) != 64 {
		return Hash{}, ErrNotAChunkKey // re-using the same sentinel
	}
	var out Hash
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(s[2*i])
		lo, ok2 := hexNibble(s[2*i+1])
		if !ok1 || !ok2 {
			return Hash{}, ErrNotAChunkKey
		}
		out[i] = byte(hi<<4) | byte(lo)
	}
	return out, nil
}

func hexNibble(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// DefaultOrphanMinAge is the minimum age an unreferenced chunk must
// reach before a guarded FindOrphans call will flag it for deletion.
//
// It closes the GC-vs-backup race.  A backup writes its chunks (made
// crash-durable by cas.Barrier) BEFORE it commits the manifest that
// references them — so between those two steps the chunks are
// unreferenced-but-live.  A `repo gc --apply` (or `repair chunks
// --orphans --apply`) whose CollectReferences snapshot was taken
// before that commit would otherwise reap exactly those chunks,
// leaving the about-to-be-committed manifest pointing at deleted data.
// 24h comfortably exceeds any plausible single backup and mirrors
// DefaultTombstoneGracePeriod; operators with a quiesced repo can opt
// out via FindOrphansOptions.MinAge<0.
const DefaultOrphanMinAge = 24 * time.Hour

// FindOrphansOptions tunes FindOrphansWithOptions.  The zero value
// applies DefaultOrphanMinAge with time.Now as the clock.
type FindOrphansOptions struct {
	// MinAge is the minimum ModTime age an unreferenced chunk must
	// reach before it is eligible to be flagged an orphan.  A chunk
	// younger than this — or one whose ModTime the backend does not
	// expose (zero value) — is conservatively KEPT, so a chunk a
	// concurrent backup has written but not yet referenced in a
	// committed manifest is never reaped out from under it.
	//
	// Zero means DefaultOrphanMinAge.  A negative value disables the
	// floor entirely (every unreferenced chunk is an orphan,
	// regardless of age) — only safe on a quiesced repo, e.g. a
	// forensic/scrub run.
	MinAge time.Duration

	// Now is the clock used for the age comparison.  Zero means
	// time.Now (real wallclock).  Tests inject a fixed instant.
	Now time.Time
}

func (o FindOrphansOptions) effectiveMinAge() time.Duration {
	if o.MinAge == 0 {
		return DefaultOrphanMinAge
	}
	if o.MinAge < 0 {
		return 0
	}
	return o.MinAge
}

func (o FindOrphansOptions) effectiveNow() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now.UTC()
}

// FindOrphans is the RAW unreferenced-chunk finder: it returns every
// chunk whose hash is not in refs, with NO age floor.  The slice is
// sorted (lex by hex hash) for deterministic output.
//
// Destructive callers (gc --apply, repair chunks --orphans --apply)
// MUST instead use FindOrphansWithOptions so a chunk an in-flight
// backup has written but not yet committed in a manifest is never
// reaped.  This primitive exists for forensic / scrub callers that
// want the complete orphan set on a quiesced repo, and for the
// reference-logic unit tests.
func FindOrphans(ctx context.Context, sp storage.StoragePlugin, refs *RefSet) ([]Hash, error) {
	return FindOrphansWithOptions(ctx, sp, refs, FindOrphansOptions{MinAge: -1})
}

// FindOrphansWithOptions scans every chunks/sha256/... key and returns
// those whose hash is NOT in refs AND that are older than the option's
// effective MinAge.  See FindOrphansOptions for the age-floor
// rationale (the GC-vs-backup race).  The slice is sorted (lex by hex
// hash) for deterministic output.
func FindOrphansWithOptions(ctx context.Context, sp storage.StoragePlugin, refs *RefSet, opts FindOrphansOptions) ([]Hash, error) {
	minAge := opts.effectiveMinAge()
	var ageCutoff time.Time
	if minAge > 0 {
		ageCutoff = opts.effectiveNow().Add(-minAge)
	}
	var orphans []Hash
	for info, err := range sp.List(ctx, "chunks/sha256/") {
		if err != nil {
			return nil, err
		}
		// Same cooperative-cancellation rationale as
		// CollectReferences: a million-chunk walk shouldn't be
		// uninterruptible from the operator's keyboard.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash, err := ParseChunkKey(info.Key)
		if err != nil {
			continue
		}
		if refs.Has(hash) {
			continue
		}
		// Age floor: keep chunks younger than minAge (or with an
		// unknown ModTime) so an in-flight backup's not-yet-referenced
		// chunks survive a concurrent sweep.  Mirrors the zero-ModTime
		// conservatism of the tombstone-grace path.
		if minAge > 0 {
			if info.ModTime.IsZero() || info.ModTime.After(ageCutoff) {
				continue
			}
		}
		orphans = append(orphans, hash)
	}
	sortHashes(orphans)
	return orphans, nil
}

// FindStaleTempManifests returns the keys of orphaned staging files
// (`*.json.tmp.<rand>` manifest temps and `*.history.tmp.<rand>` timeline
// temps) under manifests/ and wal/.  A
// commit writes its body to such a tmp key and then atomically renames
// it onto the real key; if the process dies in between (or a rename
// error's best-effort cleanup never runs), the tmp file is left
// behind.  It is never referenced and never becomes the committed
// object, so it is pure dead weight that no chunk sweep would ever
// reclaim (FindOrphans only walks chunks/).
//
// As with chunk orphans, the sweep must not race a commit that is
// mid-flight between its tmp Put and rename, so only tmp files older
// than the option's effective MinAge are returned; a tmp file whose
// ModTime the backend doesn't expose is conservatively kept.  Keys are
// sorted for deterministic output.
func FindStaleTempManifests(ctx context.Context, sp storage.StoragePlugin, opts FindOrphansOptions) ([]string, error) {
	minAge := opts.effectiveMinAge()
	var ageCutoff time.Time
	if minAge > 0 {
		ageCutoff = opts.effectiveNow().Add(-minAge)
	}
	var stale []string
	for _, prefix := range []string{"manifests/", "wal/"} {
		for info, err := range sp.List(ctx, prefix) {
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// Manifest staging files (`*.json.tmp.<rand>`) and timeline
			// history staging files (`*.history.tmp.<rand>`, written by
			// timeline.Store) are both orphaned commit temps a crash can
			// strand. Reap either — but ONLY when the staging marker is in
			// the key's basename, never a parent path component. See
			// isStaleTempKey (own file for the mutation guard).
			if !isStaleTempKey(info.Key) {
				continue
			}
			if minAge > 0 {
				if info.ModTime.IsZero() || info.ModTime.After(ageCutoff) {
					continue
				}
			}
			stale = append(stale, info.Key)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// FindMissing scans every reference in refs and returns the hashes
// the storage backend says aren't present. This catches the "manifest
// refers to a chunk that's been deleted out from under it" case —
// rare, but a real scenario after a buggy restore plugin or a
// misdirected `aws s3 rm`.
func FindMissing(ctx context.Context, sp storage.StoragePlugin, refs *RefSet) ([]Hash, error) {
	var missing []Hash
	err := refs.ForEach(func(h Hash) error {
		// Cooperative cancellation. Same rationale as
		// CollectReferences/FindOrphans: a million-chunk Stat loop
		// shouldn't be uninterruptible.
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := sp.Stat(ctx, ChunkKey(h))
		if err != nil {
			// Only a definitive "not present" counts as missing.
			// Transient/backend errors (throttling, network,
			// permission) must abort rather than falsely reporting
			// the backup as damaged — otherwise a blip during a scan
			// would flag healthy chunks and trigger needless alarm.
			if errors.Is(err, storage.ErrNotFound) {
				missing = append(missing, h)
				return nil
			}
			return fmt.Errorf("repo: FindMissing: stat %s: %w", ChunkKey(h), err)
		}
		return nil
	})
	return missing, err
}

// ScrubResult records the outcome of a SHA-256 spot-check across
// chunks. mismatch is the count of chunks whose on-disk hash
// disagreed with their key. Bytes is the total bytes read.
type ScrubResult struct {
	Sampled    int
	OK         int
	Mismatches []Hash
	Bytes      int64
}

// Scrub samples up to limit chunks, fetches each, and verifies the
// PLAINTEXT SHA-256 matches the key (via the CAS read path which
// already does the verify). If a chunk fails verification the hash
// is added to ScrubResult.Mismatches.
//
// limit=0 means "every chunk" — a full scrub. Realistic for small
// repos; for repos with millions of chunks the operator picks a
// sample size matching their scrub-budget.
func Scrub(ctx context.Context, cas *CAS, refs *RefSet, limit int) (ScrubResult, error) {
	res := ScrubResult{}

	var stop = errors.New("scrub: limit reached")
	err := refs.ForEach(func(h Hash) error {
		if limit > 0 && res.Sampled >= limit {
			return stop
		}
		// Cooperative cancellation. Scrub re-reads + re-hashes every
		// chunk; on a million-chunk repo that's hours of work. A
		// cancelled ctx mid-loop should bail without continuing to
		// hash; we surface what we measured so far via the partial
		// ScrubResult so the operator sees it as a clean abort, not
		// a verification finding. (cas.GetChunkBytes propagates ctx
		// too, but we want the early bail in the CPU-heavy path.)
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := cas.GetChunkBytes(ctx, h)
		if err != nil {
			// Distinguish ctx-cancelled from a real fetch failure —
			// the former is not a "mismatch."
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			res.Mismatches = append(res.Mismatches, h)
			res.Sampled++
			return nil
		}
		// CAS.GetChunkBytes already verifies plaintext SHA. We
		// re-verify here to guard against any future wrapping that
		// might silently bypass that check.
		if Hash(sha256.Sum256(body)) != h {
			res.Mismatches = append(res.Mismatches, h)
			res.Sampled++
			return nil
		}
		res.OK++
		res.Sampled++
		res.Bytes += int64(len(body))
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return res, err
	}
	return res, nil
}

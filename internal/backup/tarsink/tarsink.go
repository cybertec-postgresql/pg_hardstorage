// Package tarsink implements basebackup.Sink: it consumes the raw tar
// byte stream PG sends per tablespace, parses tar entries, runs each
// regular file's bytes through the chunker into the CAS, and
// accumulates a list of backup.FileEntry values ready for manifest
// assembly.
//
// Architecture:
//
//	BASE_BACKUP (PG) -- CopyData --> Sink.OnTablespaceData
//	                                     |
//	                                  io.Pipe
//	                                     v
//	                              tar parser goroutine
//	                                     |
//	                          tar.Next + tar.Read
//	                                     |
//	                          chunker.Iter(tarReader)
//	                                     |
//	                            cas.PutChunk per chunk
//	                                     |
//	                              FileEntry assembly
//
// io.Pipe gives natural backpressure: the network read in BASE_BACKUP
// blocks until the chunker pulls bytes through. No unbounded buffering.
//
// Special handling for two well-known files in the FIRST tablespace's
// tar: `backup_label` and `tablespace_map`. PG produces them via
// pg_backup_start / pg_backup_stop and embeds them in the tar; we
// intercept their bodies for the manifest. They are NOT chunked into
// CAS — they're tiny and live in the manifest itself.
//
// Manifest CopyOut (idx == basebackup.ManifestSinkIndex) is NOT a tar
// stream; it's the raw bytes of PG's own backup manifest. We collect
// them verbatim into ManifestBytes for the orchestrator's later use.
//
// Goroutine safety: each Sink instance is single-consumer (driven by
// basebackup.Run). The internal parser goroutine runs concurrently with
// OnTablespaceData calls; OnTablespaceEnd waits for the parser to drain.
// Sink methods are NOT safe to call concurrently from multiple
// goroutines.
package tarsink

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Special file names that PG embeds in the first tablespace's tar.
// The first is the backup label (the bytes from pg_backup_start);
// the second is the tablespace map (only present when there are
// non-default tablespaces).
const (
	BackupLabelName   = "backup_label"
	TablespaceMapName = "tablespace_map"
)

// Sink is a basebackup.Sink that funnels tar streams through a CAS.
//
// Construction takes ownership of the *repo.CAS and the supplied ctx.
// Cancelling ctx aborts in-flight tar parsing and chunk uploads via
// the standard error-propagation paths.
type Sink struct {
	ctx context.Context
	cas *repo.CAS

	// chunkerFactory builds a fresh chunker per file. Defaults to
	// chunker.New (4 KiB / 64 KiB / 256 KiB). Tests can override to
	// exercise tighter / looser bounds without rewiring the API.
	chunkerFactory func() *chunker.Chunker

	// chunkConcurrency bounds how many of a single file's chunks are
	// hashed + compressed + written in parallel. Default
	// min(GOMAXPROCS, 8); 1 is the legacy serial path. See chunkFile.
	chunkConcurrency int

	// fileObserver is the per-file progress callback (issue #9).
	// nil means no observer is wired — the parse path skips the
	// callback entirely.
	fileObserver FileObserver

	// Per-tablespace state populated as the basebackup.Sink callbacks
	// fire. files[idx] is the slice of FileEntries assembled for that
	// tablespace; ManifestSinkIndex (-1) is reserved for the PG manifest.
	files map[int][]backup.FileEntry

	// dirs[idx] is the slice of DirEntries observed in tablespace
	// idx's tar stream.  PG sends every PGDATA subdirectory as a
	// tar.TypeDir entry; capturing them explicitly is what makes
	// EMPTY dirs (pg_wal/, pg_dynshmem/, pg_notify/, etc.) survive
	// the round-trip.  See backup.DirEntry for the regression
	// history.
	dirs map[int][]backup.DirEntry

	// tsOID[idx] is the tablespace OID PG reported for tablespace
	// idx in the BASE_BACKUP header (TablespaceInfo.OID).  The
	// DEFAULT tablespace has OID 0 and its tar entries are relative
	// to PGDATA root; a NON-DEFAULT tablespace has a non-zero OID
	// and its tar entries are relative to the tablespace root.  We
	// stamp this OID onto every FileEntry/DirEntry parsed from
	// tablespace idx so restore can materialise non-default-
	// tablespace files under their real location (from
	// tablespace_map) instead of flattening them under PGDATA root.
	tsOID map[int]uint32

	// Special-file bytes captured from the first tablespace's tar.
	backupLabel   []byte
	tablespaceMap []byte
	manifestBytes []byte

	// Active goroutine state — set in OnTablespaceStart, cleared in
	// OnTablespaceEnd. Only meaningful while a tablespace is in flight.
	currentIdx int
	pipeWriter *io.PipeWriter
	parserDone chan parserResult

	// manifestBuf accumulates raw bytes when currentIdx == ManifestSinkIndex.
	manifestBuf bytes.Buffer

	mu sync.Mutex // protects files / *bytes / per-tablespace state across the goroutines
}

// parserResult carries the outcome of the tar-parsing goroutine back
// to OnTablespaceEnd.
type parserResult struct {
	files []backup.FileEntry
	dirs  []backup.DirEntry
	err   error
}

// Option tunes a Sink at construction time.
type Option func(*Sink)

// WithChunkerFactory replaces the default chunker.New with f. Each
// tablespace file is chunked using a fresh chunker; the factory is
// called once per file.
func WithChunkerFactory(f func() *chunker.Chunker) Option {
	return func(s *Sink) { s.chunkerFactory = f }
}

// WithChunkConcurrency caps how many of a single file's chunks are
// hashed + compressed + written in parallel. Default
// min(GOMAXPROCS, 8). A value < 1 clamps to 1 — the legacy serial
// path, useful for deterministic tests.
func WithChunkConcurrency(n int) Option {
	return func(s *Sink) {
		if n < 1 {
			n = 1
		}
		s.chunkConcurrency = n
	}
}

// defaultChunkConcurrency picks the per-file chunk-write parallelism
// when WithChunkConcurrency is not set: min(GOMAXPROCS, 8). The cap
// keeps a backup from monopolising every core on a busy host while
// still overlapping the compress/hash/write of many chunks.
func defaultChunkConcurrency() int {
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}

// FileStats summarises a single regular file's pass through the
// chunker.  Reported via FileObserver as soon as the file's last
// chunk lands in the CAS, so a subscriber (the backup runner /
// `--verbose` renderer) can stream per-file progress without
// waiting for the manifest to commit.  Issue #9.
//
//   - Path           — file path inside the tablespace tar
//     (matches FileEntry.Path).
//   - Size           — logical bytes (== sum of ChunkRef.Len).
//   - ChunkCount     — total chunk references emitted for this file.
//   - DedupedChunks  — count of chunks PutChunk reported as already
//     present (other backups, other files in this run, or the same
//     file at an earlier offset).
//   - UniqueBytes    — bytes the CAS actually had to store for this
//     file (== sum of non-deduped chunk lengths).  Always <= Size;
//     ratio Size/UniqueBytes is the file's local dedup factor.
type FileStats struct {
	Path          string
	Size          int64
	ChunkCount    int
	DedupedChunks int
	UniqueBytes   int64
}

// FileObserver fires once per regular file once chunkFile has
// committed every chunk to the CAS.  Optional; nil discards events.
// Callbacks must NOT block — they run on the tar-parsing
// goroutine and back-pressure the basebackup pipe.
type FileObserver func(FileStats)

// WithFileObserver registers an observer that fires per regular
// file as the tar parse progresses.  Backs the `--verbose` flag
// on `pg_hardstorage backup`.  Issue #9.
func WithFileObserver(f FileObserver) Option {
	return func(s *Sink) { s.fileObserver = f }
}

// New returns a Sink. The ctx flows into the tar-parsing goroutines and
// into cas.PutChunk; cancelling it aborts everything in flight.
func New(ctx context.Context, cas *repo.CAS, opts ...Option) *Sink {
	if ctx == nil {
		panic("tarsink: nil context")
	}
	if cas == nil {
		panic("tarsink: nil CAS")
	}
	s := &Sink{
		ctx:              ctx,
		cas:              cas,
		chunkerFactory:   chunker.New,
		chunkConcurrency: defaultChunkConcurrency(),
		files:            map[int][]backup.FileEntry{},
		dirs:             map[int][]backup.DirEntry{},
		tsOID:            map[int]uint32{},
		currentIdx:       unsetIdx,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// unsetIdx marks "no tablespace currently in flight". Distinct from
// ManifestSinkIndex (-1) and from any real tablespace index (>= 0).
const unsetIdx = -2

// Files returns the FileEntry slice accumulated for tablespace idx, or
// nil if that tablespace was never seen.
func (s *Sink) Files(idx int) []backup.FileEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]backup.FileEntry, len(s.files[idx]))
	copy(out, s.files[idx])
	return out
}

// AllDirs returns every DirEntry across all tablespaces in the
// order PG emitted them.  The orchestrator uses this when
// assembling the final backup.Manifest so the restore step
// can MkdirAll each captured directory and bring even empty
// ones (pg_wal/, pg_dynshmem/, ...) back from a backup —
// without this, PG refuses to start on the restored datadir.
func (s *Sink) AllDirs() []backup.DirEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]int, 0, len(s.dirs))
	for k := range s.dirs {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var out []backup.DirEntry
	for _, k := range keys {
		out = append(out, s.dirs[k]...)
	}
	return out
}

// AllFiles returns every FileEntry across all tablespaces in the order
// PG emitted them. The orchestrator uses this when assembling the
// final backup.Manifest.
func (s *Sink) AllFiles() []backup.FileEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []backup.FileEntry
	// Walk by index in ascending order so the manifest is deterministic.
	// Manifest CopyOut (-1) is excluded; it's not file content.
	max := -1
	for k := range s.files {
		if k > max {
			max = k
		}
	}
	for i := 0; i <= max; i++ {
		out = append(out, s.files[i]...)
	}
	return out
}

// BackupLabel returns the bytes of the backup_label file extracted from
// the first tablespace's tar, or nil if it was never seen. PG always
// emits this; an empty result on a successful run indicates a bug.
func (s *Sink) BackupLabel() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.backupLabel))
	copy(out, s.backupLabel)
	return out
}

// TablespaceMap returns the bytes of the tablespace_map file. Empty
// when no non-default tablespaces are present (PG omits the file).
func (s *Sink) TablespaceMap() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.tablespaceMap))
	copy(out, s.tablespaceMap)
	return out
}

// ManifestBytes returns the raw PG-emitted backup manifest. Empty when
// BASE_BACKUP was issued without MANIFEST 'yes'.
func (s *Sink) ManifestBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.manifestBytes))
	copy(out, s.manifestBytes)
	return out
}

// OnTablespaceStart implements basebackup.Sink. For real tablespaces
// (idx >= 0) it spawns the tar-parsing goroutine and opens the io.Pipe.
// For the manifest CopyOut (idx == ManifestSinkIndex) it switches to
// raw-byte accumulation.
func (s *Sink) OnTablespaceStart(idx int, info basebackup.TablespaceInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentIdx != unsetIdx {
		return fmt.Errorf("tarsink: OnTablespaceStart(%d) while %d is still active", idx, s.currentIdx)
	}
	s.currentIdx = idx

	if idx == basebackup.ManifestSinkIndex {
		s.manifestBuf.Reset()
		return nil
	}

	// Record the tablespace OID so parseTar can stamp every FileEntry
	// / DirEntry from this tar with its owning tablespace.  OID 0 (the
	// default tablespace) is stored as 0 and is the zero value, so the
	// stamp is a no-op for PGDATA-root files.
	s.tsOID[idx] = info.OID

	pr, pw := io.Pipe()
	s.pipeWriter = pw
	done := make(chan parserResult, 1)
	s.parserDone = done

	// Capture references for the goroutine's closure: ctx (so chunk
	// uploads see cancellation), the channel itself (so the goroutine
	// doesn't re-read s.parserDone at send time, which would race with
	// OnTablespaceEnd nilling it out), and idx (immutable per-call).
	ctx := s.ctx
	tsOID := info.OID
	go func() {
		// Defer pr.Close so any subsequent Write on pw returns
		// io.ErrClosedPipe rather than blocking forever.
		defer pr.Close()
		files, dirs, err := s.parseTar(ctx, pr, idx, tsOID)
		done <- parserResult{files: files, dirs: dirs, err: err}
	}()
	return nil
}

// OnTablespaceData implements basebackup.Sink. For real tablespaces it
// writes bytes into the pipe (which the parser pulls from); for the
// manifest CopyOut it accumulates into the manifest buffer.
//
// Returning a non-nil error aborts BASE_BACKUP: basebackup.Run wraps it
// and propagates upstream.
func (s *Sink) OnTablespaceData(idx int, data []byte) error {
	s.mu.Lock()
	if s.currentIdx != idx {
		s.mu.Unlock()
		return fmt.Errorf("tarsink: OnTablespaceData(%d) but currentIdx=%d", idx, s.currentIdx)
	}
	if idx == basebackup.ManifestSinkIndex {
		s.manifestBuf.Write(data)
		s.mu.Unlock()
		return nil
	}
	pw := s.pipeWriter
	s.mu.Unlock()

	// Write outside the mutex so the parser goroutine can take the
	// lock for its own state if it needs to. Pipe writes are
	// synchronous: they block until the parser reads, providing
	// natural backpressure all the way back to the network.
	if _, err := pw.Write(data); err != nil {
		// The pipe closed for one of three reasons. Surface them in
		// causal order so the caller sees the root cause, not the
		// downstream io.ErrClosedPipe symptom:
		//   1. parser hit a fatal error (e.g. malformed tar entry)
		//   2. ctx was cancelled (caller-driven abort)
		//   3. some other transport-level write failure
		if rootErr := s.peekParserErr(); rootErr != nil {
			return rootErr
		}
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("tarsink: pipe write: %w", err)
	}
	return nil
}

// peekParserErr non-blockingly tries to read from parserDone. Returns
// the parser's error if it has finished, nil otherwise. Used to
// upgrade an io.ErrClosedPipe into the parser's actual root cause.
func (s *Sink) peekParserErr() error {
	s.mu.Lock()
	ch := s.parserDone
	s.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case res := <-ch:
		// Re-stash the result; OnTablespaceEnd will read it for real.
		// Because parserDone is buffered (cap 1), this push always
		// succeeds.
		ch <- res
		return res.err
	default:
		return nil
	}
}

// OnTablespaceEnd implements basebackup.Sink. Closes the pipe writer
// (so the parser sees EOF), waits for the parser goroutine to drain,
// and stashes its FileEntry list. For the manifest CopyOut it commits
// the accumulated bytes to ManifestBytes.
func (s *Sink) OnTablespaceEnd(idx int) error {
	s.mu.Lock()
	if s.currentIdx != idx {
		s.mu.Unlock()
		return fmt.Errorf("tarsink: OnTablespaceEnd(%d) but currentIdx=%d", idx, s.currentIdx)
	}

	if idx == basebackup.ManifestSinkIndex {
		s.manifestBytes = append([]byte(nil), s.manifestBuf.Bytes()...)
		s.manifestBuf.Reset()
		s.currentIdx = unsetIdx
		s.mu.Unlock()
		return nil
	}

	pw := s.pipeWriter
	done := s.parserDone
	s.pipeWriter = nil
	s.parserDone = nil
	s.mu.Unlock()

	// Closing the writer makes the parser's tar.Next return io.EOF for
	// the current entry, then the next tar.Next sees io.EOF too. The
	// parser exits cleanly.
	if err := pw.Close(); err != nil {
		return fmt.Errorf("tarsink: close pipe: %w", err)
	}

	res, ok := waitParser(s.ctx, done)
	if !ok {
		return s.ctx.Err()
	}
	if res.err != nil {
		return fmt.Errorf("tarsink: tablespace %d parse: %w", idx, res.err)
	}

	s.mu.Lock()
	s.files[idx] = res.files
	s.dirs[idx] = res.dirs
	s.currentIdx = unsetIdx
	s.mu.Unlock()
	return nil
}

// waitParser blocks on done with ctx-cancellation discipline. Returns
// (result, true) on parser completion and (zero, false) on ctx cancel.
func waitParser(ctx context.Context, done <-chan parserResult) (parserResult, bool) {
	select {
	case res := <-done:
		return res, true
	case <-ctx.Done():
		return parserResult{}, false
	}
}

// parseTar reads tar entries from r and produces FileEntry values for
// each regular file, with chunked bodies stored via cas.
//
// Special files (backup_label, tablespace_map) are intercepted into
// the Sink struct and NOT emitted as FileEntries.
//
// Errors at any step abort the parse: subsequent OnTablespaceData
// writes will see io.ErrClosedPipe and be re-surfaced as this error.
func (s *Sink) parseTar(ctx context.Context, r io.Reader, idx int, tsOID uint32) ([]backup.FileEntry, []backup.DirEntry, error) {
	tr := tar.NewReader(r)
	var files []backup.FileEntry
	var dirs []backup.DirEntry
	for {
		// ctx cancellation between entries.
		if err := ctx.Err(); err != nil {
			return files, dirs, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, dirs, nil
		}
		if err != nil {
			return files, dirs, fmt.Errorf("tar.Next: %w", err)
		}

		// Validate the entry name at the BACKUP boundary. PG's
		// BASE_BACKUP only ever emits clean PGDATA-relative names, so an
		// absolute path or a ".." traversal component signals a
		// corrupt, MITM'd, or malicious source. hdr.Name flows verbatim
		// into the manifest's FileEntry/DirEntry.Path and is later
		// joined onto the restore target dir. Restore re-checks via
		// safeJoinTarget (a poisoned entry would otherwise abort the
		// restore at recovery time, on a backup the operator thought was
		// good) — but rejecting here means we never CREATE such a
		// backup, and protects every other path consumer (partial dump,
		// verify sandbox, pg_combinebackup staging) regardless of their
		// own guards.
		if err := validateTarEntryName(hdr.Name); err != nil {
			return files, dirs, fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Capture directory entries explicitly.  PG sends every
			// PGDATA subdir as a TypeDir entry; for empty ones —
			// pg_wal/, pg_dynshmem/, pg_notify/, pg_replslot/,
			// pg_serial/, pg_snapshots/, pg_stat/, pg_stat_tmp/,
			// pg_subtrans/, pg_tblspc/, pg_twophase/ — this is the
			// only signal the restore has that they should exist.
			// Without recording them here, the restore step's
			// MkdirAll-from-file-parent only creates dirs that
			// happen to contain at least one regular file, and the
			// restored datadir is missing pg_wal/ etc., so PG
			// refuses to start (issue #7).
			dirs = append(dirs, backup.DirEntry{
				Path:          hdr.Name,
				Mode:          uint32(hdr.Mode),
				TablespaceOID: tsOID,
			})
			continue
		case tar.TypeReg:
			// Fall through to file handling below.
		default:
			// Symlinks / hard links / devices / xattr globals.
			// Tablespace symlinks are handled out-of-band via
			// the manifest's tablespace_map entry; other
			// symlinks in PGDATA are exotic and unsupported.
			continue
		}

		// Special files captured for the manifest. backup_label and
		// tablespace_map sit at the root of the base/default tablespace's
		// tar — and PG streams that archive LAST when user tablespaces
		// exist, not first. Keying on idx==0 therefore silently dropped
		// backup_label whenever a non-default tablespace was present,
		// producing a manifest that fails its own invariant check and
		// refuses to commit (issue #17). Match by name in whichever
		// archive carries them instead: the exact-name match can only
		// fire for the base tar, since user-tablespace entries are nested
		// under PG_<ver>_<cat>/... and never named exactly "backup_label"
		// or "tablespace_map" at the root.
		switch hdr.Name {
		case BackupLabelName:
			if err := s.captureSpecial(tr, &s.backupLabel); err != nil {
				return files, dirs, fmt.Errorf("read %s: %w", hdr.Name, err)
			}
			continue
		case TablespaceMapName:
			if err := s.captureSpecial(tr, &s.tablespaceMap); err != nil {
				return files, dirs, fmt.Errorf("read %s: %w", hdr.Name, err)
			}
			continue
		}

		entry, err := s.chunkFile(ctx, hdr, tr)
		if err != nil {
			return files, dirs, fmt.Errorf("chunk %s: %w", hdr.Name, err)
		}
		// Stamp the owning tablespace so restore materialises a
		// non-default-tablespace file under its real location rather
		// than flattening it under PGDATA root.
		entry.TablespaceOID = tsOID
		files = append(files, entry)
	}
}

// validateTarEntryName rejects tar entry names that could escape the
// restore target: empty names, absolute paths, names carrying a NUL or
// the OS path separator backslash, and any name that — once cleaned —
// retains a ".." traversal component or begins with "/". PG never emits
// such names; rejecting them stops a hostile or corrupt BASE_BACKUP
// stream from being committed as a backup whose paths would later be
// written outside the restore target (or, more visibly, refuse to
// restore at all).
func validateTarEntryName(name string) error {
	if name == "" {
		return errors.New("empty entry name")
	}
	if strings.ContainsRune(name, 0) {
		return errors.New("entry name contains a NUL byte")
	}
	// tar uses forward slashes; a backslash is not a legal PGDATA path
	// separator and would be a directory name on Windows restores.
	if strings.ContainsRune(name, '\\') {
		return errors.New("entry name contains a backslash")
	}
	// path.IsAbs handles the tar/posix forward-slash convention
	// regardless of host OS (filepath.IsAbs would miss "/x" on Windows).
	if path.IsAbs(name) {
		return errors.New("absolute entry name")
	}
	// After cleaning, a leading ".." (exactly ".." or "../...") means the
	// path escapes its root. path.Clean collapses interior ".." that
	// stays in-tree (a/../b -> b) but cannot remove a leading one.
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("entry name escapes the backup root via \"..\"")
	}
	return nil
}

// captureSpecial reads the entire current tar entry into *dst.
// Used for backup_label and tablespace_map (both small files).
func (s *Sink) captureSpecial(tr *tar.Reader, dst *[]byte) error {
	body, err := io.ReadAll(tr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	*dst = body
	s.mu.Unlock()
	return nil
}

// chunkFile streams the current tar entry through a fresh chunker into
// the CAS, building the FileEntry as it goes. The chunker reuses its
// internal buffer across iterations, so we copy each chunk's data
// before handing it off to PutChunk.
//
// When a file observer is wired (issue #9 / `--verbose`), this also
// counts deduped chunks and unique bytes so the observer's
// FileStats payload reports the file's local dedup story.
func (s *Sink) chunkFile(ctx context.Context, hdr *tar.Header, tr *tar.Reader) (backup.FileEntry, error) {
	entry := backup.FileEntry{
		Path:    hdr.Name,
		Size:    hdr.Size,
		Mode:    uint32(hdr.Mode),
		ModTime: normalizeModTime(hdr.ModTime),
	}

	// Empty file: no chunks. Common for things like postmaster.opts
	// flags or empty marker files in PGDATA.  Still notify the
	// observer so progress reporting covers every file PG sent.
	if hdr.Size == 0 {
		if s.fileObserver != nil {
			s.fileObserver(FileStats{Path: entry.Path})
		}
		return entry, nil
	}

	// A file's chunks are PRODUCED sequentially — the chunker makes a
	// single CDC pass over the tar entry body — but each chunk's
	// hash + compress + write is independent, so they run on a
	// bounded worker pool. While a worker is busy in PutChunk the
	// producer reads the next chunk and other workers drain the
	// backlog; errgroup.SetLimit caps in-flight workers, and g.Go
	// blocks the producer when the pool is full, so memory stays at
	// O(chunkConcurrency) chunk bodies. Only the producer touches
	// the tar.Reader, so there is no read race on the stream.
	type chunkSlot struct {
		ref         backup.ChunkRef
		deduped     bool
		uniqueBytes int64
	}
	ch := s.chunkerFactory()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.chunkConcurrency)

	var results sync.Map // chunk index (int) -> chunkSlot
	count := 0
	var produceErr error
	for chk, err := range ch.Iter(tr) {
		if err != nil {
			produceErr = err
			break
		}
		// A worker failed: stop feeding — g.Wait surfaces the error.
		if gctx.Err() != nil {
			break
		}
		// The chunker reuses its buffer across iterations; copy
		// before the bytes cross into a worker goroutine.
		body := make([]byte, len(chk.Data))
		copy(body, chk.Data)
		idx, offset := count, chk.Offset
		count++
		g.Go(func() error {
			info, err := s.cas.PutChunk(gctx, body)
			if err != nil {
				return fmt.Errorf("cas put: %w", err)
			}
			results.Store(idx, chunkSlot{
				ref: backup.ChunkRef{
					Hash:   info.Hash,
					Offset: offset,
					Len:    info.Size,
				},
				deduped:     info.Deduped,
				uniqueBytes: info.Size,
			})
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return entry, err
	}
	if produceErr != nil {
		return entry, produceErr
	}

	// Workers finished out of order; reassemble the chunk list in
	// the file's offset order — the manifest needs it sequential.
	entry.Chunks = make([]backup.ChunkRef, count)
	var dedupedChunks int
	var uniqueBytes int64
	for i := 0; i < count; i++ {
		v, ok := results.Load(i)
		if !ok {
			return entry, fmt.Errorf("tarsink: chunk %d of %q missing after PutChunk", i, entry.Path)
		}
		slot := v.(chunkSlot)
		entry.Chunks[i] = slot.ref
		if slot.deduped {
			dedupedChunks++
		} else {
			uniqueBytes += slot.uniqueBytes
		}
	}

	if s.fileObserver != nil {
		s.fileObserver(FileStats{
			Path:          entry.Path,
			Size:          entry.Size,
			ChunkCount:    len(entry.Chunks),
			DedupedChunks: dedupedChunks,
			UniqueBytes:   uniqueBytes,
		})
	}
	return entry, nil
}

// normalizeModTime drops sub-second precision from tar mtimes so the
// resulting manifest is deterministic across PG re-emits of the same
// data. Tar headers carry second-resolution mtimes anyway; the extra
// nanoseconds Go time.Time may attach are noise.
func normalizeModTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Second)
}

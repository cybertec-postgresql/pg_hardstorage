// push.go — PushSegmentFile: chunked archive-style upload of a single WAL segment to the repo.
package walsink

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// PushOptions controls PushSegmentFile.
type PushOptions struct {
	Deployment       string
	SystemIdentifier string

	// ChunkerFactory builds the chunker used to slice the segment.
	// Defaults to chunker.New if nil.
	ChunkerFactory func() *chunker.Chunker

	// WORM, when non-nil, propagates a retention deadline to the
	// per-segment manifest Put. Same semantics as Sink.Options.WORM
	// — the deadline is captured at commit time, computed against
	// time.Now() so each archived segment's retention starts at
	// the moment of archive_command invocation. Chunk retention is
	// the caller's concern: pass a CAS built via
	// casdefault.NewWithRetention if chunks should also be locked.
	WORM *repo.WORMPolicy

	// Encryption, when non-nil, is recorded on the segment manifest so
	// restore can resolve the shared DEK from it. The caller MUST also
	// pass an encrypting CAS (casdefault.NewEncrypted*) built with the
	// same DEK so the chunks are actually encrypted; this field only
	// stamps the envelope, it does not encrypt (issue #106).
	Encryption *EncryptionInfo

	// SegmentSize is the cluster's DECLARED wal_segment_size (from
	// initdb --wal-segsize, probed once per cluster). The pushed file
	// MUST be exactly this many bytes; a file of any other length is
	// rejected rather than having its size inferred from len(body).
	//
	// Inferring the size from the file length is unsafe: a segment
	// truncated to a SMALLER power of two (e.g. a 16 MiB segment cut to
	// 8 MiB by a crash mid-copy) is itself a valid segment size, so it
	// would be accepted and archived with the wrong SegmentsPerLog /
	// segment-number, corrupting hole-detection and PITR math (audit
	// #58). Zero resolves to DefaultSegmentSize (16 MiB) so callers that
	// haven't yet threaded the probed size through still get the correct
	// behaviour for a default cluster.
	SegmentSize int64
}

// PushSegmentFile reads a 16 MiB WAL segment from path, runs it through
// the chunker, and commits a SegmentManifest into the repo behind sp/cas.
//
// This is the archive_command shim: PG invokes
// `pg_hardstorage wal push <deployment> %p` once per archived segment,
// passing the segment file's path. We:
//
//  1. Read the file (must be exactly SegmentSize bytes).
//  2. Derive timeline + segment number from the basename (24-char hex).
//  3. Chunk + commit the manifest.
//
// Idempotency: SegmentPath / RenameIfNotExists semantics give us
// archive_command's required idempotency for free. Re-pushing an
// already-committed segment is a no-op, the same as for streaming.
//
// History files (TLI history files PG also passes via archive_command)
// have the suffix `.history` and are NOT 16 MiB; they're rejected here
// with ErrNotASegmentFile. The caller (the wal push CLI command) is
// expected to route them through a separate history-file path; v0.1
// doesn't ship that path so they're stored elsewhere or refused.
func PushSegmentFile(ctx context.Context, cas *repo.CAS, sp storage.StoragePlugin, path string, opts PushOptions) (*SegmentManifest, error) {
	if cas == nil {
		return nil, errors.New("walsink: nil CAS")
	}
	if sp == nil {
		return nil, errors.New("walsink: nil StoragePlugin")
	}
	if opts.Deployment == "" {
		return nil, errors.New("walsink: empty deployment")
	}
	if opts.SystemIdentifier == "" {
		return nil, errors.New("walsink: empty SystemIdentifier")
	}

	base := filepath.Base(path)

	// The cluster's DECLARED segment size drives BOTH the exact-length
	// check on the file and the contiguous segment-number math. Never
	// infer it from the file length: a truncated segment is a smaller
	// valid size and would be silently mis-numbered (audit #58).
	segSize := NormSegmentSize(opts.SegmentSize)
	if !ValidSegmentSize(segSize) {
		return nil, fmt.Errorf("walsink push: declared SegmentSize=%d is not a valid WAL segment size (want a power of two in [1 MiB, 1 GiB])", opts.SegmentSize)
	}

	// Validate the NAME shape first so non-segment files (.history,
	// .partial, .backup, garbage) are rejected with ErrNotASegmentFile
	// and routed to the aux path before we read any bytes.
	if _, _, err := ParseSegmentName(base, segSize); err != nil {
		return nil, err
	}
	body, err := readSegmentFile(path, segSize)
	if err != nil {
		return nil, err
	}
	tli, segNum, err := ParseSegmentName(base, segSize)
	if err != nil {
		return nil, err
	}

	chunkerFn := opts.ChunkerFactory
	if chunkerFn == nil {
		chunkerFn = chunker.New
	}

	ch := chunkerFn()
	var refs []ChunkRef
	for c, cerr := range ch.Iter(bytes.NewReader(body)) {
		if cerr != nil {
			return nil, fmt.Errorf("walsink push: chunker: %w", cerr)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// chunker reuses its buffer; copy before retaining.
		bs := append([]byte(nil), c.Data...)
		info, perr := cas.PutChunk(ctx, bs)
		if perr != nil {
			return nil, fmt.Errorf("walsink push: cas put: %w", perr)
		}
		refs = append(refs, ChunkRef{
			Hash:   info.Hash,
			Offset: c.Offset,
			Len:    info.Size,
		})
	}

	startLSN := pglogrepl.LSN(uint64(segNum) * uint64(segSize))
	endLSN := startLSN + pglogrepl.LSN(segSize)
	m := &SegmentManifest{
		Schema:           Schema,
		Deployment:       opts.Deployment,
		SystemIdentifier: opts.SystemIdentifier,
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      base,
		StartLSN:         startLSN.String(),
		EndLSN:           endLSN.String(),
		SegmentSize:      segSize,
		Chunks:           refs,
		CreatedAt:        time.Now().UTC(),
		Encryption:       opts.Encryption,
	}

	// commitManifestStandalone wraps the same primitives as Sink.commitManifest
	// without depending on Sink state. WORM (when configured)
	// flows through here so the manifest gets the same retention
	// the Sink path applies.
	if err := commitManifestStandalone(ctx, sp, m, opts.WORM); err != nil {
		return nil, err
	}
	// Metrics: one segment durably archived.  SegmentSize is the fixed
	// 16 MiB logical width of a WAL segment; the on-the-wire footprint
	// after chunk dedup/compression is smaller, but the archived-bytes
	// counter tracks logical throughput so operators can reason about
	// WAL generation rate independent of dedup.
	metrics.WALSegmentArchived(opts.Deployment, segSize)
	return m, nil
}

// ReadSystemIdentifierFromSegment extracts the cluster
// system_identifier from a WAL segment file's first-page long
// header.  Lets archive_command callers skip the libpq round-trip
// per push (issue #8) — the header is a self-describing, integer-
// addressable field present on every PG-generated segment since
// the format was introduced.
//
// Layout of the first 32 bytes (XLogPageHeaderData +
// XLogLongPageHeaderData prefix), all native-endian (x86_64 ==
// little-endian; we read LE explicitly so the result is
// reproducible across architectures):
//
//	offset  0  uint16  xlp_magic       (PG version sentinel)
//	offset  2  uint16  xlp_info        (XLP_LONG_HEADER == 0x0002)
//	offset  4  uint32  xlp_tli
//	offset  8  uint64  xlp_pageaddr
//	offset 16  uint32  xlp_rem_len
//	offset 20  uint32  (alignment pad)
//	offset 24  uint64  xlp_sysid       ← the field we want
//
// We require XLP_LONG_HEADER on the first page; only then is
// xlp_sysid present.  PG always sets this on segment-page-0, so
// any 16 MiB WAL file PG hands archive_command will satisfy the
// check.  Returns the system_identifier as a decimal string —
// matching the format pg_hardstorage stamps on every manifest.
func ReadSystemIdentifierFromSegment(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("walsink: open %s: %w", path, err)
	}
	defer f.Close()
	var hdr [32]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return "", fmt.Errorf("walsink: read header of %s: %w", path, err)
	}
	xlpInfo := binary.LittleEndian.Uint16(hdr[2:4])
	const xlpLongHeader uint16 = 0x0002
	if xlpInfo&xlpLongHeader == 0 {
		return "", fmt.Errorf("walsink: %s lacks XLP_LONG_HEADER (info=0x%04x); not the first page of a segment?",
			path, xlpInfo)
	}
	sysID := binary.LittleEndian.Uint64(hdr[24:32])
	if sysID == 0 {
		return "", fmt.Errorf("walsink: %s reports xlp_sysid=0 (corrupt or fixture?)", path)
	}
	return strconv.FormatUint(sysID, 10), nil
}

// readSegmentFile reads a WAL segment file in full and returns its
// bytes. The file MUST be EXACTLY wantSize bytes — the cluster's
// declared wal_segment_size. We do NOT infer the size from the file's
// own length: a segment truncated to a smaller power of two (e.g. a
// crash mid-copy) is itself a valid segment size, so inferring would
// accept it and archive it with the wrong segment-number, corrupting
// hole-detection math (audit #58). Any length other than wantSize —
// truncated, over-long, or a non-segment file routed here by mistake —
// is refused with an obvious error.
func readSegmentFile(path string, wantSize int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("walsink push: open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("walsink push: stat %s: %w", path, err)
	}
	if st.Size() != wantSize {
		return nil, fmt.Errorf("walsink push: %s: size=%d does not match the declared wal_segment_size=%d (a truncated segment, a non-segment file, or a cluster whose segment size wasn't threaded through?)", path, st.Size(), wantSize)
	}
	body := make([]byte, st.Size())
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, fmt.Errorf("walsink push: read %s: %w", path, err)
	}
	return body, nil
}

// ErrNotASegmentFile is returned by ParseSegmentName when the input
// isn't a canonical 24-char hex segment name. The CLI maps this to
// notfound.wal_segment_name.
var ErrNotASegmentFile = errors.New("walsink: not a canonical WAL segment file name")

// AuxiliaryFileKind tags a non-segment file PG hands archive_command
// alongside real 16 MiB WAL segments.  The two kinds we observe:
//
//   - `*.backup` — backup-history file (a few hundred bytes of plain
//     text recording START/STOP LSN of a base backup; emitted by
//     pg_backup_stop / pg_basebackup).
//   - `*.history` — timeline history file (small text file recording
//     a timeline's parent and switch LSN; emitted on promotion or
//     PITR-with-recovery_target).
//
// Both are too small to be worth chunking, carry no segment header
// (so ReadSystemIdentifierFromSegment fails on them), and are
// referenced by recovery either by name (`.history`) or for operator
// visibility only (`.backup`).  We archive them verbatim in a
// dedicated repo prefix — issue #10.
type AuxiliaryFileKind int

const (
	// AuxiliaryNone — input is a regular WAL segment.
	AuxiliaryNone AuxiliaryFileKind = iota
	// AuxiliaryBackup — `<24segchars>.<offset>.backup`.
	AuxiliaryBackup
	// AuxiliaryHistory — `<8tli>.history`.
	AuxiliaryHistory
	// AuxiliaryPartial — `<24segchars>.partial`: the final, partially
	// filled segment of a timeline that PG archives on a STANDBY PROMOTION
	// when archive_mode=always. We store it verbatim (keyed by timeline,
	// like a .backup) for two reasons: (1) rejecting it fails the
	// archive_command, and PG's archiver retries the SAME file without
	// skipping ahead — stalling ALL subsequent WAL archiving on the
	// promoted node until pg_wal fills the disk; (2) the partial holds the
	// old timeline's tail WAL, which a PITR onto that timeline may need.
	AuxiliaryPartial
)

// ClassifyArchiveInput inspects the basename PG handed
// archive_command and tells the caller which code path to use.
// The classifier is purely lexical (no I/O, no header read) so it
// can run before any expensive open/stat call.
func ClassifyArchiveInput(name string) AuxiliaryFileKind {
	switch {
	case strings.HasSuffix(name, ".backup"):
		return AuxiliaryBackup
	case strings.HasSuffix(name, ".history"):
		return AuxiliaryHistory
	case strings.HasSuffix(name, ".partial"):
		return AuxiliaryPartial
	default:
		return AuxiliaryNone
	}
}

// AuxiliaryFilePath returns the canonical repo key for an
// archive_command-supplied auxiliary file.  The layout deliberately
// keeps `.backup` files adjacent to the timeline they describe (so a
// `repo gc` walk that retains a timeline's WAL also retains its
// backup-history) and pools `.history` files under a single prefix
// (one per TLI, deployment-wide):
//
//	wal/<deployment>/<TLI-hex>/<basename>           — .backup
//	wal/<deployment>/history/<basename>             — .history
//
// kind must be AuxiliaryBackup or AuxiliaryHistory; AuxiliaryNone
// yields the empty string (callers route segments via SegmentPath
// instead).  Timeline is derived from the leading 8 hex chars of the
// basename; an unparseable name falls back to the `unknown/` bucket
// so we never lose a file PG sent us, even on novel naming.
func AuxiliaryFilePath(deployment, basename string, kind AuxiliaryFileKind) string {
	switch kind {
	case AuxiliaryBackup, AuxiliaryPartial:
		// Both name as `<24hex...>...` with the timeline in the leading 8
		// hex, so both land next to that timeline's segment-manifest keys.
		tli := parseLeadingTimeline(basename)
		if tli == "" {
			return fmt.Sprintf("wal/%s/unknown/%s", deployment, basename)
		}
		return fmt.Sprintf("wal/%s/%s/%s", deployment, tli, basename)
	case AuxiliaryHistory:
		return fmt.Sprintf("wal/%s/history/%s", deployment, basename)
	default:
		return ""
	}
}

// parseLeadingTimeline returns the 8-char uppercase-hex timeline
// prefix of name, normalised to the same format
// SegmentFileName/SegmentPath emit (so .backup files land next to
// the segment-manifest key set even if PG happened to lower-case).
// Returns "" when name doesn't begin with 8 hex digits.
func parseLeadingTimeline(name string) string {
	if len(name) < 8 {
		return ""
	}
	for i := 0; i < 8; i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	tli, err := strconv.ParseUint(name[:8], 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%08X", uint32(tli))
}

// MaxAuxiliaryFileSize bounds what we'll accept as an auxiliary
// file.  PG's `.backup` and `.history` files are tiny — a few
// hundred bytes, in pathological cases under a kilobyte.  64 KiB
// gives a 100x margin and keeps the in-memory read trivially
// bounded.  A larger payload here means PG handed archive_command
// something we don't understand; refuse loudly rather than copy
// arbitrary bytes into the repo.
const MaxAuxiliaryFileSize = 64 * 1024

// maxAuxiliaryFileSize is the per-kind upper bound. `.backup` / `.history`
// are tiny text files, so the 64 KiB cap both bounds the in-memory read and
// catches misuse (a real segment routed through the aux path). A `.partial`
// is a partially-filled WAL SEGMENT, though — up to a full segment — so it
// gets the maximum-segment-size cap (1 GiB, the largest wal_segment_size PG
// supports), which accepts a partial from a cluster of any segment size.
// Capping it at 64 KiB would reject every real partial and re-stall the
// archiver (the bug this kind was added to fix).
func maxAuxiliaryFileSize(kind AuxiliaryFileKind) int64 {
	if kind == AuxiliaryPartial {
		return MaxSegmentSize
	}
	return MaxAuxiliaryFileSize
}

// PushAuxiliaryFile archives a non-segment archive_command input
// (`.backup` / `.history`) into the repo verbatim.  Idempotent via
// RenameIfNotExists: a repushed file with the same key is a no-op,
// matching archive_command's required semantics.
//
// Auxiliary files do NOT carry a system_identifier: they have no
// segment header, and `.history` files predate the first
// `IDENTIFY_SYSTEM` we'd run anyway (PG promotes the standby to
// primary, then writes `00000002.history` referencing the new TLI).
// Recording sysid would require a libpq round-trip per push, which
// is exactly what issue #8 removed for segments — we keep aux
// files quiet on that field for the same reason.
func PushAuxiliaryFile(ctx context.Context, sp storage.StoragePlugin, path string, opts PushOptions) (string, AuxiliaryFileKind, error) {
	if sp == nil {
		return "", AuxiliaryNone, errors.New("walsink: nil StoragePlugin")
	}
	if opts.Deployment == "" {
		return "", AuxiliaryNone, errors.New("walsink: empty deployment")
	}
	base := filepath.Base(path)
	kind := ClassifyArchiveInput(base)
	if kind == AuxiliaryNone {
		return "", AuxiliaryNone, fmt.Errorf("%w: %q (not .backup, .history or .partial)", ErrNotASegmentFile, base)
	}

	body, err := readAuxiliaryFile(path, maxAuxiliaryFileSize(kind))
	if err != nil {
		return "", kind, err
	}

	key := AuxiliaryFilePath(opts.Deployment, base, kind)
	tmp := key + ".tmp." + randSuffix()
	putOpts := storage.PutOptions{ContentLength: int64(len(body))}
	if !opts.WORM.IsZero() {
		now := time.Now().UTC()
		putOpts.RetainUntil = opts.WORM.RetainUntil(now)
		putOpts.RetentionMode = storage.WORMMode(opts.WORM.Mode)
	}
	if _, err := sp.Put(ctx, tmp, bytes.NewReader(body), putOpts); err != nil {
		return "", kind, fmt.Errorf("walsink push: put tmp aux file: %w", err)
	}
	if err := sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = sp.Delete(ctx, tmp)
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Idempotent: PG retried archive_command after a
			// previous success; the existing object is the
			// authoritative copy.  Treat as success.
			return key, kind, nil
		}
		return "", kind, fmt.Errorf("walsink push: commit aux file: %w", err)
	}
	return key, kind, nil
}

// readAuxiliaryFile slurps an auxiliary file in full, refusing anything larger
// than max (kind-dependent, see maxAuxiliaryFileSize). The bound keeps the
// in-memory read trivially bounded and catches misuse (a regular segment
// routed through the aux path).
func readAuxiliaryFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("walsink push: open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("walsink push: stat %s: %w", path, err)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("walsink push: %s: size=%d exceeds aux-file cap %d", path, st.Size(), max)
	}
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("walsink push: read %s: %w", path, err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("walsink push: %s: size>%d (aux-file cap)", path, max)
	}
	return body, nil
}

// ParseSegmentName splits a 24-char hex segment name into (timeline,
// contiguous segment_number) for the given segment size. Layout (PG
// canonical):
//
//	TTTTTTTT LLLLLLLL SSSSSSSS
//	timeline log_id   seg_in_log
//
// The contiguous segment number = log_id * segmentsPerLog + seg_in_log,
// where segmentsPerLog = 4 GiB / segmentSize (256 for the default
// 16 MiB). segmentSize 0 resolves to 16 MiB. Returns ErrNotASegmentFile
// for anything that doesn't match — including .history files (which
// have a `.history` suffix), .partial files, and 25+ char names.
func ParseSegmentName(name string, segmentSize int64) (tli uint32, segNum uint64, err error) {
	if len(name) != 24 {
		return 0, 0, fmt.Errorf("%w: %q (len %d, want 24)", ErrNotASegmentFile, name, len(name))
	}
	t64, terr := strconv.ParseUint(name[:8], 16, 32)
	logID, lerr := strconv.ParseUint(name[8:16], 16, 32)
	segLo, serr := strconv.ParseUint(name[16:24], 16, 32)
	if terr != nil || lerr != nil || serr != nil {
		return 0, 0, fmt.Errorf("%w: %q (non-hex)", ErrNotASegmentFile, name)
	}
	return uint32(t64), logID*SegmentsPerLog(segmentSize) + segLo, nil
}

// commitManifestStandalone writes m to its canonical key via tmp+rename.
// Mirrors Sink.commitManifest but doesn't depend on Sink internals so
// the push path stays self-contained. The optional WORM policy
// propagates a per-Put retention deadline.
func commitManifestStandalone(ctx context.Context, sp storage.StoragePlugin, m *SegmentManifest, worm *repo.WORMPolicy) error {
	lock, err := repo.AcquireMutationLock(ctx, sp, "WAL push manifest "+m.SegmentName)
	if err != nil {
		return fmt.Errorf("walsink push: commit mutation lock: %w", err)
	}
	defer func() { _ = lock.Release(context.Background()) }()
	if err := ensureSegmentChunksPresent(ctx, sp, m); err != nil {
		return err
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		return err
	}
	key := SegmentPath(m.Deployment, m.Timeline, m.SegmentName)
	tmp := key + ".tmp." + randSuffix()
	putOpts := storage.PutOptions{ContentLength: int64(len(body))}
	if !worm.IsZero() {
		now := time.Now().UTC()
		putOpts.RetainUntil = worm.RetainUntil(now)
		putOpts.RetentionMode = storage.WORMMode(worm.Mode)
	}
	if _, err := sp.Put(ctx, tmp, bytes.NewReader(body), putOpts); err != nil {
		return fmt.Errorf("walsink push: put tmp manifest: %w", err)
	}
	if err := sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = sp.Delete(ctx, tmp)
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Existing manifest at this key — could be a
			// genuine retry (same content, true idempotent
			// success) OR a split-brain doppelgänger (two
			// clusters with cloned system_identifier both
			// archiving the same segment number with
			// DIFFERENT body bytes; archive_command's
			// idempotent-rename path used to silently treat
			// the loser as success — see L4_doppelganger
			// scenario for the regression test).
			//
			// Read the existing manifest, compare against
			// what we tried to commit:
			//
			//   * sysid mismatch → splitbrain across
			//     clusters (operator cloned a datadir
			//     without pg_resetwal).
			//   * sysid match but chunk-list mismatch →
			//     content drift on the same cluster (data
			//     corruption, mid-stream truncation, ...).
			//   * sysid match AND chunk-list match → true
			//     idempotent retry; return nil.
			return verifyExistingManifest(ctx, sp, key, m)
		}
		return fmt.Errorf("walsink push: commit manifest: %w", err)
	}
	return nil
}

// verifyExistingManifest reads the on-disk manifest at key
// and compares it to m's identity.  Returns nil iff the
// existing manifest is byte-equivalent (true idempotent
// re-push); otherwise returns a structured error describing
// the kind of split-brain detected.  Used by both the
// archive_command path (commitManifestStandalone) and the
// streaming path (Sink.commitManifest).
//
// Error code prefixes:
//
//	splitbrain.system_identifier_mismatch — different cluster
//	    archiving the same segment number (cloned datadir).
//	splitbrain.content_mismatch          — same cluster but
//	    different bytes (mid-stream corruption, an external
//	    tool modifying pg_wal, etc.).
//	splitbrain.read_failed               — manifest exists but
//	    can't be read (probably transient; refusal-on-doubt
//	    so the operator gets a clear retry signal).
func verifyExistingManifest(ctx context.Context, sp storage.StoragePlugin, key string, m *SegmentManifest) error {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("splitbrain.read_failed: existing manifest %q present but unreadable: %w", key, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("splitbrain.read_failed: read existing manifest %q: %w", key, err)
	}
	existing, err := ParseSegmentManifest(body)
	if err != nil {
		return fmt.Errorf("splitbrain.read_failed: parse existing manifest %q: %w", key, err)
	}
	if existing.SystemIdentifier != m.SystemIdentifier {
		return fmt.Errorf("splitbrain.system_identifier_mismatch: segment %s already archived by cluster %s; refusing to archive from cluster %s (cloned datadir without pg_resetwal?)",
			m.SegmentName, existing.SystemIdentifier, m.SystemIdentifier)
	}
	if !ChunkRefsEqual(existing.Chunks, m.Chunks) {
		return fmt.Errorf("splitbrain.content_mismatch: segment %s already archived with different content (existing manifest has %d chunks, this push has %d); split-brain or external corruption",
			m.SegmentName, len(existing.Chunks), len(m.Chunks))
	}
	// True idempotent re-push: every identifying field
	// matches.  PG retried archive_command after our prior
	// success; nothing more to do.
	return nil
}

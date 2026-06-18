package walsink_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestParseSegmentName(t *testing.T) {
	cases := []struct {
		in      string
		wantTLI uint32
		wantSeg uint64
		wantErr bool
	}{
		{"000000010000000000000003", 1, 3, false},
		{"00000002000000000000000A", 2, 10, false},
		// Spans into the second log file (logID=1, segLo=0 → 256).
		{"000000010000000100000000", 1, 256, false},
		{"badfilename", 0, 0, true},
		{"000000010000000000000003.history", 0, 0, true},
		{"000000010000000000000003.partial", 0, 0, true},
		{"00000001000000000000ZZZZ", 0, 0, true},
	}
	for _, tc := range cases {
		gotTLI, gotSeg, err := walsink.ParseSegmentName(tc.in, walsink.SegmentSize)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseSegmentName(%q) — expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSegmentName(%q) — unexpected error: %v", tc.in, err)
			continue
		}
		if gotTLI != tc.wantTLI || gotSeg != tc.wantSeg {
			t.Errorf("ParseSegmentName(%q) = (%d,%d); want (%d,%d)",
				tc.in, gotTLI, gotSeg, tc.wantTLI, tc.wantSeg)
		}
	}
	// Sentinel error type is set.
	if _, _, err := walsink.ParseSegmentName("toolong-toolong-toolong-toolong", walsink.SegmentSize); !errors.Is(err, walsink.ErrNotASegmentFile) {
		t.Errorf("expected ErrNotASegmentFile; got %v", err)
	}
}

func TestPushSegmentFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, cas := openFSRepo(t, repoURL)
	defer sp.Close()

	// Build a synthetic 16 MiB segment file.
	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "7000000000000000001",
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if m.SegmentName != segmentName {
		t.Errorf("SegmentName = %q", m.SegmentName)
	}
	if m.Timeline != 1 {
		t.Errorf("Timeline = %d", m.Timeline)
	}
	if m.SegmentNumber != 5 {
		t.Errorf("SegmentNumber = %d", m.SegmentNumber)
	}
	if len(m.Chunks) == 0 {
		t.Error("expected at least one chunk")
	}

	// Idempotent re-push: succeeds without error and produces the same
	// manifest content. (We don't compare CreatedAt — that's fresh per
	// call — but the segment-identifying fields should match.)
	m2, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "7000000000000000001",
	})
	if err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if m2.SegmentName != m.SegmentName || m2.Timeline != m.Timeline {
		t.Errorf("re-push produced different identity: %+v vs %+v", m2, m)
	}
}

func TestPushSegmentFile_RejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, cas := openFSRepo(t, repoURL)
	defer sp.Close()

	segPath := filepath.Join(t.TempDir(), "000000010000000000000001")
	if err := os.WriteFile(segPath, []byte("too small"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "size=") {
		t.Errorf("expected size-mismatch error; got %v", err)
	}
}

func TestPushSegmentFile_RejectsHistoryFile(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, cas := openFSRepo(t, repoURL)
	defer sp.Close()

	segPath := filepath.Join(t.TempDir(), "00000003.history")
	if err := os.WriteFile(segPath, []byte("history file body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "x",
	})
	if !errors.Is(err, walsink.ErrNotASegmentFile) {
		t.Errorf("expected ErrNotASegmentFile; got %v", err)
	}
}

// openFSRepo opens a fresh fs:// storage at repoURL and returns it.
// Caller must Close the returned sp.
func openFSRepo(t *testing.T, repoURL string) (storage.StoragePlugin, *repo.CAS) {
	t.Helper()
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	return p, casdefault.New(p)
}

// TestReadSystemIdentifierFromSegment_HappyPath synthesizes a
// 16 MiB WAL segment whose first page carries the
// XLogLongPageHeader signature and verifies the parser pulls
// xlp_sysid out byte-for-byte.  Issue #8 — without this,
// archive_command needed --pg-connection on every call.
func TestReadSystemIdentifierFromSegment_HappyPath(t *testing.T) {
	const wantSysID uint64 = 7388123456789012345
	dir := t.TempDir()
	path := filepath.Join(dir, "000000010000000000000003")

	body := make([]byte, walsink.SegmentSize)
	// xlp_magic — value irrelevant for sysid extraction; pick
	// PG 17's 0xD117 sentinel so a future "validate magic" gate
	// would still accept this fixture.
	binary.LittleEndian.PutUint16(body[0:2], 0xD117)
	// xlp_info — XLP_LONG_HEADER (0x0002) MUST be set on the
	// first page; the parser refuses to derive sysid otherwise.
	binary.LittleEndian.PutUint16(body[2:4], 0x0002)
	// xlp_tli, xlp_pageaddr, xlp_rem_len — the parser ignores
	// these but a real segment has them populated; leave zeroed.
	// xlp_sysid at offset 24, 8 bytes LE.
	binary.LittleEndian.PutUint64(body[24:32], wantSysID)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := walsink.ReadSystemIdentifierFromSegment(path)
	if err != nil {
		t.Fatalf("ReadSystemIdentifierFromSegment: %v", err)
	}
	if got != "7388123456789012345" {
		t.Errorf("sysid = %q; want \"7388123456789012345\"", got)
	}
}

// TestReadSystemIdentifierFromSegment_RejectsShortHeader: a page
// without XLP_LONG_HEADER set lacks xlp_sysid by definition.  We
// refuse rather than returning whatever happens to be at offset
// 24, since silently emitting a junk sysid would corrupt the
// repo's manifest (every segment manifest carries this field).
func TestReadSystemIdentifierFromSegment_RejectsShortHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000000010000000000000004")

	body := make([]byte, walsink.SegmentSize)
	// xlp_info without XLP_LONG_HEADER bit.  Even though offset
	// 24 has a non-zero number written below, the parser must
	// refuse because the long-header signature is absent.
	binary.LittleEndian.PutUint16(body[2:4], 0x0000)
	binary.LittleEndian.PutUint64(body[24:32], 0xDEADBEEFCAFEF00D)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := walsink.ReadSystemIdentifierFromSegment(path)
	if err == nil || !strings.Contains(err.Error(), "XLP_LONG_HEADER") {
		t.Errorf("expected XLP_LONG_HEADER refusal; got %v", err)
	}
}

// TestPushSegmentFile_DoppelgangerDetected reproduces the
// split-brain race: two clusters with the same
// system_identifier + timeline both push a segment with the
// same name but DIFFERENT body bytes.  Pre-fix: the second
// push silently returned nil, the operator believed their
// archive worked.  Post-fix: the second push must error with
// `splitbrain.content_mismatch`, surfacing the silent-loser
// bug to the operator before PITR ever picks the wrong
// cluster's bytes.
func TestPushSegmentFile_DoppelgangerDetected(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, cas := openFSRepo(t, repoURL)
	defer sp.Close()

	segmentName := "000000010000000000000003"
	segPath := filepath.Join(t.TempDir(), segmentName)

	// First push — cluster A's bytes.
	bodyA := make([]byte, walsink.SegmentSize)
	for i := range bodyA {
		bodyA[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, bodyA, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "split-brain-test",
		SystemIdentifier: "7388123456789012345",
	}); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// Second push — cluster B's bytes (different content) at
	// the same segment name + sysid.  The chunker emits a
	// fully distinct chunk-hash list, so the manifests
	// diverge in their Chunks slices.
	bodyB := make([]byte, walsink.SegmentSize)
	for i := range bodyB {
		bodyB[i] = byte((i ^ 0x5a) % 256)
	}
	segPathB := filepath.Join(t.TempDir(), segmentName)
	if err := os.WriteFile(segPathB, bodyB, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPathB, walsink.PushOptions{
		Deployment:       "split-brain-test",
		SystemIdentifier: "7388123456789012345", // same sysid; different body
	})
	if err == nil {
		t.Fatal("expected splitbrain.content_mismatch on doppelgänger push, got nil (silent-success bug)")
	}
	if !strings.Contains(err.Error(), "splitbrain.content_mismatch") {
		t.Errorf("error code: got %v\nwant prefix splitbrain.content_mismatch", err)
	}
}

// TestPushSegmentFile_SystemIdentifierMismatch covers the
// other half of the split-brain matrix: same segment name,
// DIFFERENT system_identifier (the realistic operator
// scenario where two truly distinct clusters were
// accidentally pointed at the same repo URL).  Must surface
// `splitbrain.system_identifier_mismatch`.
func TestPushSegmentFile_SystemIdentifierMismatch(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, cas := openFSRepo(t, repoURL)
	defer sp.Close()

	segmentName := "000000010000000000000003"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Cluster A pushes first.
	if _, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "shared-deploy",
		SystemIdentifier: "1111111111111111111",
	}); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Cluster B (DIFFERENT sysid) pushes the same bytes.
	// Even with byte-identical content, the sysid mismatch
	// is itself the operator-actionable signal.
	_, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, walsink.PushOptions{
		Deployment:       "shared-deploy",
		SystemIdentifier: "2222222222222222222",
	})
	if err == nil {
		t.Fatal("expected splitbrain.system_identifier_mismatch on second push, got nil")
	}
	if !strings.Contains(err.Error(), "splitbrain.system_identifier_mismatch") {
		t.Errorf("error code: got %v\nwant prefix splitbrain.system_identifier_mismatch", err)
	}
}

// TestPushSegmentFile_TrueIdempotentRetry pins the contract
// for the case archive_command actually retries: same bytes,
// same sysid — must succeed silently (return nil) so PG's
// retry loop converges.
func TestPushSegmentFile_TrueIdempotentRetry(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, cas := openFSRepo(t, repoURL)
	defer sp.Close()

	segmentName := "000000010000000000000003"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := walsink.PushOptions{
		Deployment:       "retry-test",
		SystemIdentifier: "9999999999999999999",
	}
	if _, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Second push with identical inputs — PG's
	// archive_command retry path; must succeed.
	if _, err := walsink.PushSegmentFile(context.Background(), cas, sp, segPath, opts); err != nil {
		t.Errorf("idempotent retry returned error; want nil: %v", err)
	}
}

// TestClassifyArchiveInput verifies the lexical classifier that
// sits in front of the real push paths.  It must not need I/O —
// archive_command runs hot, every PG invocation pays the cost.
func TestClassifyArchiveInput(t *testing.T) {
	cases := []struct {
		in   string
		want walsink.AuxiliaryFileKind
	}{
		{"000000010000000000000003", walsink.AuxiliaryNone},
		{"0000000100000013000000FE.000000D8.backup", walsink.AuxiliaryBackup},
		{"00000002.history", walsink.AuxiliaryHistory},
		// A standby promotion under archive_mode=always archives the
		// timeline's last partial segment — store it, never reject it
		// (rejecting stalls the archiver and fills the disk).
		{"000000010000000000000003.partial", walsink.AuxiliaryPartial},
		// Suffix match only — leading-dot or no-extension files
		// stay AuxiliaryNone so callers fall through to the
		// segment-validation path.
		{"backup", walsink.AuxiliaryNone},
		{"history", walsink.AuxiliaryNone},
	}
	for _, tc := range cases {
		got := walsink.ClassifyArchiveInput(tc.in)
		if got != tc.want {
			t.Errorf("ClassifyArchiveInput(%q) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

// TestAuxiliaryFilePath checks the canonical layout: backup-history
// files live next to their timeline's segment manifests; history
// files pool under a dedicated `history/` prefix; an unparseable
// leading 8 chars sinks to `unknown/` rather than panicking.
func TestAuxiliaryFilePath(t *testing.T) {
	cases := []struct {
		dep, base string
		kind      walsink.AuxiliaryFileKind
		want      string
	}{
		{"db1", "0000000100000013000000FE.000000D8.backup", walsink.AuxiliaryBackup, "wal/db1/00000001/0000000100000013000000FE.000000D8.backup"},
		// .partial lands next to its timeline's segments, like .backup.
		{"db1", "000000010000000000000003.partial", walsink.AuxiliaryPartial, "wal/db1/00000001/000000010000000000000003.partial"},
		{"db1", "00000002.history", walsink.AuxiliaryHistory, "wal/db1/history/00000002.history"},
		{"db1", "ZZZZZZZZ.history", walsink.AuxiliaryHistory, "wal/db1/history/ZZZZZZZZ.history"},
		{"db1", "ZZZZZZZZ.000000D8.backup", walsink.AuxiliaryBackup, "wal/db1/unknown/ZZZZZZZZ.000000D8.backup"},
		{"db1", "anything", walsink.AuxiliaryNone, ""},
	}
	for _, tc := range cases {
		got := walsink.AuxiliaryFilePath(tc.dep, tc.base, tc.kind)
		if got != tc.want {
			t.Errorf("AuxiliaryFilePath(%q,%q,%d) = %q; want %q", tc.dep, tc.base, tc.kind, got, tc.want)
		}
	}
}

// TestPushAuxiliaryFile_BackupRoundTrip is the happy-path coverage
// for the issue #10 fix.  A .backup file with realistic content (a
// few hundred bytes of plain text) round-trips through the storage
// plugin verbatim and lands at the canonical key.
func TestPushAuxiliaryFile_BackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	base := "0000000100000013000000FE.000000D8.backup"
	src := filepath.Join(t.TempDir(), base)
	body := []byte("START WAL LOCATION: 13/FE000098 (file 0000000100000013000000FE)\n" +
		"STOP WAL LOCATION: 13/FE000170 (file 0000000100000013000000FE)\n" +
		"CHECKPOINT LOCATION: 13/FE000130\n" +
		"BACKUP METHOD: streamed\n" +
		"BACKUP FROM: primary\n" +
		"START TIME: 2026-05-06 17:24:28 CEST\n" +
		"LABEL: pg_basebackup base backup\n" +
		"START TIMELINE: 1\n")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}

	key, kind, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatalf("PushAuxiliaryFile: %v", err)
	}
	if kind != walsink.AuxiliaryBackup {
		t.Errorf("kind = %d; want AuxiliaryBackup", kind)
	}
	wantKey := "wal/db1/00000001/" + base
	if key != wantKey {
		t.Errorf("key = %q; want %q", key, wantKey)
	}

	// Idempotent re-push: PG retried archive_command and we say
	// success without overwriting.
	if _, _, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{Deployment: "db1"}); err != nil {
		t.Fatalf("re-push: %v", err)
	}

	// Verify the body survived byte-for-byte.
	rc, err := sp.Get(context.Background(), wantKey)
	if err != nil {
		t.Fatalf("Get %q: %v", wantKey, err)
	}
	defer rc.Close()
	got, err := readAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(got, body) {
		t.Errorf("round-trip body mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// TestPushAuxiliaryFile_HistoryRoundTrip is the same coverage for
// the timeline-history path.  Realistic shape: one or two
// recovery-target lines per parent timeline.
func TestPushAuxiliaryFile_HistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	base := "00000002.history"
	src := filepath.Join(t.TempDir(), base)
	body := []byte("1\t13/FE000170\tno recovery target specified\n")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	key, kind, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{Deployment: "db1"})
	if err != nil {
		t.Fatalf("PushAuxiliaryFile: %v", err)
	}
	if kind != walsink.AuxiliaryHistory {
		t.Errorf("kind = %d; want AuxiliaryHistory", kind)
	}
	wantKey := "wal/db1/history/" + base
	if key != wantKey {
		t.Errorf("key = %q; want %q", key, wantKey)
	}
}

// TestPushAuxiliaryFile_PartialRoundTrip covers the partial-segment path: a
// standby promotion under archive_mode=always hands archive_command the
// timeline's final `.partial` segment. It must be STORED (next to its
// timeline's segments), never rejected — a rejection stalls PG's archiver on
// the retried file and fills the disk.
func TestPushAuxiliaryFile_PartialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	base := "000000010000000000000048.partial"
	src := filepath.Join(t.TempDir(), base)
	// 1 MiB — deliberately WAY past the 64 KiB .backup/.history cap, to prove
	// .partial gets the segment-size cap (a real partial is much larger than
	// a .backup; capping it at 64 KiB would reject it and stall the archiver).
	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i)
	}
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	key, kind, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{Deployment: "db1"})
	if err != nil {
		t.Fatalf("PushAuxiliaryFile(1 MiB .partial) must succeed (else the archiver stalls): %v", err)
	}
	if kind != walsink.AuxiliaryPartial {
		t.Errorf("kind = %d; want AuxiliaryPartial", kind)
	}
	wantKey := "wal/db1/00000001/" + base
	if key != wantKey {
		t.Errorf("key = %q; want %q (next to the timeline's segments)", key, wantKey)
	}
}

// TestPushAuxiliaryFile_LargePartialAccepted: a .partial can be up to a
// full WAL segment, and segments can now be larger than the 16 MiB
// default (a 64 MiB / 1 GiB cluster has correspondingly larger partials).
// So a partial past the OLD 16 MiB cap must be ACCEPTED — the cap is now
// the maximum possible segment size (1 GiB), which is what the aux path
// can't undercut without knowing the cluster's wal_segment_size.
func TestPushAuxiliaryFile_LargePartialAccepted(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	// 16 MiB + 1: would be refused under the old segment-size cap; now
	// accepted (a 64 MiB-segment cluster's partial is this big and more).
	src := filepath.Join(t.TempDir(), "000000010000000000000048.partial")
	if err := os.WriteFile(src, make([]byte, walsink.SegmentSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{Deployment: "db1"})
	if err != nil {
		t.Errorf("a partial larger than the 16 MiB default must be accepted now; got %v", err)
	}
}

// TestPushAuxiliaryFile_RefusesSegment guards against a caller
// routing a real WAL segment into the aux path — that would silently
// store 16 MiB unchunked, defeating the CAS and dedup story.
func TestPushAuxiliaryFile_RefusesSegment(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	src := filepath.Join(t.TempDir(), "000000010000000000000003")
	if err := os.WriteFile(src, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{Deployment: "db1"})
	if !errors.Is(err, walsink.ErrNotASegmentFile) {
		t.Errorf("expected ErrNotASegmentFile; got %v", err)
	}
}

// TestPushAuxiliaryFile_RefusesOversize bounds the in-memory read
// at MaxAuxiliaryFileSize.  PG's real .backup / .history payloads
// are < 1 KiB; anything past 64 KiB is refused so an operator
// error (e.g. routing the wrong file through wal push) doesn't
// quietly slurp arbitrary bytes into the repo.
func TestPushAuxiliaryFile_RefusesOversize(t *testing.T) {
	dir := t.TempDir()
	repoURL := "file://" + dir
	sp, _ := openFSRepo(t, repoURL)
	defer sp.Close()

	src := filepath.Join(t.TempDir(), "0000000100000000000000FF.000000D8.backup")
	big := make([]byte, walsink.MaxAuxiliaryFileSize+1)
	if err := os.WriteFile(src, big, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := walsink.PushAuxiliaryFile(context.Background(), sp, src, walsink.PushOptions{Deployment: "db1"})
	if err == nil || !strings.Contains(err.Error(), "aux-file cap") {
		t.Errorf("expected aux-file cap refusal; got %v", err)
	}
}

// readAll / bytesEqual: tiny helpers that keep the test imports the
// same as the rest of the file (no extra "io" / "bytes" lines).
func readAll(rc interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReadSystemIdentifierFromSegment_RejectsZeroSysID — a
// long-header page with xlp_sysid==0 is either a synthetic test
// fixture or a corrupt segment.  Stamping 0 on a manifest would
// later collide with every other corrupt-fixture-derived
// manifest; refusing here preserves the repo invariant that
// every manifest names a real cluster.
func TestReadSystemIdentifierFromSegment_RejectsZeroSysID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000000010000000000000005")

	body := make([]byte, walsink.SegmentSize)
	binary.LittleEndian.PutUint16(body[2:4], 0x0002)
	// xlp_sysid left zero
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := walsink.ReadSystemIdentifierFromSegment(path)
	if err == nil || !strings.Contains(err.Error(), "xlp_sysid=0") {
		t.Errorf("expected xlp_sysid=0 refusal; got %v", err)
	}
}

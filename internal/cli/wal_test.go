package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func newFsRepo(t *testing.T) (storage.StoragePlugin, string) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp, root
}

// putEmpty writes an empty object at key. Used to simulate committed
// segment manifests on disk without going through walsink.
func putEmpty(t *testing.T, sp storage.StoragePlugin, key string) {
	t.Helper()
	_, err := sp.Put(context.Background(), key, bytes.NewReader([]byte("{}")), storage.PutOptions{
		ContentLength: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// putRealSeg plants a real committed segment manifest (default 16 MiB)
// for tests that read the body back (resolveStartLSN → inventory).
func putRealSeg(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segNum uint64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	start := pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
	end := start + pglogrepl.LSN(walsink.SegmentSize)
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: "7000000000000000001",
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      name,
		StartLSN:         start.String(),
		EndLSN:           end.String(),
		SegmentSize:      walsink.SegmentSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

func TestParseSegmentNumber_RoundTripsWalsink(t *testing.T) {
	cases := []uint64{0, 1, 3, 256, 257, 1234, 0xCAFEBABE}
	for _, want := range cases {
		name := walsink.SegmentFileName(1, want, walsink.SegmentSize)
		got, ok := parseSegmentNumber(name)
		if !ok {
			t.Errorf("parseSegmentNumber(%q) failed", name)
			continue
		}
		if got != want {
			t.Errorf("name %q: got segNum %d, want %d", name, got, want)
		}
	}
}

func TestParseSegmentNumber_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",                          // empty
		"short",                     // wrong length
		"00000001000000000000000G",  // non-hex char
		"0000000100000000000000000", // 25 chars
	}
	for _, c := range cases {
		if _, ok := parseSegmentNumber(c); ok {
			t.Errorf("parseSegmentNumber(%q) should fail", c)
		}
	}
}

func TestHighestCommittedSegment_Empty(t *testing.T) {
	sp, _ := newFsRepo(t)
	_, found, err := highestCommittedSegment(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("found=true on empty repo; want false")
	}
}

func TestHighestCommittedSegment_PicksMaxSegmentNumber(t *testing.T) {
	sp, _ := newFsRepo(t)
	// Out-of-order placement on purpose so we know the implementation
	// doesn't rely on listing order.
	for _, segNum := range []uint64{0, 5, 2, 257, 256, 1} {
		key := fmt.Sprintf("wal/db1/00000001/%s.json", walsink.SegmentFileName(1, segNum, walsink.SegmentSize))
		putEmpty(t, sp, key)
	}
	got, found, err := highestCommittedSegment(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 257 {
		t.Errorf("max segment = %d, want 257", got)
	}
}

func TestHighestCommittedSegment_IgnoresTmpFilesAndOtherTimelines(t *testing.T) {
	sp, _ := newFsRepo(t)
	// One real commit on TLI 1.
	putEmpty(t, sp, "wal/db1/00000001/"+walsink.SegmentFileName(1, 3, walsink.SegmentSize)+".json")
	// In-flight tmp file on TLI 1 — must NOT be counted.
	putEmpty(t, sp, "wal/db1/00000001/"+walsink.SegmentFileName(1, 99, walsink.SegmentSize)+".json.tmp.abc123")
	// Real commit on a different timeline — must NOT bleed into TLI 1's count.
	putEmpty(t, sp, "wal/db1/00000002/"+walsink.SegmentFileName(2, 99, walsink.SegmentSize)+".json")
	// Garbage file with the right prefix but a non-canonical name —
	// must be ignored gracefully (forward-compat for future sidecar files).
	putEmpty(t, sp, "wal/db1/00000001/index.dat")

	got, found, err := highestCommittedSegment(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got != 3 {
		t.Errorf("max segment = %d, want 3 (tmp/other-timeline/garbage must be ignored)", got)
	}
}

func TestHighestCommittedSegment_PerDeploymentIsolation(t *testing.T) {
	sp, _ := newFsRepo(t)
	putEmpty(t, sp, "wal/db1/00000001/"+walsink.SegmentFileName(1, 5, walsink.SegmentSize)+".json")
	putEmpty(t, sp, "wal/db2/00000001/"+walsink.SegmentFileName(1, 99, walsink.SegmentSize)+".json")

	got, _, err := highestCommittedSegment(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Errorf("db1 max = %d, want 5 (db2 must not bleed)", got)
	}
}

func TestResolveStartLSN_FreshNoSlot_ReturnsZero(t *testing.T) {
	sp, _ := newFsRepo(t)
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1"}, 1, nil /* no slot info */)
	if err != nil {
		t.Fatal(err)
	}
	if lsn != 0 {
		t.Errorf("LSN = %v, want 0 on fresh-no-slot path", lsn)
	}
	if note != "fresh-no-slot-no-conn" {
		t.Errorf("note = %q, want fresh-no-slot-no-conn", note)
	}
}

func TestResolveStartLSN_FreshSlot_UsesRestartLSNAlignedDown(t *testing.T) {
	sp, _ := newFsRepo(t)
	// Slot's restart_lsn is mid-segment; the resume LSN must
	// align DOWN to the segment-start boundary because the
	// segment CONTAINING restart_lsn is what PG retains.
	// Aligning UP would push the start past PG's current flush
	// position on a quiet primary and surface as "requested
	// starting point X is ahead of the WAL flush position Y".
	// restart_lsn 0/3000800 sits inside segment 0/3000000 →
	// 0/4000000; aligned-down answer is 0/3000000.
	slot := &replication.SlotInfo{Name: "pg_hardstorage_db1", RestartLSN: "0/3000800"}
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1"}, 1, slot)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pglogrepl.ParseLSN("0/3000000")
	if lsn != want {
		t.Errorf("LSN = %v, want %v (restart_lsn aligned down to segment start)", lsn, want)
	}
	if note != "fresh-slot-restart-lsn" {
		t.Errorf("note = %q, want fresh-slot-restart-lsn", note)
	}
}

func TestResolveStartLSN_FreshSlot_RestartLSNOnSegmentBoundary(t *testing.T) {
	sp, _ := newFsRepo(t)
	// restart_lsn already on a segment boundary — no realignment
	// needed; the answer is restart_lsn itself.
	slot := &replication.SlotInfo{Name: "pg_hardstorage_db1", RestartLSN: "0/3000000"}
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1"}, 1, slot)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pglogrepl.ParseLSN("0/3000000")
	if lsn != want {
		t.Errorf("LSN = %v, want %v (restart_lsn already aligned)", lsn, want)
	}
	if note != "fresh-slot-restart-lsn" {
		t.Errorf("note = %q, want fresh-slot-restart-lsn", note)
	}
}

func TestResolveStartLSN_ResumeFromRepo(t *testing.T) {
	sp, _ := newFsRepo(t)
	// Three committed segments on TLI 1: 0, 1, 2.
	for _, n := range []uint64{0, 1, 2} {
		putRealSeg(t, sp, "db1", 1, n)
	}
	// Slot info present, restart_lsn at start-of-segment 1 —
	// the repo high-water-mark (end of segment 2 = 3*Seg) is
	// AHEAD of restart_lsn, so the safety check passes.
	slot := &replication.SlotInfo{
		Name:       "pg_hardstorage_db1",
		RestartLSN: pglogrepl.LSN(walsink.SegmentSize).String(),
	}
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1"}, 1, slot)
	if err != nil {
		t.Fatal(err)
	}
	want := pglogrepl.LSN(3 * walsink.SegmentSize)
	if lsn != want {
		t.Errorf("LSN = %v, want %v (end of segment 2)", lsn, want)
	}
	if note != "resume-from-repo" {
		t.Errorf("note = %q, want resume-from-repo", note)
	}
}

func TestResolveStartLSN_ResumeBehindSlot_Refuses(t *testing.T) {
	sp, _ := newFsRepo(t)
	// One committed segment in repo at segment 0 → end LSN
	// = 1*SegmentSize.  But the slot's restart_lsn has
	// already advanced past segment 5.  Resume from end of
	// segment 0 would ask PG for WAL it has long recycled —
	// the safety check must refuse with a typed error.
	putRealSeg(t, sp, "db1", 1, 0)
	slot := &replication.SlotInfo{
		Name:       "pg_hardstorage_db1",
		RestartLSN: pglogrepl.LSN(5 * walsink.SegmentSize).String(),
	}
	_, _, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1"}, 1, slot)
	if err == nil {
		t.Fatal("expected refusal when resume LSN is older than slot restart_lsn")
	}
	if !strings.Contains(err.Error(), "start_before_slot_restart_lsn") &&
		!strings.Contains(err.Error(), "older than the slot") {
		t.Errorf("error should reference the safety check; got %v", err)
	}
}

func TestResolveStartLSN_ExplicitFlagWins(t *testing.T) {
	sp, _ := newFsRepo(t)
	// Repo has an even higher segment, but the flag must win.
	putEmpty(t, sp, "wal/db1/00000001/"+walsink.SegmentFileName(1, 100, walsink.SegmentSize)+".json")
	// Slot restart_lsn at 0/2000000, well before the explicit
	// flag's 0/3000000 — safety check passes.
	slot := &replication.SlotInfo{Name: "pg_hardstorage_db1", RestartLSN: "0/2000000"}
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1", startLSN: "0/3000000"}, 1, slot)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pglogrepl.ParseLSN("0/3000000")
	if lsn != want {
		t.Errorf("LSN = %v, want %v", lsn, want)
	}
	if note != "explicit-flag" {
		t.Errorf("note = %q, want explicit-flag", note)
	}
}

func TestResolveStartLSN_ExplicitFlagBehindSlot_Refuses(t *testing.T) {
	sp, _ := newFsRepo(t)
	// Operator passed an explicit --start-lsn older than the
	// slot's restart_lsn.  PG would either reject the
	// START_REPLICATION or silently ignore us; the safety
	// check turns this into a typed error before we ever
	// open the streaming connection.
	slot := &replication.SlotInfo{Name: "pg_hardstorage_db1", RestartLSN: "0/5000000"}
	_, _, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1", startLSN: "0/3000000"}, 1, slot)
	if err == nil {
		t.Fatal("expected refusal when --start-lsn is older than slot restart_lsn")
	}
	if !strings.Contains(err.Error(), "older than the slot") {
		t.Errorf("error should reference the safety check; got %v", err)
	}
}

func TestResolveStartLSN_RejectsUnalignedFlag(t *testing.T) {
	sp, _ := newFsRepo(t)
	// 0/3000001 is one byte past a segment boundary.
	_, _, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1", startLSN: "0/3000001"}, 1, nil)
	if err == nil {
		t.Fatal("expected error on unaligned LSN")
	}
	if !strings.Contains(err.Error(), "segment-aligned") {
		t.Errorf("error should mention segment alignment; got %v", err)
	}
}

func TestResolveStartLSN_RejectsBadLSN(t *testing.T) {
	sp, _ := newFsRepo(t)
	_, _, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1", startLSN: "not-an-lsn"}, 1, nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "--start-lsn") {
		t.Errorf("error should mention the flag; got %v", err)
	}
}

// TestNoInactivityTimeoutFlag regresses issue #12: --no-inactivity-timeout
// must translate to a negative sentinel in the streaming.Reader options
// so the watchdog is disabled, not set to "use default".
//
// We exercise the flag-translation logic directly rather than spinning
// up a real stream; that's what the integration test covers.
func TestNoInactivityTimeoutFlag_TranslatesToNegativeSentinel(t *testing.T) {
	cases := []struct {
		name      string
		opts      walStreamOptions
		wantValue time.Duration
	}{
		{"flag-set-no-explicit-duration",
			walStreamOptions{noInactivityTimeout: true},
			-1},
		{"explicit-duration-wins-over-bool",
			walStreamOptions{noInactivityTimeout: true, inactivityTimeout: 30 * time.Second},
			30 * time.Second},
		{"neither-set-passes-zero-streaming-default",
			walStreamOptions{},
			0},
		{"explicit-duration-only",
			walStreamOptions{inactivityTimeout: 90 * time.Second},
			90 * time.Second},
	}
	for _, c := range cases {
		got := resolveInactivityTimeout(c.opts)
		if got != c.wantValue {
			t.Errorf("%s: resolveInactivityTimeout = %v, want %v",
				c.name, got, c.wantValue)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		cur, max, want time.Duration
	}{
		{time.Second, 30 * time.Second, 2 * time.Second},
		{2 * time.Second, 30 * time.Second, 4 * time.Second},
		{16 * time.Second, 30 * time.Second, 30 * time.Second}, // clamps at max
		{30 * time.Second, 30 * time.Second, 30 * time.Second}, // already at max
		{time.Second, 0, 2 * time.Second},                      // zero max → no clamp
	}
	for _, c := range cases {
		got := nextBackoff(c.cur, c.max)
		if got != c.want {
			t.Errorf("nextBackoff(%v, %v) = %v, want %v", c.cur, c.max, got, c.want)
		}
	}
}

func TestSleepBackoff_CtxCancelReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before the sleep finishes — sleep returns false so the
	// caller breaks out of the retry loop instead of looping forever.
	cancel()
	if sleepBackoff(ctx, 5*time.Second) {
		t.Error("sleepBackoff returned true on cancelled ctx; want false")
	}
}

func TestSleepBackoff_ZeroDurationDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	got := sleepBackoff(ctx, 0)
	if !got {
		t.Error("sleepBackoff(ctx, 0) returned false on healthy ctx")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("sleepBackoff(ctx, 0) blocked unexpectedly")
	}
}

func TestWalStreamResultBody_WriteText(t *testing.T) {
	body := walStreamResultBody{
		Deployment:    "db1",
		Slot:          "pg_hardstorage_db1",
		Timeline:      1,
		StartLSN:      "0/3000000",
		SyncedLSN:     "0/5000000",
		BytesAdvanced: 32 * 1024 * 1024,
		DurationMS:    12345,
		CleanStop:     true,
	}
	var buf bytes.Buffer
	if err := body.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"WAL stream stopped cleanly",
		"db1",
		"pg_hardstorage_db1",
		"0/3000000",
		"0/5000000",
		"32.0 MiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}

func TestWalStreamResultBody_WriteText_DirtyStop(t *testing.T) {
	body := walStreamResultBody{
		Deployment: "db1", Slot: "s", Timeline: 1,
		StartLSN: "0/0", SyncedLSN: "0/0", CleanStop: false,
	}
	var buf bytes.Buffer
	if err := body.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "terminated") {
		t.Errorf("dirty stop should say 'terminated'; got %s", buf.String())
	}
}

// Issue #76: stopping the stream before any complete 16 MiB segment
// has committed used to render a negative `bytes_advanced` (computed
// as `0 - startLSN` because the synced LSN was still 0).  The current
// renderer clamps to >= 0 and adds a `Received LSN` / `Received` pair
// so the operator sees that bytes WERE received but not yet committed.
func TestWalStreamResultBody_WriteText_MidSegmentStop(t *testing.T) {
	// Reporter's exact reproducer: started at 0x20000000, received
	// 159 KiB of WAL, no segment completed.  Pre-fix: BytesAdvanced
	// rendered as -512 MiB.  Post-fix: BytesAdvanced is 0, Received
	// is 159 KiB, with a clear "buffered, not committed" annotation.
	body := walStreamResultBody{
		Deployment:    "play",
		Slot:          "pg_hardstorage_play",
		Timeline:      1,
		StartLSN:      "0/20000000",
		SyncedLSN:     "0/0", // nothing committed
		ReceivedLSN:   "0/20027C00",
		BytesAdvanced: 0,
		BytesReceived: 159 * 1024,
		DurationMS:    325793,
		CleanStop:     true,
	}
	var buf bytes.Buffer
	if err := body.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// The negative-byte regression that issue #76 caught.
	if strings.Contains(out, "-") && strings.Contains(out, "B") {
		// Allow other dashes (e.g. em-dashes); just look for a
		// "-NNN B" or "-NNN MiB" style number.
		for _, bad := range []string{"-1 B", "-512 MiB", "-1.0 KiB", "Advanced:     -"} {
			if strings.Contains(out, bad) {
				t.Errorf("rendered negative byte count %q:\n%s", bad, out)
			}
		}
	}
	for _, want := range []string{
		"Start LSN:    0/20000000",
		"Synced LSN:   0/0",
		"Received LSN: 0/20027C00",
		"buffered, not committed",
		"PG resends on next stream",
		"Advanced:     0 B",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}

// When bytes_received == bytes_advanced (the happy path where every
// received segment also committed), the Received row should be
// suppressed so the summary stays compact.
func TestWalStreamResultBody_WriteText_HidesReceivedWhenEqual(t *testing.T) {
	body := walStreamResultBody{
		Deployment:    "db1",
		Slot:          "s",
		Timeline:      1,
		StartLSN:      "0/3000000",
		SyncedLSN:     "0/5000000",
		ReceivedLSN:   "0/5000000",
		BytesAdvanced: 32 * 1024 * 1024,
		BytesReceived: 32 * 1024 * 1024,
		CleanStop:     true,
	}
	var buf bytes.Buffer
	if err := body.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "Received LSN") || strings.Contains(out, "Received:") {
		t.Errorf("Received rows should be hidden when received == advanced; got:\n%s", out)
	}
}

func TestNewWalCmd_TreeShape(t *testing.T) {
	c := newWalCmd()
	if c.Use != "wal" {
		t.Errorf("Use = %q, want wal", c.Use)
	}
	want := map[string]bool{"stream": false, "push": false, "fetch": false, "list": false, "repair": false}
	for _, sub := range c.Commands() {
		// sub.Use is "verb <args>"; take the first word.
		name := strings.Fields(sub.Use)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for verb, seen := range want {
		if !seen {
			t.Errorf("`wal %s` is missing", verb)
		}
	}
}

func TestParseSegmentNameForFetch(t *testing.T) {
	// Round-trip via walsink.SegmentFileName for a few segment numbers.
	cases := []struct {
		name    string
		wantTLI uint32
		wantSeg uint64
		wantOK  bool
	}{
		{walsink.SegmentFileName(1, 0, walsink.SegmentSize), 1, 0, true},
		{walsink.SegmentFileName(1, 3, walsink.SegmentSize), 1, 3, true},
		{walsink.SegmentFileName(1, 256, walsink.SegmentSize), 1, 256, true},
		{walsink.SegmentFileName(0xABCDEF, 0xCAFEBABE, walsink.SegmentSize), 0xABCDEF, 0xCAFEBABE, true},
		// Bad inputs
		{"", 0, 0, false},
		{"short", 0, 0, false},
		{"00000001.history", 0, 0, false},          // history file (correctly rejected)
		{"00000001000000000000000G", 0, 0, false},  // non-hex
		{"0000000100000000000000000", 0, 0, false}, // 25 chars
	}
	for _, c := range cases {
		gotTLI, gotSeg, gotOK := parseSegmentNameForFetch(c.name)
		if gotOK != c.wantOK {
			t.Errorf("%q: ok=%v, want %v", c.name, gotOK, c.wantOK)
			continue
		}
		if !c.wantOK {
			continue
		}
		if gotTLI != c.wantTLI {
			t.Errorf("%q: tli=%#x, want %#x", c.name, gotTLI, c.wantTLI)
		}
		if gotSeg != c.wantSeg {
			t.Errorf("%q: seg=%d, want %d", c.name, gotSeg, c.wantSeg)
		}
	}
}

func TestNewWalStreamCmd_RequiresDeployment(t *testing.T) {
	c := newWalStreamCmd()
	c.SetArgs([]string{}) // no positional
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	err := c.Execute()
	if err == nil {
		t.Error("expected ExactArgs(1) failure")
	}
}

func TestResolveDurability(t *testing.T) {
	cases := []struct {
		in      string
		want    walsink.DurabilityMode
		wantErr bool
	}{
		{"", walsink.DurabilityPerSegment, false},
		{"per-segment", walsink.DurabilityPerSegment, false},
		{"per-chunk", walsink.DurabilityPerChunk, false},
		{"lazy", "", true},
		{"nonsense", "", true},
	}
	for _, c := range cases {
		got, err := resolveDurability(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v, wantErr = %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsPermanentStreamSetupError gates the issue-#79 fail-fast
// classification: only error codes whose remediation needs operator
// action should short-circuit the retry loop. Transient classes
// (connect.replication, pg.identify_failed, repo.list_failed) must
// stay on the retry path so a Patroni failover is survived.
func TestIsPermanentStreamSetupError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Permanent: operator must drop slot / pass --start-lsn.
		{
			name: "start_before_slot_restart_lsn (#79 reporter)",
			err: output.NewError("wal.start_before_slot_restart_lsn",
				"WAL recycled past resume LSN"),
			want: true,
		},
		// Permanent: slot has no restart_lsn → stale slot, needs drop.
		{
			name: "slot_no_restart_lsn",
			err:  output.NewError("wal.slot_no_restart_lsn", "empty"),
			want: true,
		},
		// Permanent: operator input class.
		{
			name: "bad_lsn",
			err:  output.NewError("usage.bad_lsn", "junk"),
			want: true,
		},
		{
			name: "unaligned_lsn",
			err:  output.NewError("usage.unaligned_lsn", "mid"),
			want: true,
		},
		// Transient: connection refused / network blip survives a
		// Patroni failover.
		{
			name: "connect.replication (transient)",
			err:  output.NewError("connect.replication", "refused"),
			want: false,
		},
		{
			name: "pg.identify_failed (transient)",
			err:  output.NewError("pg.identify_failed", "no rows"),
			want: false,
		},
		// Unstructured errors stay on the retry path — we only act
		// on codes we recognise.
		{
			name: "plain error",
			err:  fmt.Errorf("boom"),
			want: false,
		},
		// nil → false (predicate is total).
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isPermanentStreamSetupError(c.err)
			if got != c.want {
				t.Errorf("isPermanentStreamSetupError(%v) = %v, want %v",
					c.err, got, c.want)
			}
		})
	}
}

// TestWriteSegmentAtomically_VerifiesChunkOffsets pins the reassembly
// integrity check: chunks must form a contiguous, ascending, gap-free
// cover of the segment. WAL segment manifests aren't signed, so a
// reordered or gapped chunk list (corruption / tampering) would
// otherwise assemble individually-hash-valid bytes into the WRONG
// positions and still pass the total-size check — silently-corrupt WAL.
func TestWriteSegmentAtomically_VerifiesChunkOffsets(t *testing.T) {
	sp, _ := newFsRepo(t)
	cas := casdefault.New(sp)
	ctx := context.Background()
	a, err := cas.PutChunk(ctx, []byte("AAAA"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := cas.PutChunk(ctx, []byte("BBBB"))
	if err != nil {
		t.Fatal(err)
	}

	// Contiguous, in order → assembles correctly.
	good := &walsink.SegmentManifest{
		SegmentSize: 8,
		Chunks: []walsink.ChunkRef{
			{Hash: a.Hash, Offset: 0, Len: 4},
			{Hash: b.Hash, Offset: 4, Len: 4},
		},
	}
	target := filepath.Join(t.TempDir(), "seg")
	if err := writeSegmentAtomically(ctx, cas, good, target); err != nil {
		t.Fatalf("contiguous chunks should assemble: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "AAAABBBB" {
		t.Errorf("assembled = %q, want AAAABBBB", got)
	}

	// Out-of-order offsets (same total size) → refused.
	reordered := &walsink.SegmentManifest{
		SegmentSize: 8,
		Chunks: []walsink.ChunkRef{
			{Hash: a.Hash, Offset: 4, Len: 4}, // first chunk wrongly claims offset 4
			{Hash: b.Hash, Offset: 0, Len: 4},
		},
	}
	if err := writeSegmentAtomically(ctx, cas, reordered, filepath.Join(t.TempDir(), "s2")); err == nil ||
		!strings.Contains(err.Error(), "chunk_offset_mismatch") {
		t.Errorf("out-of-order offsets must be refused with chunk_offset_mismatch; got %v", err)
	}

	// A gap between chunks → refused.
	gapped := &walsink.SegmentManifest{
		SegmentSize: 12,
		Chunks: []walsink.ChunkRef{
			{Hash: a.Hash, Offset: 0, Len: 4},
			{Hash: b.Hash, Offset: 8, Len: 4}, // should be at offset 4
		},
	}
	if err := writeSegmentAtomically(ctx, cas, gapped, filepath.Join(t.TempDir(), "s3")); err == nil ||
		!strings.Contains(err.Error(), "chunk_offset_mismatch") {
		t.Errorf("gapped offsets must be refused with chunk_offset_mismatch; got %v", err)
	}
}

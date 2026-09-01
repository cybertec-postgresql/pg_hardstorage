package cli_test

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// plantWALSegment plants a segment manifest at the canonical
// wal/<dep>/<TLI>/<24-char>.json key.
//
// Gap detection reads only the KEY, but the body still has to be a real
// manifest: scanWALSegments reads one to learn the cluster's
// wal_segment_size, and every segment NUMBER is derived from it.
func plantWALSegment(t *testing.T, repoURL, deployment string, tli uint32, segNum uint64) {
	t.Helper()
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	segName := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	key := walsink.SegmentPath(deployment, tli, segName)
	// The schema string here used to read "pg_hardstorage.walsink.v1",
	// which is not the constant (walsink.Schema is
	// "pg_hardstorage.wal_segment.v1"). ParseSegmentManifest rejected
	// every one of these fixtures, so the whole `wal audit` suite was
	// silently exercising the "no manifest could be read" fallback
	// rather than the normal path — invisible while the fallback
	// happened to guess the same 16 MiB the fixture used. Build a real
	// manifest so these tests exercise what production does.
	body, merr := stdjson.Marshal(walsink.SegmentManifest{
		Schema:        walsink.Schema,
		Deployment:    deployment,
		Timeline:      tli,
		SegmentNumber: segNum,
		SegmentName:   segName,
		SegmentSize:   walsink.SegmentSize,
	})
	if merr != nil {
		t.Fatal(merr)
	}
	if _, err := sp.Put(context.Background(), key,
		bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("plant segment %s: %v", segName, err)
	}
}

// TestWalAudit_RequiresRepo: missing --repo is the cobra-level
// usage error.
func TestWalAudit_RequiresRepo(t *testing.T) {
	_, stderr, exit := runCmd(t, "wal", "audit", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit ExitMisuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", stderr)
	}
}

// TestWalAudit_EmptyDeployment: no WAL committed for the deployment
// → audit reports 0 segments + 0 gaps + exit 0. Cron-driven runs
// against pre-bootstrap deployments shouldn't alarm.
func TestWalAudit_EmptyDeployment(t *testing.T) {
	repoURL := initRepoForTest(t)
	stdout, _, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("empty deployment should exit OK; got %d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"gap_count": 0`) {
		t.Errorf("expected gap_count=0:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segment_count": 0`) {
		t.Errorf("expected segment_count=0:\n%s", stdout)
	}
}

// TestWalAudit_ContiguousNoGaps: plant a contiguous run of segments
// → 0 gaps + exit 0.
func TestWalAudit_ContiguousNoGaps(t *testing.T) {
	repoURL := initRepoForTest(t)
	for seg := uint64(0); seg < 5; seg++ {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	stdout, _, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("contiguous run should exit OK; got %d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"gap_count": 0`) {
		t.Errorf("expected gap_count=0:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"segment_count": 5`) {
		t.Errorf("expected segment_count=5:\n%s", stdout)
	}
}

// TestWalAudit_OneGap_ExitVerifyFailed: plant segments 0,1,3,4 →
// gap at #2; exit ExitVerifyFailed (9), error code
// verify.wal_gap_detected.
func TestWalAudit_OneGap_ExitVerifyFailed(t *testing.T) {
	repoURL := initRepoForTest(t)
	for _, seg := range []uint64{0, 1, 3, 4} {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	stdout, stderr, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("gap should exit ExitVerifyFailed (9); got %d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stderr, `"code": "verify.wal_gap_detected"`) {
		t.Errorf("expected verify.wal_gap_detected code in error:\n%s", stderr)
	}
	// The error message should list the missing range.
	if !strings.Contains(stderr, "TLI 1") || !strings.Contains(stderr, "missing") {
		t.Errorf("expected human-readable gap detail:\n%s", stderr)
	}
}

// TestWalAudit_MultipleGaps: plant segments 0,1,3,5,6 → two gaps
// (#2 and #4). Counters and human-readable output should reflect
// both.
func TestWalAudit_MultipleGaps(t *testing.T) {
	repoURL := initRepoForTest(t)
	for _, seg := range []uint64{0, 1, 3, 5, 6} {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	_, stderr, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("multiple gaps should exit ExitVerifyFailed; got %d\n%s", exit, stderr)
	}
	if !strings.Contains(stderr, "2 gap(s) totalling 2 missing segment(s)") {
		t.Errorf("expected '2 gap(s) totalling 2 missing segment(s)' in message:\n%s", stderr)
	}
}

// TestWalAudit_CrossTimelineNoFalsePositive: a TLI bump is NOT a gap.
//
// This test used to plant TLI 1 = {0,1,2} and TLI 2 = {5,6,7} and
// assert gap_count=0, on the rationale that the detector skips
// boundaries between different timelines. The rationale is right and
// the data was wrong: segments 3 and 4 were on neither timeline, so
// that layout is a genuine three-and-four-shaped hole that no PITR can
// cross. Asserting it was clean is what made `wal audit` — and the
// chaos soak's gap-free gate, which runs it — structurally blind to
// the one hole an HA deployment is most likely to have.
//
// The real no-false-positive case is a CONTIGUOUS handover, which is
// what a promotion actually produces: the new timeline's first archived
// segment is the one containing the branch LSN, so it lands at or just
// after the old timeline's last. That is what this now plants.
// Overlap and same-segment handover are covered as unit cases in
// wal_gaps_timeline_test.go.
func TestWalAudit_CrossTimelineNoFalsePositive(t *testing.T) {
	repoURL := initRepoForTest(t)
	// TLI 1: segments 0,1,2.
	for _, seg := range []uint64{0, 1, 2} {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	// TLI 2 picks up at #3 — the promotion lost nothing.
	for _, seg := range []uint64{3, 4, 5} {
		plantWALSegment(t, repoURL, "db1", 2, seg)
	}
	stdout, _, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("a contiguous handover across a TLI bump is NOT a gap; should exit OK. got %d\n%s",
			exit, stdout)
	}
	if !strings.Contains(stdout, `"gap_count": 0`) {
		t.Errorf("expected gap_count=0 for a contiguous handover:\n%s", stdout)
	}
}

// TestWalAudit_HoleStraddlingAPromotionIsDetected is the case the test
// above used to assert away.
//
// TLI 1 stops at #2, TLI 2 resumes at #5. Segments 3 and 4 are held
// nowhere. This is precisely the residue of a stream that resumed at
// the new leader's position instead of the previous timeline's
// frontier, and `wal audit` is the tool an operator reaches for to find
// out whether their archive is replayable.
func TestWalAudit_HoleStraddlingAPromotionIsDetected(t *testing.T) {
	repoURL := initRepoForTest(t)
	for _, seg := range []uint64{0, 1, 2} {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	for _, seg := range []uint64{5, 6, 7} {
		plantWALSegment(t, repoURL, "db1", 2, seg)
	}
	stdout, stderr, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("segments #3 and #4 are on neither timeline, so this archive cannot be "+
			"replayed across the promotion — `wal audit` must fail.\ngot exit %d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "verify.wal_gap_detected") {
		t.Errorf("expected verify.wal_gap_detected:\n%s", stderr)
	}
	// The straddle must be named: it points at a promotion that lost
	// WAL, which is a different cause and a different remedy than a
	// hole inside one timeline.
	if !strings.Contains(stderr, "1->2") {
		t.Errorf("the gap report does not name the timeline change:\n%s", stderr)
	}
}

// TestWalAudit_TimelineFilter: --timeline 2 only audits TLI 2; gaps
// on TLI 1 don't count.
func TestWalAudit_TimelineFilter(t *testing.T) {
	repoURL := initRepoForTest(t)
	// TLI 1: gappy. TLI 2: contiguous.
	for _, seg := range []uint64{0, 1, 3} {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	for _, seg := range []uint64{0, 1, 2} {
		plantWALSegment(t, repoURL, "db1", 2, seg)
	}
	// Default (no filter) sees TLI 1's gap → fails.
	_, _, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("default audit should see TLI 1 gap; got exit %d", exit)
	}
	// --timeline 2 only sees TLI 2 → OK.
	stdout2, _, exit2 := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL,
		"--timeline", "2", "-o", "json")
	if exit2 != int(output.ExitOK) {
		t.Errorf("filtered to TLI 2 should pass; got exit %d\n%s", exit2, stdout2)
	}
}

// TestWalAudit_TextRender confirms the operator-friendly text body
// has the punch-list format.
func TestWalAudit_TextRender(t *testing.T) {
	repoURL := initRepoForTest(t)
	for seg := uint64(0); seg < 3; seg++ {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	stdout, _, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"wal audit — db1",
		"Segments scanned: 3",
		"Timelines:",
		"TLI 1: 3 segments",
		"no gaps detected",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// TestWalAudit_AuditEmissionOnFinding: a gap-finding run writes a
// wal.gap_detected event to the audit chain. We assert the event is
// reachable via `audit list`.
func TestWalAudit_AuditEmissionOnFinding(t *testing.T) {
	repoURL := initRepoForTest(t)
	for _, seg := range []uint64{0, 1, 3, 4} {
		plantWALSegment(t, repoURL, "db1", 1, seg)
	}
	// Run the audit (will exit 9); we don't care about the output.
	runCmd(t, "wal", "audit", "db1", "--repo", repoURL, "-o", "json")

	// Walk the audit chain via the existing `audit search` command.
	stdout, _, exit := runCmd(t,
		"audit", "search",
		"--repo", repoURL,
		"--action", "wal.gap_detected",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "wal.gap_detected") {
		t.Errorf("expected wal.gap_detected in audit chain:\n%s", stdout)
	}
}

// TestWalAudit_SchemaStable: the JSON body carries the v1 schema
// string for forward compat.
func TestWalAudit_SchemaStable(t *testing.T) {
	repoURL := initRepoForTest(t)
	plantWALSegment(t, repoURL, "db1", 1, 0)
	plantWALSegment(t, repoURL, "db1", 1, 1)
	stdout, _, exit := runCmd(t,
		"wal", "audit", "db1", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	if !strings.Contains(string(body), `"schema":"pg_hardstorage.wal.audit.v1"`) {
		t.Errorf("schema field missing or wrong:\n%s", body)
	}
	for _, want := range []string{
		`"deployment":"db1"`,
		`"timelines":`,
		`"segment_count":2`,
		`"gap_count":0`,
		`"missing_total":0`,
		`"started_at":`,
		`"stopped_at":`,
		`"duration_ms":`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing field %q:\n%s", want, body)
		}
	}
}

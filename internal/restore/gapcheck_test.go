package restore

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// newGapTestSP builds a temp file:// SP for the gap tests.
func newGapTestSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// putGap is a small helper that writes one Record via the
// gapstate package. Real Coordinators populate this in
// production; in tests we synthesise.
func putGap(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, startLSN, endLSN string, bytes uint64, at time.Time) {
	t.Helper()
	s := gapstate.NewWithClock(sp, func() time.Time { return at })
	if _, err := s.Put(context.Background(), gapstate.Record{
		Deployment:  deployment,
		SlotName:    "test_slot",
		SlotRole:    "leader",
		Timeline:    tli,
		GapStartLSN: startLSN,
		GapEndLSN:   endLSN,
		GapBytes:    bytes,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPreflightWALGap_ManifestEmbeddedGapRefuses: the v0.6+
// "manifest-carried gap metadata" path. A target LSN that falls
// in a manifest-embedded gap MUST refuse even when live
// gapstate is empty (operator wiped state, or gap-record GC
// reaped the live record). The signed manifest is the durable
// source of truth.
func TestPreflightWALGap_ManifestEmbeddedGapRefuses(t *testing.T) {
	sp := newGapTestSP(t) // empty live gapstate

	manifestGaps := []backup.WALGap{{
		SlotName:    "pg_hardstorage_db1",
		SlotRole:    "leader",
		Timeline:    7,
		GapStartLSN: "0/3000028",
		GapEndLSN:   "0/30001A0",
		GapBytes:    420,
		DetectedAt:  time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
	}}
	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}
	err := preflightWALGap(context.Background(), sp, "db1", rec, manifestGaps, nil)
	if err == nil {
		t.Fatal("expected refusal for in-manifest-gap target")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "restore.target_in_wal_gap" {
		t.Errorf("expected restore.target_in_wal_gap; got %v", err)
	}
	// Source attribution: the error message should name the
	// manifest as the source so operators can distinguish
	// stale manifest-embedded gaps from live ones.
	if !strings.Contains(err.Error(), "source=manifest") {
		t.Errorf("error should attribute to manifest source; got %v", err)
	}
}

// TestPreflightWALGap_LiveGapAttributesAsLive: the live
// gapstate path's source attribution. Symmetric counterpart
// to the manifest test above.
func TestPreflightWALGap_LiveGapAttributesAsLive(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 7, "0/3000028", "0/30001A0", 420,
		time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))

	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}
	err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "source=live") {
		t.Errorf("error should attribute to live source; got %v", err)
	}
}

// TestPreflightWALGap_ManifestPrecedesLive: when both sources
// would refuse the same target, the manifest path fires first
// (we walk it before live gapstate). This isn't strictly
// observable beyond the source= attribution but matters for
// the case where the manifest gap's detection_at is older than
// the live one — a future "tell me which gap is canonical"
// surface would see the manifest as authoritative.
func TestPreflightWALGap_ManifestPrecedesLive(t *testing.T) {
	sp := newGapTestSP(t)
	// Live gap on TLI 7.
	putGap(t, sp, "db1", 7, "0/100", "0/200", 256,
		time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))

	manifestGaps := []backup.WALGap{{
		SlotName:    "old",
		Timeline:    7,
		GapStartLSN: "0/100", GapEndLSN: "0/200", GapBytes: 256,
		DetectedAt: time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
	}}
	rec := &Recovery{Enable: true, TargetLSN: "0/150"}
	err := preflightWALGap(context.Background(), sp, "db1", rec, manifestGaps, nil)
	if err == nil {
		t.Fatal("expected refusal")
	}
	// Manifest came first → source=manifest in the message.
	if !strings.Contains(err.Error(), "source=manifest") {
		t.Errorf("manifest path should fire first; got %v", err)
	}
}

// TestPreflightWALGap_SkipGapCheck_BypassesRefusal: the
// operator's explicit override. With SkipGapCheck=true a
// target that would otherwise refuse is allowed; the bypass
// emits a Notice event so post-incident review sees the
// choice was made.
func TestPreflightWALGap_SkipGapCheck_BypassesRefusal(t *testing.T) {
	sp := newGapTestSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, at)

	var captured []*output.Event
	emit := func(ev *output.Event) { captured = append(captured, ev) }

	// Without SkipGapCheck: target 0/150 refuses.
	rec := &Recovery{Enable: true, TargetLSN: "0/150"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, emit); err == nil {
		t.Fatal("baseline: expected refusal without SkipGapCheck")
	}
	captured = nil // reset for the override test

	// With SkipGapCheck: same target now allowed; Notice
	// event fires.
	rec.SkipGapCheck = true
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, emit); err != nil {
		t.Errorf("SkipGapCheck should bypass; got %v", err)
	}
	var found *output.Event
	for _, ev := range captured {
		if ev.Op == "wal_gap_check_skipped" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("expected wal_gap_check_skipped event; got ops=%v", opsOf(captured))
	}
	if found.Severity != output.SeverityNotice {
		t.Errorf("severity = %v, want SeverityNotice (audit-trail level)", found.Severity)
	}
}

// TestPreflightWALGap_SkipGapCheck_AlsoBypassesAdvisory: the
// override silences the time-target advisory too — a single
// flag flip kills the entire gap-pre-flight surface.
func TestPreflightWALGap_SkipGapCheck_AlsoBypassesAdvisory(t *testing.T) {
	sp := newGapTestSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, at)

	var captured []*output.Event
	emit := func(ev *output.Event) { captured = append(captured, ev) }

	rec := &Recovery{
		Enable:       true,
		TargetTime:   time.Now(),
		SkipGapCheck: true,
	}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, emit); err != nil {
		t.Errorf("SkipGapCheck + time target should not error; got %v", err)
	}
	for _, ev := range captured {
		if ev.Op == "wal_gap_advisory" {
			t.Errorf("SkipGapCheck should suppress wal_gap_advisory; got %+v", ev)
		}
	}
	// The audit-trail "skipped" event must still fire.
	var skipped *output.Event
	for _, ev := range captured {
		if ev.Op == "wal_gap_check_skipped" {
			skipped = ev
			break
		}
	}
	if skipped == nil {
		t.Fatalf("expected wal_gap_check_skipped; got ops=%v", opsOf(captured))
	}
}

// TestPreflightWALGap_SkipGapCheck_NoEmit: a programmatic
// caller (no emit callback) using SkipGapCheck still bypasses
// without panic. Defensive against callers that don't wire
// events.
func TestPreflightWALGap_SkipGapCheck_NoEmit(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())

	rec := &Recovery{
		Enable:       true,
		TargetLSN:    "0/150",
		SkipGapCheck: true,
	}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("SkipGapCheck with nil emit should not error; got %v", err)
	}
}

// TestPreflightWALGap_NoGap_NoRefusal: a clean repo with no
// gaps must not refuse any target.
func TestPreflightWALGap_NoGap_NoRefusal(t *testing.T) {
	sp := newGapTestSP(t)
	rec := &Recovery{
		Enable:    true,
		TargetLSN: "0/3000028",
	}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("no-gap should not refuse; got %v", err)
	}
}

// TestPreflightWALGap_TargetInGapRange_Refuses: the headline
// case. A target LSN inside [gap_start, gap_end) surfaces the
// structured refusal with a Suggestion pointing at
// `wal gaps` + `repair slot`.
func TestPreflightWALGap_TargetInGapRange_Refuses(t *testing.T) {
	sp := newGapTestSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 7, "0/3000028", "0/30001A0", 420, at)

	rec := &Recovery{
		Enable:    true,
		TargetLSN: "0/3000080", // mid-gap
	}
	err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil)
	if err == nil {
		t.Fatal("expected refusal for in-gap target")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) {
		t.Fatalf("expected *output.Error; got %T", err)
	}
	if oerr.Code != "restore.target_in_wal_gap" {
		t.Errorf("code = %q, want restore.target_in_wal_gap", oerr.Code)
	}
	if oerr.Suggestion == nil {
		t.Fatal("expected Suggestion populated")
	}
	if oerr.Suggestion.Command != "pg_hardstorage wal gaps db1" {
		t.Errorf("Suggestion.Command = %q", oerr.Suggestion.Command)
	}
	if oerr.Suggestion.DocURL == "" {
		t.Errorf("Suggestion.DocURL should link to the runbook")
	}
}

// TestPreflightWALGap_TargetAtGapStart_Refuses: the closed-
// boundary case. target == gap_start IS in the gap by our
// half-open convention. Pin so a future contract change
// surfaces.
func TestPreflightWALGap_TargetAtGapStart_Refuses(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())

	rec := &Recovery{Enable: true, TargetLSN: "0/100"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err == nil {
		t.Error("target at gap_start should refuse (closed boundary)")
	}
}

// TestPreflightWALGap_TargetAtGapEnd_Allows: the open-
// boundary case. target == gap_end is OUTSIDE the gap (PG's
// exclusive-end WAL convention).
func TestPreflightWALGap_TargetAtGapEnd_Allows(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())

	rec := &Recovery{Enable: true, TargetLSN: "0/200"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("target at gap_end should be allowed (exclusive end); got %v", err)
	}
}

// TestPreflightWALGap_TargetBeforeGap_Allows: a target before
// any recorded gap is fine (PITR within an earlier window
// completed before the failover).
func TestPreflightWALGap_TargetBeforeGap_Allows(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/3000028", "0/30001A0", 420, time.Now().UTC())

	rec := &Recovery{Enable: true, TargetLSN: "0/2000000"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("target before gap should be allowed; got %v", err)
	}
}

// TestPreflightWALGap_TargetAfterGap_Allows: target after a
// gap is fine — the gap is already past, the WAL after the gap
// is intact.
func TestPreflightWALGap_TargetAfterGap_Allows(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())

	rec := &Recovery{Enable: true, TargetLSN: "0/300"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("target after gap should be allowed; got %v", err)
	}
}

// TestPreflightWALGap_RecoveryDisabled_NoCheck: when Recovery
// isn't enabled, the function is a no-op.
func TestPreflightWALGap_RecoveryDisabled_NoCheck(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())

	rec := &Recovery{Enable: false, TargetLSN: "0/150"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("disabled recovery → no check; got %v", err)
	}
}

// TestPreflightWALGap_TimeTarget_GapsExist_Refuses: when the
// operator uses TargetTime + the deployment has recorded gaps,
// audit made the default fail-closed.  The previous
// behaviour (warn-and-proceed) is preserved as an opt-in
// (--skip-gap-check, covered in
// TestPreflightWALGap_TimeTarget_SkipGapCheck_BypassesRefusal).
//
// We assert that the refusal carries the structured error code +
// a Suggestion pointing at `wal gaps`.
func TestPreflightWALGap_TimeTarget_GapsExist_Refuses(t *testing.T) {
	sp := newGapTestSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, at)

	rec := &Recovery{Enable: true, TargetTime: time.Now()}
	err := preflightWALGap(context.Background(), sp, "db1", rec, nil, func(ev *output.Event) {})
	if err == nil {
		t.Fatal("expected refusal for time-target + recorded gap; got nil")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "restore.target_in_wal_gap" {
		t.Errorf("expected restore.target_in_wal_gap; got %v", err)
	}
	if oerr.Suggestion == nil || oerr.Suggestion.Command != "pg_hardstorage wal gaps db1" {
		t.Errorf("Suggestion.Command should point at wal gaps; got %+v", oerr.Suggestion)
	}
}

// TestPreflightWALGap_Suggestions_ReferenceRealFlags guards
// against a user-facing regression: the gap-check refusal and
// advisory both steer the operator toward the LSN-targeted flag
// "switch to --to-lsn to get a static check". The flag is
// --to-lsn (see internal/cli/restore.go); an earlier revision
// said --target-lsn, which does not exist — an operator copying
// it would hit "unknown flag". Assert the real flag is named and
// the phantom one never appears.
func TestPreflightWALGap_Suggestions_ReferenceRealFlags(t *testing.T) {
	sp := newGapTestSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, at)

	// (1) Time-target refusal suggestion.
	rec := &Recovery{Enable: true, TargetTime: time.Now()}
	var captured []*output.Event
	emit := func(ev *output.Event) { captured = append(captured, ev) }
	err := preflightWALGap(context.Background(), sp, "db1", rec, nil, emit)
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Suggestion == nil {
		t.Fatalf("expected structured error with suggestion; got %v", err)
	}
	if strings.Contains(oerr.Suggestion.Human, "--target-lsn") {
		t.Errorf("refusal suggestion references nonexistent --target-lsn flag:\n%s", oerr.Suggestion.Human)
	}
	if !strings.Contains(oerr.Suggestion.Human, "--to-lsn") {
		t.Errorf("refusal suggestion should name the real --to-lsn flag:\n%s", oerr.Suggestion.Human)
	}

	// (2) Advisory-warning suggestion (skip-gap-check path so the
	// refusal doesn't short-circuit before the warning is emitted).
	captured = nil
	recSkip := &Recovery{Enable: true, TargetName: "before-incident", SkipGapCheck: false}
	// Trigger the advisory via the warning emitter directly: the
	// refusal path returns before the advisory in the same call, so
	// exercise emitTimeTargetGapWarning to cover its suggestion text.
	emitTimeTargetGapWarning(context.Background(), sp, "db1", recSkip, nil, emit)
	var advisory *output.Event
	for _, ev := range captured {
		if ev.Op == "wal_gap_advisory" {
			advisory = ev
		}
	}
	if advisory == nil || advisory.Suggestion == nil {
		t.Fatalf("expected wal_gap_advisory event with suggestion; got %+v", captured)
	}
	if strings.Contains(advisory.Suggestion.Human, "--target-lsn") {
		t.Errorf("advisory suggestion references nonexistent --target-lsn flag:\n%s", advisory.Suggestion.Human)
	}
	if !strings.Contains(advisory.Suggestion.Human, "--to-lsn") {
		t.Errorf("advisory suggestion should name the real --to-lsn flag:\n%s", advisory.Suggestion.Human)
	}
}

// TestPreflightWALGap_TimeTarget_NoGaps_NoWarning: a clean
// deployment with no gaps + a time target → no warning event
// (no signal to surface).
func TestPreflightWALGap_TimeTarget_NoGaps_NoWarning(t *testing.T) {
	sp := newGapTestSP(t)

	var captured []*output.Event
	emit := func(ev *output.Event) { captured = append(captured, ev) }

	rec := &Recovery{Enable: true, TargetTime: time.Now()}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, emit); err != nil {
		t.Errorf("clean + time-target → allow; got %v", err)
	}
	for _, ev := range captured {
		if ev.Op == "wal_gap_advisory" {
			t.Errorf("clean deployment should NOT emit wal_gap_advisory; got %+v", ev)
		}
	}
}

// TestPreflightWALGap_TimeTarget_ManifestGapAlsoTriggers: a
// time-targeted PITR against a backup whose manifest carries
// embedded gaps (but live gapstate is empty) is REFUSED — audit
// v23 #1 changed the default from warn to refuse so an operator
// with a stale time target doesn't silently produce a corrupt
// PITR.  Operators override with --skip-gap-check.
func TestPreflightWALGap_TimeTarget_ManifestGapAlsoTriggers(t *testing.T) {
	sp := newGapTestSP(t) // empty live gapstate

	manifestGaps := []backup.WALGap{{
		SlotName: "x", Timeline: 1,
		GapStartLSN: "0/100", GapEndLSN: "0/200", GapBytes: 256,
		DetectedAt: time.Now().UTC(),
	}}

	rec := &Recovery{Enable: true, TargetName: "production-snap"}
	err := preflightWALGap(context.Background(), sp, "db1", rec, manifestGaps, func(ev *output.Event) {})
	if err == nil {
		t.Fatal("expected refusal for time-target + manifest gaps; got nil")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "restore.target_in_wal_gap" {
		t.Errorf("expected restore.target_in_wal_gap; got %v", err)
	}
}

// TestPreflightWALGap_TimeTarget_SkipGapCheck_BypassesRefusal:
// the --skip-gap-check escape hatch lets an operator who knows
// what they're doing proceed despite recorded gaps.  an internal audit
// fail-closed default; --skip-gap-check is the documented opt-out.
func TestPreflightWALGap_TimeTarget_SkipGapCheck_BypassesRefusal(t *testing.T) {
	sp := newGapTestSP(t)
	manifestGaps := []backup.WALGap{{
		SlotName: "x", Timeline: 1,
		GapStartLSN: "0/100", GapEndLSN: "0/200", GapBytes: 256,
		DetectedAt: time.Now().UTC(),
	}}

	rec := &Recovery{Enable: true, TargetName: "production-snap", SkipGapCheck: true}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, manifestGaps, func(ev *output.Event) {}); err != nil {
		t.Errorf("--skip-gap-check should bypass refusal; got %v", err)
	}
}

// TestPreflightWALGap_NoTarget_StillAllowed: an end-of-WAL
// (no-target) restore is NOT refused even when gaps exist — PG's
// own end-of-WAL semantics handle the gap-tail scenario
// correctly when the agent missed bytes near the end of WAL.
func TestPreflightWALGap_NoTarget_StillAllowed(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())
	rec := &Recovery{Enable: true /* no target */}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("no-target restore should allow even with gaps; got %v", err)
	}
}

// opsOf is a small test helper for human-readable failure
// messages — extracts the Op field from a slice of events.
func opsOf(evs []*output.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Op
	}
	return out
}

// TestPreflightWALGap_NoTargetLSN_SkipsCheck: TargetTime /
// TargetName paths used to *always* skip the static check (no
// LSN to compare).  An audit changed the default for
// time/name targets to fail-closed when gaps exist.  This test
// now asserts the surviving skip cases:
//
//   - End-of-WAL (no target set at all): still skipped — PG's
//     own end-of-WAL semantics handle the gap-tail scenario.
//   - Time / name targets with gaps recorded: now REFUSED.  See
//     the dedicated time/name refusal tests above.
//   - Time / name targets with no recorded gaps: still skipped,
//     because the deployment is clean.
func TestPreflightWALGap_NoTargetLSN_SkipsCheck(t *testing.T) {
	sp := newGapTestSP(t)
	// No gap planted — clean deployment.
	cases := []*Recovery{
		{Enable: true, TargetTime: time.Now()},
		{Enable: true, TargetName: "label"},
		{Enable: true /* no target at all */},
	}
	for i, rec := range cases {
		if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
			t.Errorf("case %d: clean deployment + non-LSN target should allow; got %v", i, err)
		}
	}
}

// TestPreflightWALGap_BadLSN_Refuses: a malformed target_lsn
// doesn't slip through; surface usage.bad_target_lsn so the
// operator sees the typo rather than letting PG's own opaque
// error fire later.
func TestPreflightWALGap_BadLSN_Refuses(t *testing.T) {
	sp := newGapTestSP(t)
	rec := &Recovery{Enable: true, TargetLSN: "not-an-lsn"}
	err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil)
	if err == nil {
		t.Fatal("expected error for bad LSN")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "usage.bad_target_lsn" {
		t.Errorf("expected usage.bad_target_lsn; got %v", err)
	}
}

// TestPreflightWALGap_MalformedRecordSkipped: a gapstate
// record with unparseable LSN strings should be quietly
// skipped (not a refusal, not a panic). Defensive.
func TestPreflightWALGap_MalformedRecordSkipped(t *testing.T) {
	sp := newGapTestSP(t)
	// Plant a record with a malformed LSN string by going
	// through gapstate (which doesn't validate LSN format).
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := gapstate.NewWithClock(sp, func() time.Time { return at })
	if _, err := s.Put(context.Background(), gapstate.Record{
		Deployment:  "db1",
		SlotName:    "x",
		Timeline:    1,
		GapStartLSN: "garbage",
		GapEndLSN:   "more-garbage",
		GapBytes:    100,
	}); err != nil {
		t.Fatal(err)
	}

	rec := &Recovery{Enable: true, TargetLSN: "0/100"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("malformed-record should be skipped; got %v", err)
	}
}

// TestPreflightWALGap_DeploymentScoped: a gap on db2 must
// not affect db1's restore.
func TestPreflightWALGap_DeploymentScoped(t *testing.T) {
	sp := newGapTestSP(t)
	at := time.Now().UTC()
	putGap(t, sp, "db2", 1, "0/100", "0/200", 256, at)

	rec := &Recovery{Enable: true, TargetLSN: "0/150"}
	// db1 should be unaffected by db2's gap.
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err != nil {
		t.Errorf("db2's gap should not affect db1; got %v", err)
	}
}

// TestPreflightWALGap_MultipleGapsAnyMatchRefuses: with N
// gaps recorded, any one matching → refusal. Sanity that the
// loop walks every record rather than only checking the
// newest.
func TestPreflightWALGap_MultipleGapsAnyMatchRefuses(t *testing.T) {
	sp := newGapTestSP(t)
	t1 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)

	// Three gaps; the middle one covers our target.
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, t1)
	putGap(t, sp, "db1", 2, "0/300", "0/400", 256, t2)
	putGap(t, sp, "db1", 3, "0/500", "0/600", 256, t3)

	rec := &Recovery{Enable: true, TargetLSN: "0/350"} // in gap 2
	err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil)
	if err == nil {
		t.Fatal("expected refusal — target falls in middle gap")
	}
	if !strings.Contains(err.Error(), "0/300..0/400") {
		t.Errorf("error should name the matching gap range; got %v", err)
	}
}

// TestPreflightWALGap_LSNComparisonIsNumeric: this is the
// regression guard from the v8 audit's "LSN compared as
// strings" hallucination. A target with leading zeros + a
// gap range without leading zeros must compare numerically
// (uint64) — string-comparison would put "0/200" > "0/3F"
// because the second char '/' < '/'... actually that's
// equal. Let me pick a real string-vs-numeric mismatch:
// "0/100" < "0/3F" lexicographically (since '1' < '3'),
// but numerically 0x100 (256) > 0x3F (63). With a target
// at 0x100 and a gap at [0x80, 0x3F] (which is malformed
// but…) actually this is hard to write. Just exercise
// hex parsing directly.
func TestPreflightWALGap_LSNComparisonIsNumeric(t *testing.T) {
	sp := newGapTestSP(t)
	// gap [0x80, 0x100): target 0xA0 is numerically inside
	// (160), but lexicographically "0/A0" > "0/100" (because
	// 'A' > '1'). If our compare were string-based, the
	// in-range check would fail and the test would NOT see
	// a refusal — but since we use pglogrepl.LSN (uint64),
	// the refusal fires.
	putGap(t, sp, "db1", 1, "0/80", "0/100", 0x80, time.Now().UTC())

	rec := &Recovery{Enable: true, TargetLSN: "0/A0"}
	if err := preflightWALGap(context.Background(), sp, "db1", rec, nil, nil); err == nil {
		t.Error("target 0/A0 (160) should refuse — within gap [0/80, 0/100) (128, 256)")
	}
}

// Sanity: make sure pglogrepl.LSN comparison still does what
// we expect after Go's potential future changes.
func TestPreflightWALGap_pglogreplLSNStillMonotonic(t *testing.T) {
	a, _ := pglogrepl.ParseLSN("0/100")
	b, _ := pglogrepl.ParseLSN("0/A0")
	if !(b < a) {
		t.Errorf("expected 0/A0 (%d) < 0/100 (%d)", b, a)
	}
}

// putSegManifest plants a real WAL segment manifest (default 16 MiB) for
// the contiguity test.
func putSegManifest(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segNum uint64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	start := pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: "7000000000000000001",
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      name,
		StartLSN:         start.String(),
		EndLSN:           (start + pglogrepl.LSN(walsink.SegmentSize)).String(),
		SegmentSize:      walsink.SegmentSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath(deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw), storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// TestPreflightWALContiguity_WarnsOnHole: a missing segment between the
// backup's stop point and an LSN target (with NO gap record) must emit a
// warning-only `restore.wal_archive_hole` event — never refuse.
func TestPreflightWALContiguity_WarnsOnHole(t *testing.T) {
	sp := newGapTestSP(t)
	// Archive segments 0, 1, 3 on TLI 7 — segment 2 is MISSING.
	for _, n := range []uint64{0, 1, 3} {
		putSegManifest(t, sp, "db1", 7, n)
	}
	m := &backup.Manifest{StopLSN: "0/800000", Timeline: 7} // stop is inside segment 0
	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}  // target inside segment 3

	var got []*output.Event
	emit := func(ev *output.Event) { got = append(got, ev) }
	preflightWALContiguity(context.Background(), sp, "db1", m, rec, emit)

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(got))
	}
	ev := got[0]
	if ev.Op != "wal_archive_hole" || ev.Severity != output.SeverityWarning {
		t.Errorf("event op/severity = %s/%s, want wal_archive_hole/warning", ev.Op, ev.Severity)
	}
	body := ev.Body.(map[string]any)
	if body["missing_from_lsn"] != "0/2000000" { // segment 2 start
		t.Errorf("missing_from_lsn = %v, want 0/2000000 (segment 2)", body["missing_from_lsn"])
	}
}

// TestPreflightWALContiguity_QuietWhenContiguous: no hole, non-LSN
// target, and disabled recovery must all emit nothing (no false alarms).
func TestPreflightWALContiguity_QuietWhenContiguous(t *testing.T) {
	sp := newGapTestSP(t)
	for _, n := range []uint64{0, 1, 2, 3} { // contiguous
		putSegManifest(t, sp, "db1", 7, n)
	}
	m := &backup.Manifest{StopLSN: "0/800000", Timeline: 7}

	count := 0
	emit := func(*output.Event) { count++ }

	// Contiguous archive → quiet.
	preflightWALContiguity(context.Background(), sp, "db1", m,
		&Recovery{Enable: true, TargetLSN: "0/3000080"}, emit)
	// Non-LSN target → out of scope, quiet.
	preflightWALContiguity(context.Background(), sp, "db1", m,
		&Recovery{Enable: true, TargetName: "before-upgrade"}, emit)
	// Recovery disabled → quiet.
	preflightWALContiguity(context.Background(), sp, "db1", m,
		&Recovery{Enable: false, TargetLSN: "0/3000080"}, emit)

	if count != 0 {
		t.Errorf("expected no events, got %d", count)
	}
}

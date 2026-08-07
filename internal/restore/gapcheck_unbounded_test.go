package restore

// gapcheck_unbounded_test.go — unbounded recovery over a recorded gap
// must refuse, not silently truncate.
//
// PG cannot distinguish a WAL hole from the genuine end of the
// archive: with recovery.signal and no target, it ends recovery at the
// first segment restore_command cannot supply, PROMOTES, and reports
// success arbitrarily far behind. The chaos gate's first boot-proof
// run caught exactly this — a backup taken before the streamer first
// started promoted cleanly missing the entire fault-window workload.
// The recorded gap (wal_prestream_gap.go, the coordinator's failover
// records) is the knowledge; this refusal is what turns it into a
// typed error at the only moment the operator can still choose a
// better backup.

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

func unboundedRecovery() *Recovery { return &Recovery{Enable: true, Timeline: "latest"} }

// TestPreflightWALGap_UnboundedOverLiveGap_Refuses is the finding.
func TestPreflightWALGap_UnboundedOverLiveGap_Refuses(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/4000000", GapEndLSN: "0/6000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}

	err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0" /* backup stops BELOW the gap: replay crosses it */, unboundedRecovery(), nil, nil)
	if err == nil {
		t.Fatal("unbounded recovery over a recorded gap was allowed.\n\n" +
			"PG ends recovery at the hole and promotes: the operator asked for everything " +
			"and silently got a prefix. The recorded gap existed precisely so this call " +
			"could refuse.")
	}
	if !strings.Contains(err.Error(), "target_in_wal_gap") {
		t.Errorf("wrong code: %v", err)
	}
	if oe, ok := output.AsOutputError(err); !ok || oe.Suggestion == nil ||
		!strings.Contains(oe.Suggestion.Human, "--skip-gap-check") {
		t.Errorf("refusal's suggestion does not name the override: %v", err)
	}
}

// TestPreflightWALGap_UnboundedGapBelowStop_Allowed: a gap ENTIRELY
// below the backup's stop is history this backup never replays.
// Refusing on it would block every restore from a newer backup forever
// — the false positive that would get this check disabled.
func TestPreflightWALGap_UnboundedGapBelowStop_Allowed(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/1000000", GapEndLSN: "0/2000000", GapBytes: 0x1000000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0" /* stop ABOVE the whole gap */, unboundedRecovery(), nil, nil); err != nil {
		t.Fatalf("a gap below the backup's stop refused a restore that never replays it: %v", err)
	}
}

// TestPreflightWALGap_UnboundedManifestGap_Refuses: the manifest-
// embedded record (signed, survives gapstate GC and replication) must
// gate the same way.
func TestPreflightWALGap_UnboundedManifestGap_Refuses(t *testing.T) {
	sp := gapTestRepo(t)
	gaps := []backup.WALGap{{GapStartLSN: "0/4000000", GapEndLSN: "0/6000000"}}
	err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", unboundedRecovery(), gaps, nil)
	if err == nil || !strings.Contains(err.Error(), "target_in_wal_gap") {
		t.Fatalf("manifest-embedded gap did not refuse unbounded recovery: %v", err)
	}
}

// TestPreflightWALGap_UnboundedSkipGapCheck_Bypasses: the operator's
// eyes-open override must keep working — recovery-to-the-hole is a
// legitimate salvage move when no newer backup exists.
func TestPreflightWALGap_UnboundedSkipGapCheck_Bypasses(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/4000000", GapEndLSN: "0/6000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}
	rec := unboundedRecovery()
	rec.SkipGapCheck = true
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", rec, nil, nil); err != nil {
		t.Fatalf("--skip-gap-check did not bypass: %v", err)
	}
}

// TestPreflightWALGap_UnboundedNoStopLSN_NoRefusal: with no stop to
// anchor the comparison (defensive: a caller passing ""), the check
// stays out of the way rather than guessing.
func TestPreflightWALGap_UnboundedNoStopLSN_NoRefusal(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/4000000", GapEndLSN: "0/6000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"", unboundedRecovery(), nil, nil); err != nil {
		t.Fatalf("empty stop LSN refused: %v", err)
	}
}

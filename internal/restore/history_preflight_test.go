package restore

// history_preflight_test.go — see history_preflight.go. The scenario
// staged here is the silent one: segments archived through TLI 3, the
// TLI-3 history file never archived. Without the preflight PG picks
// timeline 2 (first probe miss), replays to its end, promotes, and
// reports success — every TLI-3 segment ignored.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

func plantHistoryAux(t *testing.T, sp storage.StoragePlugin, tli uint32) {
	t.Helper()
	name := fmt.Sprintf("%08X.history", tli)
	body := []byte(fmt.Sprintf("%d\t0/4000000\tno particular reason\n", tli-1))
	key := walsink.AuxiliaryFilePath(gapTestDeployment, name, walsink.AuxiliaryHistory)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

func latestRecovery() *Recovery { return &Recovery{Enable: true, Timeline: "latest"} }

// threeTimelineArchive plants segments on TLIs 1..3 and the TLI-2
// history; TLI 3's history is deliberately absent unless a test adds
// it.
func threeTimelineArchive(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := gapTestRepo(t)
	plantSeg(t, sp, 1, 3)
	plantSeg(t, sp, 2, 4)
	plantSeg(t, sp, 3, 5)
	plantHistoryAux(t, sp, 2)
	return sp
}

// TestPreflightTimelineHistory_MissingHistory_Refuses is the finding.
func TestPreflightTimelineHistory_MissingHistory_Refuses(t *testing.T) {
	sp := threeTimelineArchive(t)
	err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, latestRecovery(), nil)
	if err == nil {
		t.Fatal("a --to-latest restore was allowed with 00000003.history unreachable.\n\n" +
			"PG probes history files ascending and stops at the first miss: recovery ends " +
			"on timeline 2, promotes, and reports success while every segment archived on " +
			"timeline 3 is silently ignored.")
	}
	if !strings.Contains(err.Error(), "timeline_history_unreachable") ||
		!strings.Contains(err.Error(), "00000003.history") {
		t.Errorf("refusal does not name the code and missing file: %v", err)
	}
	if oe, ok := output.AsOutputError(err); !ok || oe.Suggestion == nil ||
		!strings.Contains(oe.Suggestion.Human, "wal push") {
		t.Errorf("suggestion does not name the repair: %v", err)
	}
}

// TestPreflightTimelineHistory_IntermediateHoleRefuses: the TOP
// history being present is not enough — PG's ascending probe stops at
// the first miss, so a hole below the top caps recovery below it.
func TestPreflightTimelineHistory_IntermediateHoleRefuses(t *testing.T) {
	sp := gapTestRepo(t)
	plantSeg(t, sp, 1, 3)
	plantSeg(t, sp, 3, 5)
	plantHistoryAux(t, sp, 3) // top present, 2.history missing
	err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, latestRecovery(), nil)
	if err == nil {
		t.Fatal("intermediate history hole (00000002.history) not refused — PG stops " +
			"probing there and never reads the timeline-3 history that IS archived")
	}
	if !strings.Contains(err.Error(), "00000002.history") {
		t.Errorf("refusal does not name the intermediate hole: %v", err)
	}
}

// TestPreflightTimelineHistory_AuxComplete_Passes: full chain in the
// archive path → no refusal.
func TestPreflightTimelineHistory_AuxComplete_Passes(t *testing.T) {
	sp := threeTimelineArchive(t)
	plantHistoryAux(t, sp, 3)
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, latestRecovery(), nil); err != nil {
		t.Fatalf("complete history chain refused: %v", err)
	}
}

// TestPreflightTimelineHistory_TimelineStoreFallback_Passes: the
// streaming follower's timeline store is the second serve location —
// a `wal stream`-only HA deployment has its histories THERE, not in
// the archive path, and refusing it would break every such restore.
func TestPreflightTimelineHistory_TimelineStoreFallback_Passes(t *testing.T) {
	sp := threeTimelineArchive(t)
	if err := timeline.New(sp).Put(context.Background(), gapTestDeployment, 3,
		[]byte("2\t0/5000000\tstream-captured\n")); err != nil {
		t.Fatal(err)
	}
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, latestRecovery(), nil); err != nil {
		t.Fatalf("timeline-store history not honoured: %v", err)
	}
}

// TestPreflightTimelineHistory_SeedOnNewestTimeline_Passes: nothing
// above the seed → nothing to reach → no probes, no refusal.
func TestPreflightTimelineHistory_SeedOnNewestTimeline_Passes(t *testing.T) {
	sp := threeTimelineArchive(t)
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 3, latestRecovery(), nil); err != nil {
		t.Fatalf("seed already on the newest timeline refused: %v", err)
	}
}

// TestPreflightTimelineHistory_PinnedTimeline: a numeric --timeline
// needs that timeline's own history (it carries the full ancestry).
func TestPreflightTimelineHistory_PinnedTimeline(t *testing.T) {
	sp := threeTimelineArchive(t)
	rec := &Recovery{Enable: true, Timeline: "3"}
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, rec, nil); err == nil {
		t.Fatal("pinned --timeline 3 allowed without 00000003.history")
	}
	plantHistoryAux(t, sp, 3)
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, rec, nil); err != nil {
		t.Fatalf("pinned timeline with history present refused: %v", err)
	}
	// Pinned to the seed's own timeline: no history needed.
	rec = &Recovery{Enable: true, Timeline: "1"}
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, rec, nil); err != nil {
		t.Fatalf("pin to the seed's own timeline refused: %v", err)
	}
}

// TestPreflightTimelineHistory_SkipGapCheck_Bypasses: the recovery
// preflights share one eyes-open override.
func TestPreflightTimelineHistory_SkipGapCheck_Bypasses(t *testing.T) {
	sp := threeTimelineArchive(t)
	rec := latestRecovery()
	rec.SkipGapCheck = true
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, rec, nil); err != nil {
		t.Fatalf("--skip-gap-check did not bypass: %v", err)
	}
}

// TestPreflightTimelineHistory_EmptyArchive_Passes: no archived
// segments at all → nothing to reach.
func TestPreflightTimelineHistory_EmptyArchive_Passes(t *testing.T) {
	sp := gapTestRepo(t)
	if err := preflightTimelineHistory(context.Background(), sp, gapTestDeployment, 1, latestRecovery(), nil); err != nil {
		t.Fatalf("empty archive refused: %v", err)
	}
}

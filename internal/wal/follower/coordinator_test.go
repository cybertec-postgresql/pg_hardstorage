package follower_test

import (
	"context"
	"errors"
	"io"
	"net/url"
	"sync"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// recordingSink is a tiny test-helper that captures events for
// assertion. Concurrency-safe — Coordinator emits events from
// the polling goroutine.
type recordingSink struct {
	mu     sync.Mutex
	events []*output.Event
}

func (r *recordingSink) record(ev *output.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingSink) ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Op
	}
	return out
}

func (r *recordingSink) firstWithOp(op string) *output.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Op == op {
			return e
		}
	}
	return nil
}

// newTimelineStore builds a temp-fs-backed timeline store for tests.
func newTimelineStore(t *testing.T) *timeline.Store {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return timeline.New(sp)
}

// newGapStoreSP builds a temp-fs-backed StoragePlugin the gap-store tests share.
// Returned StoragePlugin is closed by t.Cleanup; callers wrap with gapstate.New.
func newGapStoreSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// Sanity-import the gapstate package so the new tests compile
// even if the per-test references are temporarily unused
// during refactors.
var _ = gapstate.Schema

// fakePatroniClient builds a Patroni REST client pointing at a
// no-op fake server so New() can be called without a real
// connection. The Coordinator's Run path is exercised separately.
func fakePatroniClient(t *testing.T) *patroni.Client {
	t.Helper()
	c, err := patroni.NewClient("http://127.0.0.1:1") // never reached
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestNew_ValidatesRequired: required-field checks at construction.
func TestNew_ValidatesRequired(t *testing.T) {
	store := newTimelineStore(t)
	client := fakePatroniClient(t)
	dsn := func(string, int) string { return "postgres://x" }

	cases := []struct {
		name string
		mut  func(o *follower.Options)
	}{
		{"nil-client", func(o *follower.Options) { o.Client = nil }},
		{"empty-slot", func(o *follower.Options) { o.SlotName = "" }},
		{"empty-deployment", func(o *follower.Options) { o.Deployment = "" }},
		{"nil-dsn-fn", func(o *follower.Options) { o.DSNFor = nil }},
		{"nil-timeline-store", func(o *follower.Options) { o.TimelineStore = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := follower.Options{
				Client:        client,
				SlotName:      "slot1",
				Deployment:    "db1",
				DSNFor:        dsn,
				TimelineStore: store,
			}
			c.mut(&opts)
			if _, err := follower.New(opts); err == nil {
				t.Errorf("%s: expected error", c.name)
			}
		})
	}
}

// TestHandleLeaderChange_NilNewEmitsLeaderGone: when a leader
// change event reports New=nil (no current leader in /cluster),
// we emit leader_gone and skip reconciliation. Defends against
// the WAL streaming consumer trying to connect to nothing.
func TestHandleLeaderChange_NilNewEmitsLeaderGone(t *testing.T) {
	rec := &recordingSink{}
	coord, err := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
		// Seams: should NOT be called when New=nil.
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			t.Errorf("ReconcileSlot must not be called when New=nil")
			return nil, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error {
			t.Errorf("CaptureTimelineHistory must not be called when New=nil")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		Old: &patroni.LeaderEndpoint{Name: "node-1", Host: "h1", Port: 5432, Timeline: 1, Role: "leader"},
		New: nil,
	})

	ops := rec.ops()
	wantOps := []string{"leader_change", "leader_gone"}
	if !equalStrSlice(ops, wantOps) {
		t.Errorf("ops = %v, want %v", ops, wantOps)
	}
}

// TestHandleLeaderChange_DSNBuildFailureSurfaces: an empty DSN
// from DSNFor surfaces a structured error event without
// invoking the seams. Covers the "config bug at the agent
// boundary" failure mode.
func TestHandleLeaderChange_DSNBuildFailureSurfaces(t *testing.T) {
	rec := &recordingSink{}
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "" }, // misconfigured
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 2, Role: "leader"},
	})
	if rec.firstWithOp("dsn_build_failed") == nil {
		t.Errorf("expected dsn_build_failed event; got ops=%v", rec.ops())
	}
}

// TestHandleLeaderChange_SlotFoundEmitsReconciled: the
// Strategy-A/B happy path. ReconcileSlot returns SlotFound +
// no gap; we emit slot_reconciled (notice level) and proceed
// to timeline capture.
func TestHandleLeaderChange_SlotFoundEmitsReconciled(t *testing.T) {
	rec := &recordingSink{}
	store := newTimelineStore(t)
	captureCalls := 0

	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: store,
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{Name: "slot1", RestartLSN: "0/30001A0"},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error {
			captureCalls++
			return pg.ErrNoHistoryForTLI1 // synthetic: TLI 1 path
		},
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "h1", Port: 5432, Timeline: 1, Role: "leader"},
	})

	if captureCalls != 1 {
		t.Errorf("CaptureTimelineHistory should be invoked once; got %d", captureCalls)
	}
	ops := rec.ops()
	wantContains := []string{"leader_change", "slot_reconciled", "timeline_no_history"}
	for _, want := range wantContains {
		found := false
		for _, op := range ops {
			if op == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing op %q in %v", want, ops)
		}
	}
	// Critical-severity wal_gap_detected MUST NOT fire on a
	// no-gap reconcile.
	if rec.firstWithOp("wal_gap_detected") != nil {
		t.Errorf("wal_gap_detected should not fire on SlotFound + no gap")
	}
}

// TestHandleLeaderChange_GapPersistsToGapStore: when a gap is
// detected, the Coordinator writes a gapstate.Record to the
// configured GapStore. Validates the v0.6+ "gap survives agent
// restart, becomes visible to doctor" property.
func TestHandleLeaderChange_GapPersistsToGapStore(t *testing.T) {
	rec := &recordingSink{}
	store := newTimelineStore(t)

	// Use the same temp file:// SP for the gap store so we can
	// re-read what the Coordinator persisted.
	gapStoreSP := newGapStoreSP(t)
	gs := gapstate.New(gapStoreSP)

	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: store,
		GapStore:      gs,
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome:          replication.SlotRecreated,
				Slot:             &replication.SlotInfo{Name: "slot1", RestartLSN: "0/30001A0"},
				GapBytes:         420,
				GapStartLSN:      pglogrepl.LSN(0x3000028),
				GapEndLSN:        pglogrepl.LSN(0x30001A0),
				LastConfirmedLSN: pglogrepl.LSN(0x3000028),
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})

	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 7, Role: "leader"},
	})

	// The structured event must have fired (existing
	// behaviour).
	if rec.firstWithOp("wal_gap_detected") == nil {
		t.Fatal("expected wal_gap_detected event")
	}
	// The persisted record must be readable.
	got, err := gs.List(context.Background(), "db1")
	if err != nil {
		t.Fatalf("List gaps: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(gaps) = %d, want 1", len(got))
	}
	r := got[0]
	if r.GapBytes != 420 {
		t.Errorf("GapBytes = %d, want 420", r.GapBytes)
	}
	if r.SlotName != "slot1" {
		t.Errorf("SlotName = %q, want slot1", r.SlotName)
	}
	if r.Timeline != 7 {
		t.Errorf("Timeline = %d, want 7", r.Timeline)
	}
}

// TestHandleLeaderChange_NoGapNoPersist: a no-gap reconcile
// does NOT write a gapstate record. Pin so a future regression
// doesn't accidentally persist every reconcile.
func TestHandleLeaderChange_NoGapNoPersist(t *testing.T) {
	rec := &recordingSink{}
	gapStoreSP := newGapStoreSP(t)
	gs := gapstate.New(gapStoreSP)

	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		GapStore:      gs,
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{Name: "slot1"},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})

	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "h1", Port: 5432, Timeline: 1, Role: "leader"},
	})

	got, err := gs.List(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 persisted gaps for no-gap reconcile; got %d", len(got))
	}
}

// TestHandleLeaderChange_GapEscalatesToCritical: when EnsureSlot
// returns a non-zero gap, the event severity is Critical and
// the event op is wal_gap_detected (drives PagerDuty etc.).
func TestHandleLeaderChange_GapEscalatesToCritical(t *testing.T) {
	rec := &recordingSink{}
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome:          replication.SlotRecreated,
				Slot:             &replication.SlotInfo{Name: "slot1", RestartLSN: "0/30001A0"},
				GapBytes:         420,
				GapStartLSN:      pglogrepl.LSN(0x3000028),
				GapEndLSN:        pglogrepl.LSN(0x30001A0),
				LastConfirmedLSN: pglogrepl.LSN(0x3000028),
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error {
			return pg.ErrNoHistoryForTLI1
		},
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 2, Role: "leader"},
	})

	ev := rec.firstWithOp("wal_gap_detected")
	if ev == nil {
		t.Fatalf("expected wal_gap_detected event; got ops=%v", rec.ops())
	}
	if ev.Severity != output.SeverityCritical {
		t.Errorf("severity = %v, want SeverityCritical", ev.Severity)
	}
	if ev.Suggestion == nil {
		t.Errorf("wal_gap_detected should carry a Suggestion (pointer to runbook)")
	} else {
		if ev.Suggestion.DocURL == "" {
			t.Errorf("Suggestion.DocURL should link to the runbook; got empty")
		}
		if ev.Suggestion.Command == "" {
			t.Errorf("Suggestion.Command should suggest the repair-slot CLI")
		}
	}
	body, _ := ev.Body.(map[string]any)
	if body["gap_bytes"] != uint64(420) {
		t.Errorf("body.gap_bytes = %v, want 420", body["gap_bytes"])
	}
}

// TestHandleLeaderChange_SlotReconcileFailureContinuesCapture:
// a failing slot reconcile shouldn't prevent timeline capture
// from running. The operator may want the .history file
// recorded while they investigate the slot issue.
func TestHandleLeaderChange_SlotReconcileFailureContinuesCapture(t *testing.T) {
	rec := &recordingSink{}
	captureCalls := 0
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return nil, errors.New("simulated failure")
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error {
			captureCalls++
			return nil
		},
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "h1", Port: 5432, Timeline: 2, Role: "leader"},
	})

	if rec.firstWithOp("slot_reconcile_failed") == nil {
		t.Errorf("expected slot_reconcile_failed event")
	}
	if captureCalls != 1 {
		t.Errorf("CaptureTimelineHistory should still run after slot failure; got %d calls", captureCalls)
	}
}

// TestHandleLeaderChange_TimelineCaptureSuccessEmitsEvent:
// successful capture path → timeline_captured event.
func TestHandleLeaderChange_TimelineCaptureSuccessEmitsEvent(t *testing.T) {
	rec := &recordingSink{}
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{Name: "slot1"},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 3, Role: "leader"},
	})
	ev := rec.firstWithOp("timeline_captured")
	if ev == nil {
		t.Fatalf("expected timeline_captured; ops=%v", rec.ops())
	}
	body, _ := ev.Body.(map[string]any)
	if body["timeline"] != uint32(3) {
		t.Errorf("body.timeline = %v, want 3", body["timeline"])
	}
}

// TestOptions_LastConfirmedLSNSignature: the v0.6+ callback
// shape takes a `patroni.LeaderEndpoint` so the agent's closure
// can scope its LSN lookup to the right timeline. Pin the
// signature so a future rename surfaces as a compile error
// here. The actual production-path invocation is exercised by
// integration tests (the unit-test ReconcileSlot seam bypasses
// the callback).
func TestOptions_LastConfirmedLSNSignature(t *testing.T) {
	var calls int
	opts := follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		LastConfirmedLSN: func(leader patroni.LeaderEndpoint) pglogrepl.LSN {
			calls++
			// Property the agent's closure relies on: leader
			// carries Timeline + Name so the closure can scope
			// an inventory.HighestArchivedLSN lookup.
			if leader.Timeline == 0 {
				t.Errorf("Timeline should be populated; got %+v", leader)
			}
			return pglogrepl.LSN(0x1234)
		},
	}
	if _, err := follower.New(opts); err != nil {
		t.Fatalf("New: %v", err)
	}
	// Manually exercise the closure to confirm the signature
	// matches without depending on the production reconcileSlot
	// path (which needs a real PG).
	got := opts.LastConfirmedLSN(patroni.LeaderEndpoint{Name: "node-1", Timeline: 7})
	if got != pglogrepl.LSN(0x1234) {
		t.Errorf("LastConfirmedLSN return = %v, want 0x1234", got)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestHandleLeaderChange_TimelineCaptureFailureSurfaces: a
// non-TLI-1 capture error surfaces as timeline_capture_failed.
// (TLI-1's pg.ErrNoHistoryForTLI1 sentinel is the expected-skip
// path, covered in SlotFoundEmitsReconciled.)
func TestHandleLeaderChange_TimelineCaptureFailureSurfaces(t *testing.T) {
	rec := &recordingSink{}
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{Outcome: replication.SlotFound, Slot: &replication.SlotInfo{}}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error {
			return errors.New("PG returned malformed .history")
		},
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 3, Role: "leader"},
	})
	ev := rec.firstWithOp("timeline_capture_failed")
	if ev == nil {
		t.Fatalf("expected timeline_capture_failed; ops=%v", rec.ops())
	}
	if ev.Severity != output.SeverityError {
		t.Errorf("severity = %v, want SeverityError", ev.Severity)
	}
}

// equalStrSlice is a small helper for ordered-slice comparison.
func equalStrSlice(a, b []string) bool {
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

// flakyPutSP wraps a StoragePlugin and fails the first failsLeft Put
// calls (simulating transient repo write failures), then delegates
// normally. Embedding promotes every other method unchanged.
type flakyPutSP struct {
	storage.StoragePlugin
	mu        sync.Mutex
	failsLeft int
	puts      int
}

func (f *flakyPutSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	f.mu.Lock()
	f.puts++
	if f.failsLeft > 0 {
		f.failsLeft--
		f.mu.Unlock()
		return storage.PutResult{}, errors.New("injected transient put failure")
	}
	f.mu.Unlock()
	return f.StoragePlugin.Put(ctx, key, r, opts)
}

func gapReconcile() func(context.Context, string) (*replication.SlotContinuityResult, error) {
	return func(context.Context, string) (*replication.SlotContinuityResult, error) {
		return &replication.SlotContinuityResult{
			Outcome:          replication.SlotRecreated,
			Slot:             &replication.SlotInfo{Name: "slot1", RestartLSN: "0/30001A0"},
			GapBytes:         420,
			GapStartLSN:      pglogrepl.LSN(0x3000028),
			GapEndLSN:        pglogrepl.LSN(0x30001A0),
			LastConfirmedLSN: pglogrepl.LSN(0x3000028),
		}, nil
	}
}

// TestPersistGap_RetriesTransientFailure: persistGap must retry a failed
// gap-record write (the gap is detected exactly once, so a lost record is
// lost forever) and persist EXACTLY ONE record despite the retry — the
// detection time is stamped once so every attempt targets the same key.
func TestPersistGap_RetriesTransientFailure(t *testing.T) {
	rec := &recordingSink{}
	flaky := &flakyPutSP{StoragePlugin: newGapStoreSP(t), failsLeft: 2} // 2 fail, 3rd succeeds
	gs := gapstate.New(flaky)
	coord, _ := follower.New(follower.Options{
		Client:                 fakePatroniClient(t),
		SlotName:               "slot1",
		Deployment:             "db1",
		DSNFor:                 func(string, int) string { return "postgres://x" },
		TimelineStore:          newTimelineStore(t),
		GapStore:               gs,
		OnEvent:                rec.record,
		ReconcileSlot:          gapReconcile(),
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})

	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 7, Role: "leader"},
	})

	got, err := gs.List(context.Background(), "db1")
	if err != nil {
		t.Fatalf("List gaps: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(gaps) = %d, want exactly 1 (retry must not duplicate)", len(got))
	}
	// The CRITICAL escalation must NOT have fired (it eventually succeeded).
	if ev := rec.firstWithOp("gap_persist_failed"); ev != nil {
		t.Errorf("gap_persist_failed should not fire when a retry succeeds; got %+v", ev)
	}
}

// TestPersistGap_EscalatesOnTotalFailure: when every attempt fails, the
// gap is unrecorded — persistGap must emit a CRITICAL gap_persist_failed
// event so the operator is alerted (restore preflight can no longer
// refuse a PITR into the gap).
func TestPersistGap_EscalatesOnTotalFailure(t *testing.T) {
	rec := &recordingSink{}
	flaky := &flakyPutSP{StoragePlugin: newGapStoreSP(t), failsLeft: 100} // all attempts fail
	gs := gapstate.New(flaky)
	coord, _ := follower.New(follower.Options{
		Client:                 fakePatroniClient(t),
		SlotName:               "slot1",
		Deployment:             "db1",
		DSNFor:                 func(string, int) string { return "postgres://x" },
		TimelineStore:          newTimelineStore(t),
		GapStore:               gs,
		OnEvent:                rec.record,
		ReconcileSlot:          gapReconcile(),
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})

	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 7, Role: "leader"},
	})

	got, _ := gs.List(context.Background(), "db1")
	if len(got) != 0 {
		t.Errorf("no record should persist on total failure; got %d", len(got))
	}
	ev := rec.firstWithOp("gap_persist_failed")
	if ev == nil {
		t.Fatal("expected a gap_persist_failed event on total failure")
	}
	if ev.Severity != output.SeverityCritical {
		t.Errorf("severity = %v, want SeverityCritical (operator must be alerted)", ev.Severity)
	}
}

// TestHandleLeaderChange_BackfillsMissingIntermediateHistories pins the
// streaming-only multi-promotion fix: observing a leader several TLIs
// above the last captured must backfill every intermediate <tli>.history,
// because PG's recovery_target_timeline='latest' discovery stops at the
// first missing one (capping recovery at a stale timeline).
func TestHandleLeaderChange_BackfillsMissingIntermediateHistories(t *testing.T) {
	rec := &recordingSink{}
	var captured []uint32
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{Outcome: replication.SlotFound, Slot: &replication.SlotInfo{Name: "slot1"}}, nil
		},
		CaptureTimelineHistory: func(_ context.Context, _ string, tli uint32) error {
			captured = append(captured, tli)
			return nil
		},
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 5, Role: "leader"},
	})
	got := map[uint32]bool{}
	for _, tli := range captured {
		got[tli] = true
	}
	for _, want := range []uint32{2, 3, 4, 5} { // leader TLI + every intermediate
		if !got[want] {
			t.Errorf("TLI %d history was not captured; captured=%v", want, captured)
		}
	}
	if got[1] {
		t.Errorf("TLI 1 has no history file and must not be captured; captured=%v", captured)
	}
	if rec.firstWithOp("timeline_backfilled") == nil {
		t.Errorf("expected a timeline_backfilled event; ops=%v", rec.ops())
	}
}

// TestHandleLeaderChange_BackfillSkipsAlreadyCaptured: backfill must not
// re-fetch an intermediate TLI whose .history is already committed (no
// redundant PG round-trip), while still capturing the genuinely-missing
// ones.
func TestHandleLeaderChange_BackfillSkipsAlreadyCaptured(t *testing.T) {
	rec := &recordingSink{}
	store := newTimelineStore(t)
	if err := store.Put(context.Background(), "db1", 3, []byte("3\t0/3000000\tno recovery target\n")); err != nil {
		t.Fatalf("pre-commit TLI 3: %v", err)
	}
	var captured []uint32
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		SlotName:      "slot1",
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: store,
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{Outcome: replication.SlotFound, Slot: &replication.SlotInfo{Name: "slot1"}}, nil
		},
		CaptureTimelineHistory: func(_ context.Context, _ string, tli uint32) error {
			captured = append(captured, tli)
			return nil
		},
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 5, Role: "leader"},
	})
	for _, tli := range captured {
		if tli == 3 {
			t.Errorf("TLI 3 is already committed; backfill must skip it; captured=%v", captured)
		}
	}
	got := map[uint32]bool{}
	for _, tli := range captured {
		got[tli] = true
	}
	for _, need := range []uint32{2, 4, 5} { // missing intermediates + leader TLI
		if !got[need] {
			t.Errorf("TLI %d should have been captured; captured=%v", need, captured)
		}
	}
}

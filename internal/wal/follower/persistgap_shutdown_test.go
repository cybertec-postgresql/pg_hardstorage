package follower_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// These tests pin the shutdown behaviour of persistGap: a WAL gap is
// detected exactly once (at slot recreation), so agent shutdown (ctx
// cancellation) racing failover handling must NOT silently drop the
// record. The old code's retry loop had `case <-ctx.Done(): return`,
// which lost the record without persisting it AND without emitting the
// CRITICAL gap_persist_failed escalation — permanently disarming restore
// preflight's refusal of PITR targets inside the gap.

// newGapCoordinator wires a Coordinator whose reconcile seam always
// reports the canonical 420-byte gap, persisting into gs.
func newGapCoordinator(t *testing.T, gs *gapstate.Store, rec *recordingSink) *follower.Coordinator {
	t.Helper()
	coord, err := follower.New(follower.Options{
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
	if err != nil {
		t.Fatal(err)
	}
	return coord
}

func fireGapLeaderChange(ctx context.Context, coord *follower.Coordinator) {
	coord.HandleLeaderChange(ctx, patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-2", Host: "h2", Port: 5432, Timeline: 7, Role: "leader"},
	})
}

// TestPersistGap_ShutdownPersistsViaDetachedContext: ctx is ALREADY
// cancelled when the gap is detected, but the store itself is healthy
// (the fs plugin fails a Put only because it is ctx-aware). The record
// must still land via the one final Put on a detached context, and no
// gap_persist_failed escalation fires.
func TestPersistGap_ShutdownPersistsViaDetachedContext(t *testing.T) {
	rec := &recordingSink{}
	gs := gapstate.New(newGapStoreSP(t)) // healthy, ctx-aware fs store
	coord := newGapCoordinator(t, gs, rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already in progress before the failover is handled
	fireGapLeaderChange(ctx, coord)

	got, err := gs.List(context.Background(), "db1")
	if err != nil {
		t.Fatalf("List gaps: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(gaps) = %d, want 1 (detached final attempt must persist the once-only record)", len(got))
	}
	if got[0].GapBytes != 420 {
		t.Errorf("GapBytes = %d, want 420", got[0].GapBytes)
	}
	if ev := rec.firstWithOp("gap_persist_failed"); ev != nil {
		t.Errorf("gap_persist_failed must not fire when the detached attempt succeeds; got %+v", ev)
	}
}

// TestPersistGap_ShutdownStoreDownEscalatesCritical: ctx is cancelled AND
// the store fails even on the detached final attempt. The record is
// genuinely lost, so the CRITICAL gap_persist_failed event MUST be
// emitted — silence here is the original bug.
func TestPersistGap_ShutdownStoreDownEscalatesCritical(t *testing.T) {
	rec := &recordingSink{}
	// flakyPutSP fails before delegating, regardless of ctx — so the
	// detached attempt fails too.
	flaky := &flakyPutSP{StoragePlugin: newGapStoreSP(t), failsLeft: 100}
	gs := gapstate.New(flaky)
	coord := newGapCoordinator(t, gs, rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fireGapLeaderChange(ctx, coord)

	got, _ := gs.List(context.Background(), "db1")
	if len(got) != 0 {
		t.Errorf("no record should persist when the store is down; got %d", len(got))
	}
	ev := rec.firstWithOp("gap_persist_failed")
	if ev == nil {
		t.Fatal("expected gap_persist_failed on shutdown with a failing store (old code exited silently)")
	}
	if ev.Op != "gap_persist_failed" {
		t.Errorf("Op = %q, want gap_persist_failed", ev.Op)
	}
	if ev.Severity != output.SeverityCritical {
		t.Errorf("severity = %v, want SeverityCritical (operator must record the gap by hand)", ev.Severity)
	}
}

// cancelAfterFirstPutSP fails the FIRST Put transiently (without
// consulting ctx) and schedules the shutdown cancel shortly after, so the
// retry loop's backoff select observes ctx.Done mid-retry. Subsequent
// Puts delegate to the (ctx-aware) wrapped plugin.
type cancelAfterFirstPutSP struct {
	storage.StoragePlugin
	cancel context.CancelFunc

	mu   sync.Mutex
	puts int
}

func (f *cancelAfterFirstPutSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	f.mu.Lock()
	f.puts++
	first := f.puts == 1
	f.mu.Unlock()
	if first {
		// Transient failure; shutdown begins moments later, while the
		// retry loop sits in its backoff sleep.
		time.AfterFunc(30*time.Millisecond, f.cancel)
		return storage.PutResult{}, errors.New("injected transient put failure")
	}
	return f.StoragePlugin.Put(ctx, key, r, opts)
}

// TestPersistGap_CancelMidRetryPersistsViaDetachedContext pins the exact
// original bug shape: the first Put fails transiently, then shutdown
// cancels ctx while the loop is backing off. The old `case <-ctx.Done():
// return` dropped the record; the fix's detached final attempt must
// persist it (the store is healthy by then).
func TestPersistGap_CancelMidRetryPersistsViaDetachedContext(t *testing.T) {
	rec := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp := &cancelAfterFirstPutSP{StoragePlugin: newGapStoreSP(t), cancel: cancel}
	gs := gapstate.New(sp)
	coord := newGapCoordinator(t, gs, rec)

	fireGapLeaderChange(ctx, coord)

	if err := ctx.Err(); err == nil {
		t.Fatal("test harness bug: ctx should have been cancelled during the retry loop")
	}
	got, err := gs.List(context.Background(), "db1")
	if err != nil {
		t.Fatalf("List gaps: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(gaps) = %d, want 1 (cancel mid-retry must not lose the record)", len(got))
	}
	if ev := rec.firstWithOp("gap_persist_failed"); ev != nil {
		t.Errorf("gap_persist_failed must not fire when the detached attempt succeeds; got %+v", ev)
	}
}

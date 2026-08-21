package audit

// append_progress_test.go — liveness of the optimistic append loop.
//
// Append claims its slot with an IfNotExists Put and, when it loses the
// race, relinks onto whoever won and tries the next sequence. That is a
// bounded retry ONLY while the winner's body reports the sequence its
// key encodes. A body that disagrees — a hand-edited event, a
// half-migrated legacy chain, an object restored into the wrong slot —
// sends the naive "seq = winner.Sequence + 1" straight back to the slot
// it just found occupied, forever, inside a goroutine holding no lock
// and answering no cancellation.
//
// Both tests below hang indefinitely against that older shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// stuckSlotSP models the pathological repo: every event-slot Put loses
// the race, and every event-slot Get answers with the same event body,
// whose Sequence is pinned at 0 no matter which slot was asked for.
type stuckSlotSP struct {
	storage.StoragePlugin
	staleBody []byte
	puts      int
	onPut     func()
}

func (s *stuckSlotSP) Capabilities() storage.Capabilities {
	return storage.Capabilities{ConditionalPut: true}
}

// isEventSlot excludes the head pointer, which Append reads and writes
// through the same plugin.
func isEventSlot(key string) bool {
	return strings.HasPrefix(key, "audit/") && key != HeadKey && strings.HasSuffix(key, ".json")
}

func (s *stuckSlotSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if isEventSlot(key) && opts.IfNotExists {
		s.puts++
		if s.onPut != nil {
			s.onPut()
		}
		return storage.PutResult{}, storage.ErrAlreadyExists
	}
	return s.StoragePlugin.Put(ctx, key, r, opts)
}

func (s *stuckSlotSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if isEventSlot(key) {
		return io.NopCloser(bytes.NewReader(s.staleBody)), nil
	}
	return s.StoragePlugin.Get(ctx, key)
}

func newStuckSlotStore(t *testing.T) *stuckSlotSP {
	t.Helper()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	// The occupant every slot reports: sequence 0, valid JSON, a plausible
	// hash. Relinking onto it naively computes "next = 1" from every slot,
	// including slot 1 itself.
	stale, err := json.Marshal(&Event{
		Schema:   Schema,
		ID:       "01STUCK",
		Sequence: 0,
		Action:   "backup.create",
		PrevHash: GenesisHash,
		Hash:     strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &stuckSlotSP{StoragePlugin: inner, staleBody: stale}
}

// TestAppend_StaleSequenceWinnerTerminates: a slot occupant whose body
// claims a sequence at or below the one we just tried must not stall the
// loop. Append gives up with a diagnosable error instead of spinning.
func TestAppend_StaleSequenceWinnerTerminates(t *testing.T) {
	sp := newStuckSlotStore(t)
	store := NewStore(sp)

	done := make(chan error, 1)
	go func() {
		done <- store.Append(context.Background(), &Event{Action: "backup.create"})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Append succeeded against a backend that never yields a slot")
		}
		if !strings.Contains(err.Error(), "gave up after") {
			t.Errorf("want the give-up diagnostic; got %v", err)
		}
		// One Put per attempt: the loop advanced rather than
		// re-probing one slot.
		if sp.puts < 2 {
			t.Errorf("attempted %d puts; the loop did not iterate", sp.puts)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Append did not terminate: the retry loop is unbounded")
	}
}

// TestAppend_HonoursContextCancellation: an operator (or a shutting-down
// agent) cancelling the context must stop the retry loop, not wait for
// the attempt cap.
func TestAppend_HonoursContextCancellation(t *testing.T) {
	sp := newStuckSlotStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel from inside the loop, after it has genuinely started
	// retrying.
	sp.onPut = func() {
		if sp.puts == 3 {
			cancel()
		}
	}
	store := NewStore(sp)

	done := make(chan error, 1)
	go func() {
		done <- store.Append(ctx, &Event{Action: "backup.create"})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled; got %v", err)
		}
		if sp.puts > 10 {
			t.Errorf("kept retrying after cancellation: %d puts", sp.puts)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Append ignored context cancellation")
	}
}

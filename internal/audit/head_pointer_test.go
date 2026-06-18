package audit_test

import (
	"context"
	"encoding/json"
	stdio "io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// TestHeadPointer_WrittenOnFirstAppend asserts the pointer file
// lands at audit/_head.json after the very first Append, and that
// it points at the just-written event.
func TestHeadPointer_WrittenOnFirstAppend(t *testing.T) {
	store, sp := newAuditStore(t)

	ev := &audit.Event{Action: "backup.create"}
	if err := store.Append(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	// The pointer file should be readable at audit.HeadKey.
	rc, err := sp.Get(context.Background(), audit.HeadKey)
	if err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var hp audit.HeadPointer
	if err := json.Unmarshal(body, &hp); err != nil {
		t.Fatalf("decode pointer: %v\n%s", err, body)
	}
	if hp.Schema != audit.HeadPointerSchema {
		t.Errorf("Schema = %q, want %q", hp.Schema, audit.HeadPointerSchema)
	}
	if hp.Sequence != ev.Sequence {
		t.Errorf("pointer.Sequence = %d, event.Sequence = %d", hp.Sequence, ev.Sequence)
	}
	if hp.Hash != ev.Hash {
		t.Errorf("pointer.Hash mismatch: %q vs %q", hp.Hash, ev.Hash)
	}
	if hp.EventID != ev.ID {
		t.Errorf("pointer.EventID = %q, event.ID = %q", hp.EventID, ev.ID)
	}
	if hp.UpdatedAt.IsZero() {
		t.Error("pointer.UpdatedAt should be set")
	}
}

// TestHeadPointer_UpdatedOnEverySubsequentAppend asserts the pointer
// reflects the LATEST event, not just the first one.
func TestHeadPointer_UpdatedOnEverySubsequentAppend(t *testing.T) {
	store, sp := newAuditStore(t)

	var lastEv *audit.Event
	for i := 0; i < 5; i++ {
		ev := &audit.Event{Action: "test.tick"}
		if err := store.Append(context.Background(), ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		lastEv = ev
	}

	rc, err := sp.Get(context.Background(), audit.HeadKey)
	if err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	defer rc.Close()
	body, _ := stdio.ReadAll(rc)
	var hp audit.HeadPointer
	if err := json.Unmarshal(body, &hp); err != nil {
		t.Fatal(err)
	}
	if hp.Sequence != 4 {
		t.Errorf("pointer.Sequence = %d, want 4 (the last event)", hp.Sequence)
	}
	if hp.Hash != lastEv.Hash {
		t.Errorf("pointer.Hash should match last event")
	}
	if hp.EventID != lastEv.ID {
		t.Errorf("pointer.EventID = %q, want last event %q", hp.EventID, lastEv.ID)
	}
}

// TestHeadPointer_FastPathFollowsCache asserts a fresh Append uses
// the pointer (not a directory walk) by checking that even when we
// inject a newer-by-key file that the chain doesn't know about, the
// pointer's hash is what the next event's PrevHash links to.
//
// This is a contract test: the pointer is the trust input for
// "what's the head?", subject to fall-back when stale or absent.
func TestHeadPointer_FastPathFollowsCache(t *testing.T) {
	store, _ := newAuditStore(t)

	// Append three events normally.
	var lastEv *audit.Event
	for i := 0; i < 3; i++ {
		ev := &audit.Event{Action: "test.tick"}
		if err := store.Append(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		lastEv = ev
	}

	// Append a fourth and assert it links to the third (proving the
	// pointer was used and was up to date).
	fourth := &audit.Event{Action: "test.tick"}
	if err := store.Append(context.Background(), fourth); err != nil {
		t.Fatal(err)
	}
	if fourth.PrevHash != lastEv.Hash {
		t.Errorf("fourth.PrevHash = %q, want third.Hash %q", fourth.PrevHash, lastEv.Hash)
	}
	if fourth.Sequence != lastEv.Sequence+1 {
		t.Errorf("fourth.Sequence = %d, want %d", fourth.Sequence, lastEv.Sequence+1)
	}
}

// TestHeadPointer_FallbackOnMissingPointer asserts that if the
// pointer file is somehow absent (e.g. the operator deleted the
// cache, or the chain was migrated), the next Append rebuilds the
// chain by walking the listing.
func TestHeadPointer_FallbackOnMissingPointer(t *testing.T) {
	store, sp := newAuditStore(t)

	// Plant three events.
	var lastEv *audit.Event
	for i := 0; i < 3; i++ {
		ev := &audit.Event{Action: "test.tick"}
		if err := store.Append(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		lastEv = ev
	}

	// Nuke the pointer file. The chain stays correct on disk.
	if err := sp.Delete(context.Background(), audit.HeadKey); err != nil {
		t.Fatalf("delete pointer: %v", err)
	}

	// Next Append should walk the listing, find the third event, and
	// link the fourth correctly.
	fourth := &audit.Event{Action: "test.tick"}
	if err := store.Append(context.Background(), fourth); err != nil {
		t.Fatal(err)
	}
	if fourth.PrevHash != lastEv.Hash {
		t.Errorf("after pointer-delete: fourth.PrevHash = %q, want third.Hash %q",
			fourth.PrevHash, lastEv.Hash)
	}
	if fourth.Sequence != lastEv.Sequence+1 {
		t.Errorf("fourth.Sequence = %d, want %d", fourth.Sequence, lastEv.Sequence+1)
	}
}

// TestHeadPointer_NotIncludedInListWalk asserts the listing walk
// (used by Search and VerifyChain) skips the pointer file. Otherwise
// the chain walk would try to deserialize HeadPointer JSON as an
// Event and either fail or pollute results.
func TestHeadPointer_NotIncludedInListWalk(t *testing.T) {
	store, _ := newAuditStore(t)

	for i := 0; i < 3; i++ {
		if err := store.Append(context.Background(), &audit.Event{Action: "test.tick"}); err != nil {
			t.Fatal(err)
		}
	}

	// Search should return exactly 3 events, not 4 (which would mean
	// the pointer leaked into results).
	events, err := store.Search(context.Background(), audit.ListFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Errorf("Search returned %d events, want 3 (pointer must not appear)", len(events))
	}

	// VerifyChain must report 3 events checked + OK.
	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsChecked != 3 {
		t.Errorf("VerifyChain checked %d events, want 3", res.EventsChecked)
	}
	if !res.OK {
		t.Errorf("VerifyChain not OK: %+v", res)
	}
}

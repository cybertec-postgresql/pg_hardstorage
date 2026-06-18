package audit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// TestStorageBackedLog_PutGetRoundTrip exercises the basic
// publish-and-readback flow against the file-backed transparency
// log impl.
func TestStorageBackedLog_PutGetRoundTrip(t *testing.T) {
	store, sp := newAuditStore(t)
	if err := store.Append(context.Background(), &audit.Event{Action: "test.tick"}); err != nil {
		t.Fatal(err)
	}

	log := audit.NewStorageBackedLog(sp)
	a, err := store.Anchor(context.Background(), log, "publisher-A")
	if err != nil {
		t.Fatalf("Anchor: %v", err)
	}
	if a.LogID == "" {
		t.Error("LogID should be populated after Anchor")
	}
	if a.ChainHeadHash == "" {
		t.Error("ChainHeadHash should be set")
	}
	if a.HeadSequence != 0 {
		t.Errorf("HeadSequence = %d, want 0 (single event)", a.HeadSequence)
	}

	got, err := log.GetAnchor(context.Background(), a.LogID)
	if err != nil {
		t.Fatalf("GetAnchor: %v", err)
	}
	if got.ChainHeadHash != a.ChainHeadHash {
		t.Errorf("ChainHeadHash mismatch")
	}
	if got.PublisherID != "publisher-A" {
		t.Errorf("PublisherID = %q", got.PublisherID)
	}
}

// TestStorageBackedLog_AnchorIsIdempotent — anchoring the same chain
// head twice returns the same log ID and doesn't create a second
// entry. Important for the "anchor every hour from cron" use case
// where the chain may not have moved between runs.
func TestStorageBackedLog_AnchorIsIdempotent(t *testing.T) {
	store, sp := newAuditStore(t)
	if err := store.Append(context.Background(), &audit.Event{Action: "test.tick"}); err != nil {
		t.Fatal(err)
	}
	log := audit.NewStorageBackedLog(sp)

	first, err := store.Anchor(context.Background(), log, "node-A")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Anchor(context.Background(), log, "node-B")
	if err != nil {
		t.Fatal(err)
	}
	if first.LogID != second.LogID {
		t.Errorf("re-anchor should yield same LogID; %q vs %q", first.LogID, second.LogID)
	}
}

// TestStorageBackedLog_LatestAnchor returns the highest-Sequence
// anchor. After two appends + two anchors, Latest is the second.
func TestStorageBackedLog_LatestAnchor(t *testing.T) {
	store, sp := newAuditStore(t)
	if err := store.Append(context.Background(), &audit.Event{Action: "first"}); err != nil {
		t.Fatal(err)
	}
	log := audit.NewStorageBackedLog(sp)
	if _, err := store.Anchor(context.Background(), log, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), &audit.Event{Action: "second"}); err != nil {
		t.Fatal(err)
	}
	a2, err := store.Anchor(context.Background(), log, "")
	if err != nil {
		t.Fatal(err)
	}

	latest, err := log.LatestAnchor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil {
		t.Fatal("LatestAnchor returned nil with two anchors present")
	}
	if latest.HeadSequence != a2.HeadSequence {
		t.Errorf("LatestAnchor.HeadSequence = %d, want %d", latest.HeadSequence, a2.HeadSequence)
	}
}

// TestVerifyAnchor_HappyPath: post an anchor, walk the chain via
// VerifyAnchor, expect OK=true and matching hashes.
func TestVerifyAnchor_HappyPath(t *testing.T) {
	store, sp := newAuditStore(t)
	for i := 0; i < 3; i++ {
		if err := store.Append(context.Background(), &audit.Event{Action: "tick"}); err != nil {
			t.Fatal(err)
		}
	}
	log := audit.NewStorageBackedLog(sp)
	a, err := store.Anchor(context.Background(), log, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := store.VerifyAnchor(context.Background(), log, a.LogID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("VerifyAnchor: not OK: %+v", res)
	}
	if res.LocalHeadHash != a.ChainHeadHash {
		t.Errorf("LocalHeadHash %q != ChainHeadHash %q", res.LocalHeadHash, a.ChainHeadHash)
	}
}

// TestVerifyAnchor_DetectsTamperedHead — the trust-foundation test:
// after anchoring, simulate a forger by writing a "new" event that
// claims the same sequence as an anchored one. VerifyAnchor must
// flag the mismatch.
//
// We simulate the tamper by deleting the chain (which is what a
// rewrite would look like before the forger replays new events) and
// asserting the verify reports a chain shorter than the anchor.
func TestVerifyAnchor_DetectsShortChain(t *testing.T) {
	store, sp := newAuditStore(t)
	for i := 0; i < 3; i++ {
		if err := store.Append(context.Background(), &audit.Event{Action: "tick"}); err != nil {
			t.Fatal(err)
		}
	}
	log := audit.NewStorageBackedLog(sp)
	a, err := store.Anchor(context.Background(), log, "")
	if err != nil {
		t.Fatal(err)
	}

	// Tamper: delete every chain event but keep the anchor + head
	// pointer untouched. The anchor still points at sequence=2; the
	// chain is now empty.
	for info, lerr := range sp.List(context.Background(), "audit/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		// Keep the anchor + head pointer; nuke the chain events.
		if strings.HasPrefix(info.Key, "audit/anchors/") || info.Key == "audit/_head.json" {
			continue
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		if err := sp.Delete(context.Background(), info.Key); err != nil {
			t.Fatal(err)
		}
	}

	res, err := store.VerifyAnchor(context.Background(), log, a.LogID)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Errorf("VerifyAnchor should NOT be OK after the chain was truncated; got %+v", res)
	}
	if res.Mismatch == "" {
		t.Errorf("Mismatch should describe the issue: %+v", res)
	}
	if !strings.Contains(res.Mismatch, "shorter") {
		t.Errorf("Mismatch should mention chain length: %q", res.Mismatch)
	}
}

// TestAnchor_RefusesEmptyChain: anchoring an empty repo is a clear
// misuse — surface a structured error.
func TestAnchor_RefusesEmptyChain(t *testing.T) {
	store, sp := newAuditStore(t)
	log := audit.NewStorageBackedLog(sp)
	_, err := store.Anchor(context.Background(), log, "")
	if err == nil {
		t.Fatal("expected error anchoring an empty chain")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty chain: %v", err)
	}
}

// TestVerifyChain_IgnoresAnchorRecords pins the fix for the chain-
// walker that picked up audit/anchors/*.json as if those files were
// audit events.  Pre-fix, the loader unmarshaled the anchor JSON
// (schema `pg_hardstorage.audit.anchor.v1`) into a zero-valued
// audit.Event and (1) inflated the Search count by 1 per anchor,
// (2) broke VerifyChain because the zero event's empty PrevHash
// didn't link to the real tail event's Hash.  Symptom in the wild:
// after `pg_hardstorage audit anchor` ran once on a healthy chain,
// `audit verify-chain` reported
// `verify.audit_chain_broken: 1 hash mismatch(es), 1 chain break(s)`
// on the very next invocation — surfaced by
// L8_audit_anchor_round_trip.
func TestVerifyChain_IgnoresAnchorRecords(t *testing.T) {
	store, sp := newAuditStore(t)

	// Three events to make the chain non-trivial.
	for i := 0; i < 3; i++ {
		if err := store.Append(context.Background(), &audit.Event{
			Action: "test.tick",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Pre-anchor: chain must be green.
	if res, err := store.VerifyChain(context.Background()); err != nil {
		t.Fatalf("VerifyChain pre-anchor: %v", err)
	} else if !res.OK {
		t.Fatalf("chain should be green pre-anchor; got %+v", res)
	}

	// Write an anchor.
	log := audit.NewStorageBackedLog(sp)
	if _, err := store.Anchor(context.Background(), log, "test-publisher"); err != nil {
		t.Fatalf("Anchor: %v", err)
	}

	// Append one more event AFTER the anchor — this is what
	// surfaced the bug in the field (the post-anchor event
	// reads the chain to find PrevHash; if the walker picked up
	// the anchor file as a chain event with empty hash, the
	// post-anchor event would link to the wrong predecessor).
	if err := store.Append(context.Background(), &audit.Event{
		Action: "test.post-anchor",
	}); err != nil {
		t.Fatalf("post-anchor append: %v", err)
	}

	// VerifyChain must still be green — the anchor file is a
	// sidecar, not a chain event, and the walker MUST skip it.
	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatalf("VerifyChain post-anchor: %v", err)
	}
	if !res.OK {
		t.Errorf("chain should remain green after anchor + 1 event; got %+v", res)
	}
}

// TestSearch_IgnoresAnchorRecords: companion to the VerifyChain
// test above.  Search shares the same allKeysSorted walker, so the
// same bug surfaced as a phantom zero-valued event in the search
// results.
func TestSearch_IgnoresAnchorRecords(t *testing.T) {
	store, sp := newAuditStore(t)
	if err := store.Append(context.Background(), &audit.Event{
		Action: "backup.create",
	}); err != nil {
		t.Fatal(err)
	}
	log := audit.NewStorageBackedLog(sp)
	if _, err := store.Anchor(context.Background(), log, ""); err != nil {
		t.Fatal(err)
	}

	events, err := store.Search(context.Background(), audit.ListFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Errorf("Search returned %d events, want 1 (anchor sidecar must be filtered): %+v",
			len(events), events)
	}
}

package audit_test

// verify_anchor_recompute_test.go — the anchor check asked the event
// whether it had been tampered with.
//
// VerifyAnchor's whole purpose is proving the local chain has not been
// rewritten since it was externally witnessed. It compared
// anchor.ChainHeadHash against ev.Hash — a field read out of the stored
// JSON. So rewriting an event's content and leaving its Hash field at
// the anchored value passed:
//
//	before rewrite:  VerifyAnchor ok=true
//	action changed:  VerifyAnchor ok=true   mismatch=""
//	                 VerifyChain  ok=false  hashMismatches=1
//
// VerifyChain recomputes and caught it. VerifyAnchor — the externally
// witnessed check, the strongest claim the system makes — did not. The
// weaker guarantee was the one with the stronger evidence behind it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// headEventKey returns the key of the highest-sequence event object.
func headEventKey(t *testing.T, sp storage.StoragePlugin) string {
	t.Helper()
	var best string
	for info, err := range sp.List(context.Background(), "audit/") {
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(info.Key, ".json") ||
			strings.Contains(info.Key, "anchors") ||
			strings.Contains(info.Key, "_head") ||
			info.Key == audit.HeadKey {
			continue
		}
		if info.Key > best {
			best = info.Key
		}
	}
	if best == "" {
		t.Fatal("no event objects found")
	}
	return best
}

// rewriteEventKeepingHash changes an event's content in place while
// leaving its recorded Hash untouched — the lie a stored-field
// comparison cannot see.
func rewriteEventKeepingHash(t *testing.T, sp storage.StoragePlugin, key, newAction string) {
	t.Helper()
	ctx := context.Background()
	rc, err := sp.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	body, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	var ev audit.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	ev.Action = newAction // Hash field deliberately left alone
	patched, err := json.Marshal(&ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(ctx, key, bytes.NewReader(patched),
		storage.PutOptions{ContentLength: int64(len(patched))}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyAnchor_RewrittenEventKeepingItsHashIsDetected(t *testing.T) {
	store, sp := newAuditStore(t)
	ctx := context.Background()
	appendN(t, store, 3)

	log := audit.NewStorageBackedLog(sp)
	a, err := store.Anchor(ctx, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := store.VerifyAnchor(ctx, log, a.LogID); err != nil || !res.OK {
		t.Fatalf("healthy anchor did not verify: ok=%v mismatch=%q err=%v", res.OK, res.Mismatch, err)
	}

	rewriteEventKeepingHash(t, sp, headEventKey(t, sp), "backup.delete")

	res, err := store.VerifyAnchor(ctx, log, a.LogID)
	if err != nil {
		t.Fatalf("VerifyAnchor: %v", err)
	}
	if res.OK {
		t.Fatal("the anchored event's content was rewritten and VerifyAnchor reported ok=true.\n\n" +
			"It compared the anchor against the event's own Hash FIELD, which is to say it " +
			"asked the event whether it had been tampered with. VerifyChain, which " +
			"recomputes, catches this — so the externally-witnessed check was weaker than " +
			"the internal one.")
	}
	if !strings.Contains(res.Mismatch, "rewritten in place") {
		t.Errorf("mismatch does not identify the rewrite: %q", res.Mismatch)
	}
}

// The pre-existing failure mode — a legitimately different head, e.g.
// the chain moved on and an old event was replaced by a consistent one
// — must still be reported, and as a hash mismatch against the anchor
// rather than as a rewrite.
func TestVerifyAnchor_ConsistentlyRehashedEventStillMismatchesTheAnchor(t *testing.T) {
	store, sp := newAuditStore(t)
	ctx := context.Background()
	appendN(t, store, 3)

	log := audit.NewStorageBackedLog(sp)
	a, err := store.Anchor(ctx, log, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite the head AND recompute its hash correctly: internally
	// self-consistent, but no longer the event that was witnessed.
	key := headEventKey(t, sp)
	rc, err := sp.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	var ev audit.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	ev.Action = "backup.delete"
	h, herr := audit.ComputeHash(&ev)
	if herr != nil {
		t.Fatal(herr)
	}
	ev.Hash = h
	patched, _ := json.Marshal(&ev)
	if err := sp.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(ctx, key, bytes.NewReader(patched),
		storage.PutOptions{ContentLength: int64(len(patched))}); err != nil {
		t.Fatal(err)
	}

	res, err := store.VerifyAnchor(ctx, log, a.LogID)
	if err != nil {
		t.Fatalf("VerifyAnchor: %v", err)
	}
	if res.OK {
		t.Fatal("a consistently re-hashed event still matched its anchor")
	}
	if strings.Contains(res.Mismatch, "rewritten in place") {
		t.Errorf("reported as a self-hash failure; this event IS self-consistent and the "+
			"finding is that it is not the one that was witnessed: %q", res.Mismatch)
	}
}

// A healthy chain must keep verifying — recomputing must not introduce
// a false positive, which would make the tool useless in the other
// direction.
func TestVerifyAnchor_HealthyChainStillVerifies(t *testing.T) {
	store, sp := newAuditStore(t)
	ctx := context.Background()
	appendN(t, store, 5)
	log := audit.NewStorageBackedLog(sp)
	a, err := store.Anchor(ctx, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Chain moves on: later events must not disturb an older anchor.
	appendN(t, store, 2)
	res, err := store.VerifyAnchor(ctx, log, a.LogID)
	if err != nil {
		t.Fatalf("VerifyAnchor: %v", err)
	}
	if !res.OK {
		t.Fatalf("healthy chain failed anchor verification: %q", res.Mismatch)
	}
}

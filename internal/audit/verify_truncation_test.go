package audit_test

// verify_truncation_test.go — the audit chain could not see the most
// obvious tamper.
//
// VerifyChain's checks are all internal to the events that are STILL
// THERE: each event's hash recomputes, each prev_hash matches the prior
// event's hash, each event sits in the shard its scope implies.
//
// Deleting an event in the MIDDLE therefore surfaces immediately —
// event N+1's prev_hash is left dangling. Deleting the events at the
// END leaves nothing dangling: the remainder is a perfectly valid,
// shorter chain, and every check passes. Measured before the fix, on a
// six-event chain with the last two event files removed:
//
//	intact:     checked=6 ok=true breaks=0 mismatches=0
//	truncated:  checked=4 ok=true breaks=0 mismatches=0 misfiled=0
//
// That is the easiest tamper there is and the one with the clearest
// motive — the events worth deleting are the ones recording what you
// just did, and those are always at the end. `audit verify-chain`
// answered "ok".
//
// The head pointer is the only record of where the chain reached, so
// the tail is checked against it. Deleting the pointer too must not
// restore the clean verdict, or the attack just grows one step: a
// missing pointer on a non-empty shard is itself a finding, because the
// check could not run and "could not run" is not "passed".

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

// eventKeys returns the chain's event object keys in ascending order.
func eventKeys(t *testing.T, sp storage.StoragePlugin) []string {
	t.Helper()
	var keys []string
	for info, err := range sp.List(context.Background(), "audit/") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, ".json") && !strings.Contains(info.Key, "_head") &&
			info.Key != audit.HeadKey {
			keys = append(keys, info.Key)
		}
	}
	return keys
}

func appendN(t *testing.T, store *audit.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := store.Append(context.Background(), &audit.Event{Action: "backup.create"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifyChain_TailTruncationIsDetected(t *testing.T) {
	store, sp := newAuditStore(t)
	appendN(t, store, 6)

	if res, err := store.VerifyChain(context.Background()); err != nil || !res.OK {
		t.Fatalf("intact chain did not verify: ok=%v err=%v", res.OK, err)
	}

	keys := eventKeys(t, sp)
	if len(keys) != 6 {
		t.Fatalf("fixture: %d event keys, want 6", len(keys))
	}
	// Delete the last two — the classic tamper.
	for _, k := range keys[4:] {
		if err := sp.Delete(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}

	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if res.OK {
		t.Fatalf("a chain with its last two events deleted verified OK "+
			"(checked=%d breaks=%d mismatches=%d).\n\nThe remaining events form a valid "+
			"shorter chain, so every internal check passes. The tamper-evidence mechanism "+
			"cannot see the tamper it exists for.",
			res.EventsChecked, len(res.ChainBreaks), len(res.HashMismatches))
	}
	if len(res.Truncated) != 1 {
		t.Fatalf("Truncated = %v, want one finding", res.Truncated)
	}
	// Actionable: how far the chain should reach, and how far it does.
	for _, want := range []string{"5", "3", "missing"} {
		if !strings.Contains(res.Truncated[0], want) {
			t.Errorf("finding does not mention %q:\n%s", want, res.Truncated[0])
		}
	}
}

// Deleting the head pointer as well must not buy back the clean
// verdict — otherwise the attack is simply two deletions instead of one.
func TestVerifyChain_DeletingTheHeadPointerIsAlsoAFinding(t *testing.T) {
	store, sp := newAuditStore(t)
	appendN(t, store, 4)

	keys := eventKeys(t, sp)
	for _, k := range keys[2:] {
		if err := sp.Delete(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.Delete(context.Background(), audit.HeadKey); err != nil {
		t.Fatal(err)
	}

	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if res.OK {
		t.Fatal("deleting the tail AND the head pointer restored a clean verdict — the " +
			"truncation check can be disabled by the same actor it is meant to catch")
	}
	if len(res.HeadPointerMissing) == 0 {
		t.Errorf("no HeadPointerMissing finding; got %+v", res)
	}
}

// A replaced head event is invisible to every internal check: its own
// hash can be recomputed consistently, and there is no following event
// whose prev_hash would disagree. Only the pointer disagrees.
func TestVerifyChain_ReplacedHeadEventIsDetected(t *testing.T) {
	store, sp := newAuditStore(t)
	appendN(t, store, 3)

	// Rebuild the head event from scratch with different content, at
	// the same sequence, with a self-consistent hash — what a careful
	// tamperer would write.
	keys := eventKeys(t, sp)
	headKey := keys[len(keys)-1]
	forged := &audit.Event{
		Schema: audit.Schema, ID: "forged", Sequence: 2,
		Action: "backup.delete", PrevHash: chainHashAt(t, sp, keys[len(keys)-2]),
	}
	h, err := audit.ComputeHash(forged)
	if err != nil {
		t.Fatal(err)
	}
	forged.Hash = h
	writeEvent(t, sp, headKey, forged)

	res, verr := store.VerifyChain(context.Background())
	if verr != nil {
		t.Fatalf("VerifyChain: %v", verr)
	}
	if len(res.HashMismatches) != 0 {
		t.Skip("the forged event failed the self-hash check, so this test is not " +
			"exercising the pointer-only detection path")
	}
	if res.OK {
		t.Fatal("the head event was replaced with self-consistent content and the chain " +
			"still verified OK; the head pointer is the only witness and was not consulted")
	}
}

// chainHashAt reads the stored Hash of the event at key.
func chainHashAt(t *testing.T, sp storage.StoragePlugin, key string) string {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var ev audit.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Hash
}

// writeEvent overwrites the object at key with ev.
func writeEvent(t *testing.T, sp storage.StoragePlugin, key string, ev *audit.Event) {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if derr := sp.Delete(context.Background(), key); derr != nil {
		t.Fatal(derr)
	}
	if _, perr := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); perr != nil {
		t.Fatal(perr)
	}
}

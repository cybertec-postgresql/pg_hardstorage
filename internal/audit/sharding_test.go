package audit_test

import (
	"bytes"
	"context"
	stdio "io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// appendScoped appends an event scoped to a deployment (empty
// deployment → the global chain).
func appendScoped(t *testing.T, store *audit.Store, action, deployment string, ts time.Time) *audit.Event {
	t.Helper()
	ev := &audit.Event{
		Action:    action,
		Timestamp: ts,
		Subject:   audit.Subject{Deployment: deployment},
	}
	if err := store.Append(context.Background(), ev); err != nil {
		t.Fatalf("append (%s/%s): %v", action, deployment, err)
	}
	return ev
}

func listKeys(t *testing.T, sp storage.StoragePlugin, prefix string) []string {
	t.Helper()
	var out []string
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("list %s: %v", prefix, err)
		}
		out = append(out, info.Key)
	}
	return out
}

// TestShard_IndependentChainsPerDeployment: events for different
// deployments land in separate shards, each its own chain with its own
// sequence starting at 0, and the whole repo still verifies.
func TestShard_IndependentChainsPerDeployment(t *testing.T) {
	store, sp := newAuditStore(t)
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	// Interleave appends across two deployments.
	appendScoped(t, store, "backup.create", "db1", t0)
	appendScoped(t, store, "backup.create", "db2", t0.Add(time.Minute))
	appendScoped(t, store, "backup.delete", "db1", t0.Add(2*time.Minute))
	appendScoped(t, store, "backup.delete", "db2", t0.Add(3*time.Minute))
	appendScoped(t, store, "hold.add", "db1", t0.Add(4*time.Minute))

	// On-disk: each deployment has its own shard subtree + head pointer.
	for _, want := range []string{
		"audit/shards/d.db1/",
		"audit/shards/d.db2/",
		"audit/shards/d.db1/_head.json",
		"audit/shards/d.db2/_head.json",
	} {
		found := false
		for _, k := range listKeys(t, sp, "audit/") {
			if strings.HasPrefix(k, want) || k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected on-disk key with prefix %q", want)
		}
	}

	// Per-shard sequences are independent: db1 has 0,1,2; db2 has 0,1.
	seqByDep := func(dep string) []int64 {
		evs, err := store.Search(context.Background(), audit.ListFilters{Deployment: dep})
		if err != nil {
			t.Fatal(err)
		}
		var seqs []int64
		for _, e := range evs {
			seqs = append(seqs, e.Sequence)
		}
		return seqs
	}
	if got := seqByDep("db1"); len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Errorf("db1 sequences = %v, want [0 1 2]", got)
	}
	if got := seqByDep("db2"); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("db2 sequences = %v, want [0 1]", got)
	}

	// The whole repo verifies across shards.
	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.EventsChecked != 5 {
		t.Errorf("VerifyChain = %+v, want OK with 5 events", res)
	}
}

// TestShard_GlobalEventsUseLegacyLayout: events with no scope keep the
// pre-sharding on-disk layout, so existing repos stay valid.
func TestShard_GlobalEventsUseLegacyLayout(t *testing.T) {
	store, sp := newAuditStore(t)
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	appendScoped(t, store, "kms.rotate", "", t0)
	appendScoped(t, store, "repo.gc", "", t0.Add(time.Minute))

	var eventKeys, headSeen int
	for _, k := range listKeys(t, sp, "audit/") {
		if strings.HasPrefix(k, "audit/shards/") {
			t.Errorf("global event unexpectedly sharded: %s", k)
		}
		if k == audit.HeadKey {
			headSeen++
			continue
		}
		if strings.HasSuffix(k, ".json") {
			eventKeys++
		}
	}
	if eventKeys != 2 {
		t.Errorf("expected 2 legacy-layout events, got %d", eventKeys)
	}
	if headSeen != 1 {
		t.Errorf("expected the legacy %s head pointer, saw %d", audit.HeadKey, headSeen)
	}
	res, _ := store.VerifyChain(context.Background())
	if !res.OK || res.EventsChecked != 2 {
		t.Errorf("VerifyChain = %+v, want OK with 2 events", res)
	}
}

// TestShard_VerifyDetectsMisfiledEvent: an otherwise-valid event
// relocated into a shard its scope doesn't imply is flagged, even
// though its own hash is intact.
func TestShard_VerifyDetectsMisfiledEvent(t *testing.T) {
	store, sp := newAuditStore(t)
	ctx := context.Background()

	// A valid GLOBAL event.
	ev := appendScoped(t, store, "kms.rotate", "", time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))

	// Find its key and copy its bytes verbatim into a deployment shard.
	var globalKey string
	for _, k := range listKeys(t, sp, "audit/") {
		if strings.HasPrefix(k, "audit/shards/") || k == audit.HeadKey {
			continue
		}
		if strings.HasSuffix(k, ".json") {
			globalKey = k
			break
		}
	}
	if globalKey == "" {
		t.Fatal("global event key not found")
	}
	rc, err := sp.Get(ctx, globalKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stdio.ReadAll(rc)
	rc.Close()
	misfiledKey := strings.Replace(globalKey, "audit/", "audit/shards/d.db1/", 1)
	if _, err := sp.Put(ctx, misfiledKey, bytes.NewReader(raw), storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}

	res, err := store.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("VerifyChain should fail on a misfiled event")
	}
	found := false
	for _, id := range res.Misfiled {
		if id == ev.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("event %s should be reported as misfiled; got %+v", ev.ID, res)
	}
}

// TestShard_TamperIsolatedToShard: tampering one shard's event is
// caught and does not produce false findings in sibling shards.
func TestShard_TamperIsolatedToShard(t *testing.T) {
	store, sp := newAuditStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	victim := appendScoped(t, store, "backup.create", "db1", t0)
	appendScoped(t, store, "backup.create", "db2", t0.Add(time.Minute))
	appendScoped(t, store, "backup.delete", "db2", t0.Add(2*time.Minute))

	// Tamper db1's event body in place (change the action) WITHOUT
	// recomputing its hash → a hash mismatch in shard d.db1.
	var victimKey string
	for _, k := range listKeys(t, sp, "audit/shards/d.db1/") {
		if strings.HasSuffix(k, ".json") && k != "audit/shards/d.db1/_head.json" {
			victimKey = k
		}
	}
	if victimKey == "" {
		t.Fatal("db1 shard event key not found")
	}
	rc, _ := sp.Get(ctx, victimKey)
	raw, _ := stdio.ReadAll(rc)
	rc.Close()
	tampered := bytes.Replace(raw, []byte(`"backup.create"`), []byte(`"backup.tamper"`), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("tamper replacement did not apply")
	}
	if _, err := sp.Put(ctx, victimKey, bytes.NewReader(tampered), storage.PutOptions{ContentLength: int64(len(tampered))}); err != nil {
		t.Fatal(err)
	}

	res, err := store.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("VerifyChain should detect the tamper")
	}
	if len(res.HashMismatches) != 1 || res.HashMismatches[0] != victim.ID {
		t.Errorf("expected exactly the db1 event flagged, got %+v", res)
	}
	// All 3 events were still checked — the db2 shard is intact and
	// produced no false findings.
	if res.EventsChecked != 3 {
		t.Errorf("EventsChecked = %d, want 3", res.EventsChecked)
	}
	for _, id := range res.ChainBreaks {
		if id != victim.ID {
			t.Errorf("sibling shard produced a false chain break: %s", id)
		}
	}
}

// TestShard_AnchorAllWitnessesEveryShard: AnchorAll publishes one
// anchor per non-empty shard, and each anchor verifies against its own
// shard's chain.
func TestShard_AnchorAllWitnessesEveryShard(t *testing.T) {
	store, sp := newAuditStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	appendScoped(t, store, "backup.create", "db1", t0)
	appendScoped(t, store, "backup.delete", "db1", t0.Add(time.Minute))
	appendScoped(t, store, "backup.create", "db2", t0.Add(2*time.Minute))
	appendScoped(t, store, "kms.rotate", "", t0.Add(3*time.Minute)) // global

	log := audit.NewStorageBackedLog(sp)
	anchors, err := store.AnchorAll(ctx, log, "pub")
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 3 { // global + d.db1 + d.db2
		t.Fatalf("AnchorAll = %d anchors, want 3", len(anchors))
	}
	seen := map[string]bool{}
	for _, a := range anchors {
		seen[a.Shard] = true
		res, err := store.VerifyAnchor(ctx, log, a.LogID)
		if err != nil {
			t.Fatalf("VerifyAnchor(shard %q): %v", a.Shard, err)
		}
		if !res.OK {
			t.Errorf("anchor for shard %q did not verify: %+v", a.Shard, res)
		}
		if a.Shard == "d.db1" && a.HeadSequence != 1 {
			t.Errorf("d.db1 anchor HeadSequence = %d, want 1 (two events)", a.HeadSequence)
		}
	}
	for _, want := range []string{"", "d.db1", "d.db2"} {
		if !seen[want] {
			t.Errorf("shard %q was not anchored", want)
		}
	}
}

// TestShard_ConcurrentAppendsDifferentShards: appends to distinct
// deployments run concurrently without corrupting any chain — the core
// benefit of sharding (no shared head-pointer contention).
func TestShard_ConcurrentAppendsDifferentShards(t *testing.T) {
	store, _ := newAuditStore(t)
	const shards = 12
	var wg sync.WaitGroup
	for i := 0; i < shards; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dep := "db" + string(rune('a'+i))
			base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
			for j := 0; j < 3; j++ {
				appendScoped(t, store, "backup.create", dep, base.Add(time.Duration(j)*time.Minute))
			}
		}(i)
	}
	wg.Wait()

	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("concurrent cross-shard appends broke a chain: %+v", res)
	}
	if res.EventsChecked != shards*3 {
		t.Errorf("EventsChecked = %d, want %d", res.EventsChecked, shards*3)
	}
}

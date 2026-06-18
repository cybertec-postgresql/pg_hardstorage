package cli_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// countAuditEventFiles / pruneOldestAuditEvents operate directly on the
// repo's storage so the tests can forge the on-disk states doctor's
// anchor-freshness probe used to misjudge.

// pruneOldestAuditEvents deletes the n lowest-sequence audit *event* files
// under audit/ (never the head pointer, never an anchor), simulating WORM
// retention having reaped the oldest chain events while the head pointer
// and anchor keep their (higher) sequence numbers.
func pruneOldestAuditEvents(t *testing.T, sp storage.StoragePlugin, n int) {
	t.Helper()
	ctx := context.Background()
	type evKey struct {
		key string
		seq int64
	}
	var evs []evKey
	for info, err := range sp.List(ctx, "audit/") {
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		if info.Key == audit.HeadKey || strings.HasSuffix(info.Key, "/_head.json") {
			continue
		}
		if strings.HasPrefix(info.Key, audit.AnchorPrefix) {
			continue
		}
		rc, err := sp.Get(ctx, info.Key)
		if err != nil {
			t.Fatal(err)
		}
		var e audit.Event
		if derr := json.NewDecoder(rc).Decode(&e); derr != nil {
			rc.Close()
			t.Fatalf("decode %s: %v", info.Key, derr)
		}
		rc.Close()
		evs = append(evs, evKey{info.Key, e.Sequence})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].seq < evs[j].seq })
	for i := 0; i < n && i < len(evs); i++ {
		if err := sp.Delete(ctx, evs[i].key); err != nil {
			t.Fatalf("delete %s: %v", evs[i].key, err)
		}
	}
}

// TestDoctor_AnchorFresh_SurvivesRetentionPruning is the regression for the
// false-positive `audit.anchor_stale` that fired whenever WORM retention had
// pruned any audit events: doctor derived the "current head" by counting
// event files, so on a pruned chain chainKeys < headSequence+1 and a
// perfectly fresh anchor was reported stale — with a nonsensical NEGATIVE
// "events behind" count, which re-anchoring could never clear.
func TestDoctor_AnchorFresh_SurvivesRetentionPruning(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()
	store := audit.NewStore(w.sp)
	log := audit.NewStorageBackedLog(w.sp)

	// 5 global-chain events (seq 0..4), then anchor at the head (seq 4).
	for i := 0; i < 5; i++ {
		if err := store.Append(ctx, &audit.Event{Action: "test.tick"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Anchor(ctx, log, "doctor-test"); err != nil {
		t.Fatal(err)
	}
	// Retention reaps the 2 oldest events. Head pointer + anchor stay at 4.
	pruneOldestAuditEvents(t, w.sp, 2)

	writeReadWorldConfig(t, w, manifestSigConfigYAML(w.repoURL, "db1"))
	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, "audit.anchor_stale") {
		t.Errorf("fresh anchor wrongly flagged stale after retention pruning:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"anchor_fresh": true`) {
		t.Errorf("expected anchor_fresh=true:\n%s", stdout)
	}
	if strings.Contains(stdout, "behind") && strings.Contains(stdout, "-") {
		t.Errorf("a negative behind-count leaked into the report:\n%s", stdout)
	}
}

// TestDoctor_AnchorFresh_MultiShardNotOvercounted is the regression for the
// multi-shard variant of the same bug: doctor counted event files across ALL
// shards but compared the total against a single anchor (LatestAnchor returns
// the max-HeadSequence anchor, which witnesses just one shard). A repo with
// two fully-anchored shards therefore reported the anchor "N events behind",
// where N was the size of the other shard.
func TestDoctor_AnchorFresh_MultiShardNotOvercounted(t *testing.T) {
	w := newReadWorld(t)
	ctx := context.Background()
	store := audit.NewStore(w.sp)
	log := audit.NewStorageBackedLog(w.sp)

	// Global chain: 3 events (seq 0..2).
	for i := 0; i < 3; i++ {
		if err := store.Append(ctx, &audit.Event{Action: "global.tick"}); err != nil {
			t.Fatal(err)
		}
	}
	// A deployment-scoped shard: 4 events (seq 0..3) — lands under
	// audit/shards/d.shardb/, its own independent chain.
	for i := 0; i < 4; i++ {
		ev := &audit.Event{Action: "scoped.tick"}
		ev.Subject.Deployment = "shardb"
		if err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// Anchor EVERY shard's head — both chains are fully witnessed.
	if _, err := store.AnchorAll(ctx, log, "doctor-test"); err != nil {
		t.Fatal(err)
	}

	writeReadWorldConfig(t, w, manifestSigConfigYAML(w.repoURL, "db1"))
	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, "audit.anchor_stale") {
		t.Errorf("fully-anchored multi-shard repo wrongly flagged stale:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"anchor_fresh": true`) {
		t.Errorf("expected anchor_fresh=true:\n%s", stdout)
	}
}

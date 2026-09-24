package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// TestVerifyBundle_MultiShardBundleVerifies pins H20: the audit chain is
// sharded per deployment/tenant, each shard an independent chain with
// its own sequence numbers, and Search (hence the bundle) merges shards
// by timestamp. verify-bundle compared each event with the one before it
// in the merged order, so db1#1 was checked against db2#0 — same
// "next sequence", different chain — and every genuine multi-shard
// bundle failed with a false "chain break".
func TestVerifyBundle_MultiShardBundleVerifies(t *testing.T) {
	w := setupBundleWorld(t)
	base := time.Now().UTC()
	// Interleave two deployments: db1#0, db2#0, db1#1, db2#1, db1#2.
	for i := 0; i < 5; i++ {
		dep := "db1"
		if i%2 == 1 {
			dep = "db2"
		}
		w.appendEvent(t, "x.event", dep, base.Add(time.Duration(i)*time.Second))
	}

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	m, err := audit.VerifyBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("a genuine two-shard bundle failed verification: %v", err)
	}
	if m.Integrity == nil || m.Integrity.EventsChecked != 5 {
		t.Fatalf("integrity = %+v, want 5 events checked", m.Integrity)
	}
	if !m.Integrity.LinkageAsserted {
		t.Errorf("linkage not asserted on an unfiltered export whose every shard is consecutive (gaps=%d)",
			m.Integrity.SequenceGaps)
	}
}

// TestVerifyBundle_MultiShardStillCatchesBreak: checking shards
// separately must not stop checking them — a severed link inside one
// shard of a multi-shard bundle is still a chain break.
func TestVerifyBundle_MultiShardStillCatchesBreak(t *testing.T) {
	w := setupBundleWorld(t)
	base := time.Now().UTC()
	for i := 0; i < 6; i++ {
		dep := "db1"
		if i%2 == 1 {
			dep = "db2"
		}
		w.appendEvent(t, "x.event", dep, base.Add(time.Duration(i)*time.Second))
	}
	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	files := unpackBundle(t, buf.Bytes())
	lines := bytes.Split(bytes.TrimRight(files["events.ndjson"], "\n"), []byte("\n"))
	// Line 4 is db1#2; sever its link to db1#1.
	var ev audit.Event
	if err := json.Unmarshal(lines[4], &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Subject.Deployment != "db1" || ev.Sequence != 2 {
		t.Fatalf("fixture: line 4 is %s#%d, want db1#2", ev.Subject.Deployment, ev.Sequence)
	}
	ev.PrevHash = strings.Repeat("0", len(ev.PrevHash))
	h, err := audit.ComputeHash(&ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.Hash = h
	patched, _ := json.Marshal(&ev)
	lines[4] = patched
	files["events.ndjson"] = append(bytes.Join(lines, []byte("\n")), '\n')

	_, verr := audit.VerifyBundle(bytes.NewReader(repackAndResign(t, w, files)))
	if verr == nil || !strings.Contains(verr.Error(), "chain break") {
		t.Fatalf("a severed link inside one shard: got %v, want a chain break", verr)
	}
}

package repoaudit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repoaudit"
)

// latest.has_replica_copy must reflect the manifests/_replicas/ index.
// observe ignored the index it was handed, so the field was false for
// every deployment whether or not the newest backup had a replica copy.
func TestAudit_LatestHasReplicaCopy(t *testing.T) {
	w := setupAuditWorld(t)
	replicated := w.commitPlain(t, "db1", "newest", 5)
	w.commitPlain(t, "db2", "only", 1)
	ctx := context.Background()

	// Normalise the replica tree: exactly one copy, for db1's backup.
	var existing []string
	for info, err := range w.sp.List(ctx, "manifests/_replicas/") {
		if err != nil {
			t.Fatal(err)
		}
		existing = append(existing, info.Key)
	}
	for _, k := range existing {
		_ = w.sp.Delete(ctx, k)
	}
	k := "manifests/_replicas/" + replicated + ".manifest.json"
	if _, err := w.sp.Put(ctx, k, strings.NewReader("{}"), storage.PutOptions{ContentLength: 2}); err != nil {
		t.Fatal(err)
	}

	rep, err := repoaudit.Audit(ctx, w.sp, w.meta, w.repoURL, repoaudit.Options{Verifier: w.verifier})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range rep.Deployments {
		if d.Latest == nil {
			t.Fatalf("%s: no latest", d.Name)
		}
		got[d.Name] = d.Latest.HasReplicaCopy
	}
	if !got["db1"] {
		t.Error("db1's newest backup has a _replicas copy but has_replica_copy=false")
	}
	if got["db2"] {
		t.Error("db2's newest backup has no _replicas copy but has_replica_copy=true")
	}
}

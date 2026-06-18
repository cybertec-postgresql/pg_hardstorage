package backup_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// manifestFor returns a valid, commit-ready manifest for the given
// deployment and backup ID (built on validManifest()).
func manifestFor(deployment, backupID string) *backup.Manifest {
	m := validManifest()
	m.Deployment = deployment
	m.BackupID = backupID
	return m
}

func commitDep(t *testing.T, store *backup.ManifestStore, signer *backup.Signer, deployment string) {
	t.Helper()
	m := manifestFor(deployment, deployment+".full.20260506T120000Z.0001")
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", deployment, err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestDeployments_IndexBuildsAndMatchesScan: Deployments() returns the
// committed deployments, and building it leaves the index (sentinel +
// per-deployment markers) on disk so subsequent calls take the fast
// path.
func TestDeployments_IndexBuildsAndMatchesScan(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()
	for _, dep := range []string{"db1", "db2", "alpha"} {
		commitDep(t, store, signer, dep)
	}

	got, err := store.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "db1", "db2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Deployments = %v, want %v", got, want)
	}

	// Index materialized.
	if _, err := sp.Stat(ctx, "deployments/_initialized"); err != nil {
		t.Errorf("sentinel should exist after Deployments(): %v", err)
	}
	for _, dep := range want {
		if _, err := sp.Stat(ctx, "deployments/names/"+dep); err != nil {
			t.Errorf("marker for %q missing: %v", dep, err)
		}
	}

	// Fast path (sentinel present) returns the same set.
	got2, err := store.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got2, want) {
		t.Errorf("fast-path Deployments = %v, want %v", got2, want)
	}
}

// TestDeployments_NewDeploymentAppearsViaMarker: a deployment committed
// AFTER the index is authoritative is picked up via its Commit-written
// marker (no rescan needed).
func TestDeployments_NewDeploymentAppearsViaMarker(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ctx := context.Background()
	commitDep(t, store, signer, "db1")
	if _, err := store.Deployments(ctx); err != nil { // build the index
		t.Fatal(err)
	}
	commitDep(t, store, signer, "db2") // marker written at commit
	got, err := store.Deployments(ctx) // fast path
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "db2") {
		t.Errorf("db2 should appear via its marker; got %v", got)
	}
}

// TestDeployments_BackwardCompat_RebuildsIndex: a repo whose index was
// wiped (an upgraded repo that predates the index) is scanned
// authoritatively and the index rebuilt.
func TestDeployments_BackwardCompat_RebuildsIndex(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()
	commitDep(t, store, signer, "db1")
	commitDep(t, store, signer, "db2")
	if _, err := store.Deployments(ctx); err != nil {
		t.Fatal(err)
	}
	// Wipe the entire index, leaving only the manifests.
	for _, k := range []string{"deployments/_initialized", "deployments/names/db1", "deployments/names/db2"} {
		_ = sp.Delete(ctx, k)
	}

	got, err := store.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"db1", "db2"}) {
		t.Fatalf("after index wipe, Deployments = %v, want [db1 db2]", got)
	}
	if _, err := sp.Stat(ctx, "deployments/_initialized"); err != nil {
		t.Errorf("sentinel should be rebuilt: %v", err)
	}
}

// TestDeployments_RescanFindsUnmarkedDeployment pins the sentinel
// gating: the fast path is trusted ONLY when the sentinel is present.
// A deployment whose marker was dropped is still found by the rescan,
// because the self-heal removes the sentinel alongside.
func TestDeployments_RescanFindsUnmarkedDeployment(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()
	commitDep(t, store, signer, "db1")
	commitDep(t, store, signer, "db2")
	if _, err := store.Deployments(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate the post-self-heal state after a dropped marker: db2's
	// marker gone AND the sentinel gone (so the index is not trusted).
	_ = sp.Delete(ctx, "deployments/names/db2")
	_ = sp.Delete(ctx, "deployments/_initialized")

	got, err := store.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"db1", "db2"}) {
		t.Fatalf("rescan should recover db2 from its manifest; got %v", got)
	}
	// And the marker is re-created during the rebuild.
	if _, err := sp.Stat(ctx, "deployments/names/db2"); err != nil {
		t.Errorf("db2 marker should be rebuilt: %v", err)
	}
}

// TestDeployments_ExcludesReplicas: the _replicas redundancy slot is
// never reported as a deployment (parity with the scan).
func TestDeployments_ExcludesReplicas(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ctx := context.Background()
	commitDep(t, store, signer, "db1") // Commit also writes a replica
	got, err := store.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if contains(got, "_replicas") {
		t.Errorf("_replicas leaked into Deployments(): %v", got)
	}
	if !reflect.DeepEqual(got, []string{"db1"}) {
		t.Errorf("Deployments = %v, want [db1]", got)
	}
}

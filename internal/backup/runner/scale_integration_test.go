// Build-tagged integration test: fleet-scale behaviour against a real
// PG 17 testcontainer. Run with `make test-integration` (Docker).
//
//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestIntegration_Scale_MultiDeployment_IndexAndShardedAudit takes real
// backups for several deployments against one PostgreSQL instance and
// asserts the two fleet-scale mechanisms work end-to-end against the
// committed repo:
//
//   - the deployment index enumerates the fleet via the fast path
//     (sentinel present, every deployment listed), and
//   - the audit log shards per deployment (one chain per `d.<dep>`)
//     and `VerifyChain` is clean across every shard.
//
// This exercises the index + sharding with REAL backup-emitted audit
// events, not synthetic fixtures.
func TestIntegration_Scale_MultiDeployment_IndexAndShardedAudit(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	deployments := []string{"alpha", "db1", "db2", "db3"}
	for _, dep := range deployments {
		if _, err := runner.Take(ctx, runner.TakeOptions{
			PGConnString:    srv.DSN,
			RepoURL:         repoURL,
			Deployment:      dep,
			Signer:          signer,
			Verifier:        verifier,
			Fast:            true,
			IncludeManifest: true,
		}); err != nil {
			t.Fatalf("backup %q: %v", dep, err)
		}
	}

	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	defer sp.Close()

	// --- Deployment index: fast enumeration returns every deployment.
	ms := backup.NewManifestStore(sp)
	got, err := ms.Deployments(ctx)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	want := append([]string(nil), deployments...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Deployments() = %v, want %v", got, want)
	}
	// The index must be materialized (the fast path is active).
	if _, err := sp.Stat(ctx, "deployments/_initialized"); err != nil {
		t.Errorf("deployment-index sentinel missing after Deployments(): %v", err)
	}

	// --- Sharded audit: one chain per deployment, all verifying clean.
	astore := audit.NewStore(sp)
	res, err := astore.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !res.OK {
		t.Errorf("audit verify-chain not clean across shards: %+v", res)
	}
	if res.EventsChecked < len(deployments) {
		t.Errorf("EventsChecked = %d, want >= %d (one backup.create per deployment)",
			res.EventsChecked, len(deployments))
	}
	for _, dep := range deployments {
		if !hasShardEvents(t, sp, "audit/shards/d."+dep+"/") {
			t.Errorf("no audit shard for deployment %q", dep)
		}
	}
	// And nothing landed in the global chain (every event was scoped).
	if hasShardEvents(t, sp, "audit/2") { // audit/<yyyy>/...
		t.Errorf("scoped backup events unexpectedly landed in the global chain")
	}
}

// hasShardEvents reports whether any chain event (a .json that isn't a
// _head.json pointer) exists under prefix.
func hasShardEvents(t *testing.T, sp storage.StoragePlugin, prefix string) bool {
	t.Helper()
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("list %s: %v", prefix, err)
		}
		if strings.HasSuffix(info.Key, ".json") && !strings.HasSuffix(info.Key, "_head.json") {
			return true
		}
	}
	return false
}

package restore_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// commitN writes N manifests at minutely intervals so the test can
// assert ResolveLatest picks the highest StoppedAt.
func commitN(t *testing.T, sp storage.StoragePlugin, signer *backup.Signer, deployment string, n int) []string {
	t.Helper()
	store := backup.NewManifestStore(sp)
	base := time.Date(2026, 4, 28, 14, 0, 0, 0, time.UTC)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		id := deployment + ".full." + ts.Format("20060102T150405Z") + ".000" + string(rune('1'+i))
		m := &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         id,
			Deployment:       deployment,
			Tenant:           "default",
			Type:             backup.BackupTypeFull,
			PGVersion:        17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028",
			StopLSN:          "0/30001A0",
			Timeline:         1,
			Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			StartedAt:        ts,
			StoppedAt:        ts.Add(30 * time.Second),
			BackupLabel:      "START WAL LOCATION: 0/3000028\n",
			Files:            []backup.FileEntry{},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

func newRepoWithSigner(t *testing.T) (storage.StoragePlugin, *backup.Signer, *backup.Verifier) {
	t.Helper()
	root := t.TempDir()
	url := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: url}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: parseURL(t, url)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return sp, signer, verifier
}

func parseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestResolveLatest_SinglePicksIt(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 1)
	got, err := restore.ResolveLatest(context.Background(), sp, "db1", verifier)
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if got != ids[0] {
		t.Errorf("got %q want %q", got, ids[0])
	}
}

func TestResolveLatest_MultiplePicksHighest(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 5)
	got, err := restore.ResolveLatest(context.Background(), sp, "db1", verifier)
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	want := ids[len(ids)-1] // most recent StoppedAt
	if got != want {
		t.Errorf("got %q want %q (latest of %d)", got, want, len(ids))
	}
}

func TestResolveLatest_NoBackupsFound(t *testing.T) {
	sp, _, verifier := newRepoWithSigner(t)
	_, err := restore.ResolveLatest(context.Background(), sp, "db1", verifier)
	if !errors.Is(err, restore.ErrNoBackupsFound) {
		t.Errorf("expected ErrNoBackupsFound; got %v", err)
	}
}

func TestResolveLatest_OnlyConsidersNamedDeployment(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	commitN(t, sp, signer, "db2", 3)
	_, err := restore.ResolveLatest(context.Background(), sp, "db1", verifier)
	if !errors.Is(err, restore.ErrNoBackupsFound) {
		t.Errorf("backups for db2 must not satisfy ResolveLatest(db1); got %v", err)
	}
}

func TestResolveLatest_RejectsForeignVerifier(t *testing.T) {
	sp, signer, _ := newRepoWithSigner(t)
	commitN(t, sp, signer, "db1", 2)
	// Verifier from a different keypair → all manifests fail to verify.
	_, foreignPub, _ := backup.GenerateKeypair(rand.Reader)
	foreignVerifier, _ := backup.LoadVerifier(foreignPub)
	_, err := restore.ResolveLatest(context.Background(), sp, "db1", foreignVerifier)
	if err == nil {
		t.Fatal("expected error when no manifests verify")
	}
	if errors.Is(err, restore.ErrNoBackupsFound) {
		t.Errorf("error should distinguish verification failure from absence: %v", err)
	}
}

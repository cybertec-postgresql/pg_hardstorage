package capacity_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/capacity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// TestProject_FleetRateIsSumOfDeploymentRates is the regression for
// the fleet aggregation: the fleet BytesPerDay was the byte-weighted
// AVERAGE of the per-deployment slopes multiplied by the deployment
// count. With one deployment writing 1 GiB/day and one 3 GiB/day the
// repository grows 4 GiB/day; the weighted-average form reported
// (1·3 + 3·9)/12 · 2 = 5 GiB/day.
func TestProject_FleetRateIsSumOfDeploymentRates(t *testing.T) {
	const gib = int64(1 << 30)
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	store := backup.NewManifestStore(sp)
	info, err := casdefault.New(sp).PutChunk(context.Background(), []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	commit := func(dep string, at time.Time, size int64) {
		t.Helper()
		id := dep + ".full." + at.Format("20060102T150405Z")
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: dep, Tenant: "default",
			Type: backup.BackupTypeFull, PGVersion: 17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
			StartedAt: at, StoppedAt: at.Add(30 * time.Second),
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files: []backup.FileEntry{{
				Path: "data/" + id, Size: size, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: size}},
			}},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for d := 0; d < 3; d++ {
		at := start.Add(time.Duration(d) * 24 * time.Hour)
		commit("small", at, 1*gib)
		commit("large", at, 3*gib)
	}

	rep, err := capacity.Project(context.Background(), sp, repoURL, capacity.ProjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, dp := range rep.PerDeployment {
		sum += dp.BytesPerDay
	}
	if sum != 4*gib {
		t.Fatalf("per-deployment slopes sum to %d, want 4 GiB/day (fixture broken): %+v", sum, rep.PerDeployment)
	}
	if rep.BytesPerDay != sum {
		t.Errorf("fleet BytesPerDay = %.3f GiB/day, want %.3f (sum of per-deployment slopes)",
			float64(rep.BytesPerDay)/float64(gib), float64(sum)/float64(gib))
	}
}

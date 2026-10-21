//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// Real PostgreSQL, real backups: the default chunk size, then 1 MiB, then
// a second 1 MiB backup (exercising dedup hints as a Bloom filter, the
// spilled adoption set and the GC fence), each restored and checked
// against PG's own backup_manifest by restore's in-process pg_verifybackup.
func TestIntegration_ChunkSize_FewerChunks_RestoreAndDedup(t *testing.T) {
	srv := testkit.StartPostgres(t)
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// A fresh cluster is ~40 MB of small files, which chunk the same at any
	// size; add ~250 MB of poorly compressible table data so the average
	// chunk size actually matters.
	conn, err := pgx.Connect(ctx, srv.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE bulk AS
		SELECT g AS id, md5(g::text) || md5((g+1)::text) || md5((g+2)::text) || md5((g+3)::text) AS pad
		FROM generate_series(1, 2000000) g`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)

	take := func(dep string, avg int) *runner.Result {
		t.Helper()
		res, err := runner.Take(ctx, runner.TakeOptions{
			PGConnString: srv.DSN, RepoURL: repoURL, Deployment: dep,
			Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true, ChunkAvgSize: avg,
		})
		if err != nil {
			t.Fatalf("backup %s avg=%d: %v", dep, avg, err)
		}
		return res
	}
	def := take("dflt", 0)
	big1 := take("big", 1<<20)
	big2 := take("big", 1<<20)
	t.Logf("default : refs=%d unique=%d", def.TotalChunkRefs, def.UniqueChunkCount)
	t.Logf("1MiB #1 : refs=%d unique=%d", big1.TotalChunkRefs, big1.UniqueChunkCount)
	t.Logf("1MiB #2 : refs=%d unique=%d chunks_deduped=%d", big2.TotalChunkRefs, big2.UniqueChunkCount, big2.Dedup.HitsInMemory+big2.Dedup.HitsStorage)

	if big1.TotalChunkRefs*3 > def.TotalChunkRefs {
		t.Errorf("1 MiB chunks did not cut the chunk count: %d vs default %d", big1.TotalChunkRefs, def.TotalChunkRefs)
	}
	if big1.LogicalBytes != def.LogicalBytes && big1.LogicalBytes < def.LogicalBytes/2 {
		t.Errorf("logical bytes diverge wildly: %d vs %d", big1.LogicalBytes, def.LogicalBytes)
	}
	if big2.Dedup.HitsInMemory+big2.Dedup.HitsStorage == 0 {
		t.Error("second backup at the same chunk size deduplicated nothing")
	}

	for _, c := range []struct{ dep, id string }{{"dflt", def.BackupID}, {"big", big1.BackupID}, {"big", big2.BackupID}} {
		target := filepath.Join(t.TempDir(), "r")
		rres, err := restore.Restore(ctx, restore.Options{
			RepoURL: repoURL, Deployment: c.dep, BackupID: c.id, TargetDir: target, Verifier: verifier,
		})
		if err != nil {
			t.Fatalf("restore %s: %v", c.id, err)
		}
		if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
			t.Errorf("%s: restored datadir has no PG_VERSION: %v", c.id, err)
		}
		if rres.BytesWritten == 0 {
			t.Errorf("%s: restored zero bytes", c.id)
		}
	}
}

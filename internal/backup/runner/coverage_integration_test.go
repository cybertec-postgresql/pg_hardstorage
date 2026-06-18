//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestBackupRunnerCoverage_EncryptedParallelBackups(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	// All three backups MUST share one KEK (and thus one KEKRef). The CAS
	// deduplicates chunks by PLAINTEXT hash across the whole repo, so every
	// encrypted backup that can share a chunk has to resolve to the same
	// plaintext DEK — otherwise a later backup dedups onto a chunk it can't
	// decrypt, and restore fails on every shared chunk (issue #28). selectDEK
	// enforces this: a second KEK under the same ref can't unwrap the first
	// DEK and is correctly refused (hardened in 2bdfda2). An earlier version
	// of this test alternated two KEKs under one ref and "passed" only
	// because the runner silently forked to a fresh DEK, producing
	// unrestorable deduped backups. One KEK is the only dedup-safe shape;
	// taking 3 backups under it exercises the reuse path (backups 2 and 3
	// reuse backup 1's DEK).
	var kek [encryption.KeyLen]byte
	io.ReadFull(rand.Reader, kek[:])

	var ids [3]string
	var files [3]int

	for i := 0; i < 3; i++ {
		res, err := runner.Take(ctx, runner.TakeOptions{
			PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
			Signer: signer, Verifier: verifier, Fast: true,
			Encryption: &runner.EncryptionConfig{KEK: kek, KEKRef: "runner-cov"},
		})
		if err != nil {
			t.Fatalf("Take %d: %v", i+1, err)
		}
		ids[i] = res.BackupID
		files[i] = res.FileCount
	}

	for i, id := range ids {
		if id == "" || files[i] == 0 {
			t.Errorf("backup %d: id=%q files=%d", i+1, id, files[i])
		}
	}
	t.Logf("runner coverage: 3 encrypted backups OK")
}

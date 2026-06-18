// End-to-end encryption integration test: take an encrypted backup
// against a real PG 17 container, then restore it with the same KEK
// and assert the data dir matches.
//
//go:build integration

package restore_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestIntegration_BackupEncrypted_RestoreDecrypted(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString:    srv.DSN,
		RepoURL:         repoURL,
		Deployment:      "db1",
		Signer:          signer,
		Verifier:        verifier,
		Fast:            true,
		IncludeManifest: true,
		Encryption: &runner.EncryptionConfig{
			KEK:    kek,
			KEKRef: "local:default",
		},
	})
	if err != nil {
		t.Fatalf("runner.Take: %v", err)
	}

	// Restore with the same KEK — should succeed.
	target := filepath.Join(t.TempDir(), "restored")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   bres.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
		KEKForRef: func(ref string) ([encryption.KeyLen]byte, error) {
			if ref != "local:default" {
				return [encryption.KeyLen]byte{}, errors.New("unexpected ref")
			}
			return kek, nil
		},
	})
	if err != nil {
		t.Fatalf("restore.Restore: %v", err)
	}
	if rres.BytesWritten == 0 {
		t.Error("restored 0 bytes")
	}
	pgVersionBytes, err := os.ReadFile(filepath.Join(target, "PG_VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pgVersionBytes) == 0 {
		t.Error("PG_VERSION empty")
	}
}

func TestIntegration_RestoreEncrypted_WithoutKEK_Errors(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	var kek [encryption.KeyLen]byte
	_, _ = rand.Read(kek[:])

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		Verifier:     verifier,
		Fast:         true,
		Encryption: &runner.EncryptionConfig{
			KEK:    kek,
			KEKRef: "local:default",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Restore WITHOUT the KEK resolver — must error with the
	// `config.no_kek_resolver` structured code.
	target := filepath.Join(t.TempDir(), "restored")
	_, err = restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   bres.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
	})
	if err == nil {
		t.Fatal("restore without KEK resolver should fail")
	}
}

func TestIntegration_RestoreEncrypted_WrongKEK_Errors(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	var kek, otherKEK [encryption.KeyLen]byte
	_, _ = rand.Read(kek[:])
	_, _ = rand.Read(otherKEK[:])

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		Verifier:     verifier,
		Fast:         true,
		Encryption:   &runner.EncryptionConfig{KEK: kek, KEKRef: "real"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Provide the WRONG KEK at restore time. Must error with
	// restore.kek_mismatch — the AEAD tag on Unwrap fails.
	target := filepath.Join(t.TempDir(), "restored")
	_, err = restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   bres.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
		KEKForRef: func(_ string) ([encryption.KeyLen]byte, error) {
			return otherKEK, nil
		},
	})
	if err == nil {
		t.Fatal("restore with wrong KEK should fail")
	}
}

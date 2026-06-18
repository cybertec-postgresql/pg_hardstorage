// Build-tagged integration test: full backup pipeline against a real
// PG 17 testcontainer. Run with `make test-integration` (requires Docker).
//
//go:build integration

package runner_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// TestIntegration_TakeBackup_EndToEnd is the headline test: real PG
// container, real fs-backed repo, real signing key — Take produces a
// committed manifest that we can reopen, verify, and use to
// reconstitute one specific file.
func TestIntegration_TakeBackup_EndToEnd(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var events []*output.Event
	res, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString:    srv.DSN,
		RepoURL:         repoURL,
		Deployment:      "db1",
		Signer:          signer,
		Verifier:        verifier,
		Fast:            true,
		IncludeManifest: true,
		OnEvent:         func(e *output.Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("runner.Take: %v", err)
	}

	if res.BackupID == "" {
		t.Error("BackupID should be populated")
	}
	if !strings.HasPrefix(res.BackupID, "db1.full.") {
		t.Errorf("BackupID = %q, want prefix db1.full.", res.BackupID)
	}
	if want := testkit.ExpectedPGMajorInt(); res.PGVersion != want {
		t.Errorf("PGVersion = %d, want %d", res.PGVersion, want)
	}
	if res.SystemIdentifier == "" {
		t.Error("SystemIdentifier missing")
	}
	if res.StopLSN == "" || res.Timeline == 0 {
		t.Error("LSN / timeline missing")
	}
	if res.FileCount == 0 {
		t.Error("FileCount = 0; default tablespace should have many files")
	}
	if res.UniqueChunkCount == 0 {
		t.Error("UniqueChunkCount = 0; should be >0 for a real PG dump")
	}
	if res.TotalChunkRefs < res.UniqueChunkCount {
		t.Errorf("TotalChunkRefs (%d) should be >= UniqueChunkCount (%d)",
			res.TotalChunkRefs, res.UniqueChunkCount)
	}
	if res.LogicalBytes == 0 {
		t.Error("LogicalBytes = 0")
	}
	if res.Duration <= 0 {
		t.Error("Duration not populated")
	}

	// Event stream sanity.
	wantEvents := map[string]bool{
		"backup.pg_probed":       false,
		"backup.identified":      false,
		"backup.started":         false,
		"backup.stream_complete": false,
		"backup.committed":       false,
	}
	for _, ev := range events {
		key := ev.Component + "." + ev.Op
		if _, ok := wantEvents[key]; ok {
			wantEvents[key] = true
		}
	}
	for k, seen := range wantEvents {
		if !seen {
			t.Errorf("missing expected event: %s", k)
		}
	}

	// Reopen the repo from scratch and verify we can read the
	// just-committed manifest with a fresh ManifestStore.
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatalf("re-open repo: %v", err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	loaded, err := store.Read(context.Background(), "db1", res.BackupID, verifier)
	if err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	if loaded.BackupID != res.BackupID {
		t.Errorf("loaded BackupID = %q, want %q", loaded.BackupID, res.BackupID)
	}
	if want := testkit.ExpectedPGMajorInt(); loaded.PGVersion != want {
		t.Errorf("loaded PGVersion = %d, want %d", loaded.PGVersion, want)
	}
	if len(loaded.Files) != res.FileCount {
		t.Errorf("file count differs across round-trip: %d vs %d", len(loaded.Files), res.FileCount)
	}

	// Reconstitute one specific file (PG_VERSION) from the manifest's
	// chunk refs and confirm the bytes are sane.
	cas := repo.NewCAS(sp)
	var pgVersionRefs []backup.ChunkRef
	for _, f := range loaded.Files {
		if strings.HasSuffix(f.Path, "PG_VERSION") {
			pgVersionRefs = f.Chunks
			break
		}
	}
	if pgVersionRefs == nil {
		t.Fatal("PG_VERSION not in manifest's Files")
	}
	var rebuilt bytes.Buffer
	for _, ref := range pgVersionRefs {
		body, err := cas.GetChunkBytes(context.Background(), ref.Hash)
		if err != nil {
			t.Fatalf("get chunk %s: %v", ref.Hash, err)
		}
		rebuilt.Write(body)
	}
	if want, got := testkit.ExpectedPGMajor(), strings.TrimSpace(rebuilt.String()); got != want {
		t.Errorf("PG_VERSION reconstituted = %q, want %q", got, want)
	}
}

func TestIntegration_TakeBackup_RejectsRepoMissing(t *testing.T) {
	srv := testkit.StartPostgres(t)
	missingRepo := "file://" + t.TempDir() // dir exists but no HSREPO

	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)

	_, err := runner.Take(context.Background(), runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      missingRepo,
		Deployment:   "db1",
		Signer:       signer,
	})
	if err == nil {
		t.Fatal("expected error when repo doesn't exist")
	}
	if !strings.Contains(err.Error(), "repo") {
		t.Errorf("error should mention repo: %v", err)
	}
}

func TestIntegration_TakeBackup_TwoBackups_DedupSecond(t *testing.T) {
	srv := testkit.StartPostgres(t)
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		Fast:         true,
	})
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	second, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		Fast:         true,
	})
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if first.BackupID == second.BackupID {
		t.Error("two backup IDs collided")
	}
	// The second backup should have unique-chunk count comparable to
	// the first (most chunks in PG's test cluster are unchanged
	// between consecutive base backups). We don't assert exact dedup
	// because PG's WAL / clog / pg_stat tickling makes some chunks new.
	// But the unique-chunk-bytes shouldn't have grown massively.
	t.Logf("first  backup: unique=%d bytes=%d total_refs=%d",
		first.UniqueChunkCount, first.UniqueChunkBytes, first.TotalChunkRefs)
	t.Logf("second backup: unique=%d bytes=%d total_refs=%d",
		second.UniqueChunkCount, second.UniqueChunkBytes, second.TotalChunkRefs)
}

// TestIntegration_TakeBackup_TwoEncryptedBackups_BothDecrypt is the
// regression for issue #28.  With per-backup random DEKs and CAS
// dedup'd chunks, the second encrypted backup's manifest used to
// reference chunks encrypted under the FIRST backup's DEK, so verify
// and restore of the second backup failed with AES-GCM auth errors on
// every shared chunk.  The runner now reuses an existing wrapped DEK
// for the same KEKRef, so both backups share a single underlying DEK
// and every chunk decrypts under either manifest's wrapped form.
func TestIntegration_TakeBackup_TwoEncryptedBackups_BothDecrypt(t *testing.T) {
	srv := testkit.StartPostgres(t)
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	encCfg := &runner.EncryptionConfig{KEK: kek, KEKRef: "local:default"}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		Encryption:   encCfg,
		Fast:         true,
	})
	if err != nil {
		t.Fatalf("first encrypted backup: %v", err)
	}
	second, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN,
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		Encryption:   encCfg,
		Fast:         true,
	})
	if err != nil {
		t.Fatalf("second encrypted backup: %v", err)
	}

	// Both manifests must unwrap to the same plaintext DEK.  Before
	// the fix this was random-different and chunks shared by dedup
	// were unreadable from the second manifest.
	dek1, err := unwrapManifestDEK(t, repoURL, "db1", first.BackupID, verifier, kek)
	if err != nil {
		t.Fatalf("unwrap first manifest's DEK: %v", err)
	}
	dek2, err := unwrapManifestDEK(t, repoURL, "db1", second.BackupID, verifier, kek)
	if err != nil {
		t.Fatalf("unwrap second manifest's DEK: %v", err)
	}
	if !bytes.Equal(dek1, dek2) {
		t.Fatalf("DEK mismatch between backups — dedup'd chunks will be unreadable from one of them.\n  first  = %x\n  second = %x", dek1, dek2)
	}

	// Sanity: re-fetch one chunk that's referenced by BOTH manifests
	// and confirm it round-trips through an encryptor built from
	// either backup's DEK.  Picks the first chunk shared between
	// the two manifests.
	sp, err := openRepoSP(ctx, repoURL)
	if err != nil {
		t.Fatalf("open sp: %v", err)
	}
	defer sp.Close()

	shared, ok := firstSharedChunk(ctx, t, sp, verifier, "db1", first.BackupID, second.BackupID)
	if !ok {
		t.Skip("no chunk shared between the two backups (PG tickled every chunk); fix still verified by DEK-equality check above")
	}

	for _, dek := range [][]byte{dek1, dek2} {
		enc, err := aesgcm.New(dek)
		if err != nil {
			t.Fatalf("aesgcm.New: %v", err)
		}
		// Build the CAS through casdefault so the read-side
		// registry sees the same compression codecs the runner
		// wrote with (zstd, primarily).  Bare `repo.NewCAS` only
		// knows AlgoNone and would fail every chunk with
		// "compression: unknown algorithm" regardless of whether
		// the DEK matched — which would mask the actual fix the
		// test is here to lock in.
		cas := casdefault.NewEncrypted(sp, enc)
		if _, err := cas.GetChunkBytes(ctx, shared); err != nil {
			t.Fatalf("decrypt shared chunk with DEK: %v (the dek-reuse fix did not apply)", err)
		}
	}
}

func openRepoSP(ctx context.Context, repoURL string) (storage.StoragePlugin, error) {
	_, sp, err := repo.Open(ctx, repoURL)
	return sp, err
}

func unwrapManifestDEK(t *testing.T, repoURL, deployment, backupID string, verifier *backup.Verifier, kek [encryption.KeyLen]byte) ([]byte, error) {
	t.Helper()
	ctx := context.Background()
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		return nil, err
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	m, err := store.Read(ctx, deployment, backupID, verifier)
	if err != nil {
		return nil, err
	}
	if m.Encryption == nil {
		return nil, nil
	}
	wrapped, err := decodeBase64Std(m.Encryption.WrappedDEK)
	if err != nil {
		return nil, err
	}
	dek, err := encryption.Unwrap(kek, wrapped)
	if err != nil {
		return nil, err
	}
	return dek[:], nil
}

// firstSharedChunk returns the first repo.Hash that appears in both
// manifests' file → chunks lists.
func firstSharedChunk(ctx context.Context, t *testing.T, sp storage.StoragePlugin, verifier *backup.Verifier, deployment, a, b string) (repo.Hash, bool) {
	t.Helper()
	store := backup.NewManifestStore(sp)
	ma, err := store.Read(ctx, deployment, a, verifier)
	if err != nil {
		t.Fatalf("read manifest a: %v", err)
	}
	mb, err := store.Read(ctx, deployment, b, verifier)
	if err != nil {
		t.Fatalf("read manifest b: %v", err)
	}
	aHashes := map[repo.Hash]struct{}{}
	for _, f := range ma.Files {
		for _, c := range f.Chunks {
			aHashes[c.Hash] = struct{}{}
		}
	}
	for _, f := range mb.Files {
		for _, c := range f.Chunks {
			if _, ok := aHashes[c.Hash]; ok {
				return c.Hash, true
			}
		}
	}
	var zero repo.Hash
	return zero, false
}

func decodeBase64Std(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

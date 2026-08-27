package bundle_test

// The air-gap transfer's only truth is a restore on the far side.
//
// This feature shipped broken from v1.0 to v1.3 — every chunk of
// every compressed bundle was rejected at import — while its tests
// passed, because they compared repository objects instead of reading
// them back. The repaired import got tests that verify chunks land;
// this closes the loop the original gap teaches: export, import into
// the receiving repo, then run the REAL restore there and compare
// bytes with the original content.

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"net/url"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// newRepoWithURL is newRepo plus repo.Init and the file:// URL —
// restore opens the repository through the front door, which checks
// the HSREPO version gate the bare test fixture skips.
func newRepoWithURL(t *testing.T) (storage.StoragePlugin, string) {
	t.Helper()
	dir := t.TempDir()
	repoURL := "file://" + dir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("sp.Open: %v", err)
	}
	t.Cleanup(func() { sp.Close() })
	return sp, repoURL
}

func TestExportImportRestore_ByteIdentical(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t)
	// Chunks must go through the REAL CAS: the raw-bytes fixture the
	// other bundle tests use has no envelope, and restore decodes the
	// envelope — the exact layer whose mismatch hid the original
	// import bug. alpha/beta are two chunks of one 34-byte file.
	alpha, beta := []byte("chunk-alpha-bytes"), []byte("chunk-beta--byte!")
	cas := casdefault.New(src)
	ia, err := cas.PutChunk(ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := cas.PutChunk(ctx, beta)
	if err != nil {
		t.Fatal(err)
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260428T120000Z",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170,
		SystemIdentifier: "7388123456789012345",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		StartedAt:        time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		StoppedAt:        time.Date(2026, 4, 28, 12, 8, 23, 0, time.UTC),
		Compression:      "none",
		BackupLabel:      "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n",
	}
	m.Files = []backup.FileEntry{{
		Path: "base/16384/2619",
		Size: int64(len(alpha) + len(beta)),
		Chunks: []backup.ChunkRef{
			{Hash: ia.Hash, Offset: 0, Len: int64(len(alpha))},
			{Hash: ib.Hash, Offset: int64(len(alpha)), Len: int64(len(beta))},
		},
	}}
	// Restore refuses unverified manifests (usage.missing_verifier),
	// so the round-trip must carry a real signature end to end —
	// which is itself part of the proof: the bundle must preserve
	// the signed bytes verbatim or the far side cannot restore.
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	if err := backup.NewManifestStore(src).Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit signed manifest: %v", err)
	}

	var buf bytes.Buffer
	if _, err := bundle.Export(ctx, src, &buf, bundle.ExportOptions{Deployment: "db1"}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	dst, dstURL := newRepoWithURL(t)
	if _, err := bundle.Import(ctx, bytes.NewReader(buf.Bytes()), dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	target := filepath.Join(t.TempDir(), "restored-from-import")
	res, err := restore.Restore(ctx, restore.Options{
		RepoURL:    dstURL,
		Deployment: "db1",
		BackupID:   m.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
	})
	if err != nil {
		t.Fatalf("restore from imported repo: %v", err)
	}
	if res.FileCount == 0 {
		t.Fatal("restore from imported repo materialised zero files")
	}

	got, err := os.ReadFile(filepath.Join(target, "base/16384/2619"))
	if err != nil {
		t.Fatalf("read materialised file: %v", err)
	}
	want := append(append([]byte{}, alpha...), beta...)
	if !bytes.Equal(got, want) {
		t.Fatalf("restored bytes differ from the original:\n got: %q\nwant: %q", got, want)
	}
}

package backup_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// flakyPut fails the first Put of a manifest after consuming half the body
// (a connection dropped mid-upload), then behaves.
type flakyPut struct {
	storage.StoragePlugin
	failures int
}

func (f *flakyPut) Put(ctx context.Context, key string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	if f.failures > 0 && filepath.Base(key) != "" && o.ContentLength > 1000 {
		f.failures--
		_, _ = io.CopyN(io.Discard, r, o.ContentLength/2)
		return storage.PutResult{}, errors.New("injected: connection reset mid-upload")
	}
	return f.StoragePlugin.Put(ctx, key, r, o)
}

func spooled(t *testing.T, n int) (*backup.ManifestSpool, *backup.Manifest) {
	t.Helper()
	spool, err := backup.NewManifestSpool(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	fs := manyFiles(n)
	for i := range fs {
		if err := spool.Add(&fs[i]); err != nil {
			t.Fatal(err)
		}
	}
	h := validManifest()
	h.Files = nil
	return spool, h
}

func TestCommitSpooled_FailedUploadLeavesNoManifestAndRetrySucceeds(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	_, sp := storeAt(t)
	flaky := &flakyPut{StoragePlugin: sp, failures: 1}
	store := backup.NewManifestStore(flaky)
	ctx := context.Background()
	spool, h := spooled(t, 300)

	if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Fatal("commit succeeded despite a failed upload")
	}
	if _, err := sp.Stat(ctx, backup.PrimaryPath(h.Deployment, h.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a failed commit left a primary manifest behind: %v", err)
	}
	if _, err := store.Read(ctx, h.Deployment, h.BackupID, verifier); err == nil {
		t.Fatal("a backup whose commit failed is readable")
	}
	// The spool survives the failure; the same entries commit on retry.
	h2 := validManifest()
	h2.Files = nil
	if err := store.CommitSpooled(ctx, h2, spool, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if m, err := store.ReadStreaming(ctx, h2.Deployment, h2.BackupID, verifier); err != nil || m.NumFiles() != 300 {
		t.Fatalf("retry result: %v files=%v", err, m)
	}
}

func TestManifestSpool_UnwritableSpoolDirFailsCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := backup.NewManifestSpool(dir, 0); err == nil {
		t.Fatal("spool created in an unwritable directory")
	}
}

func TestCommitSpooled_CancelledContextLeavesNothing(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	store, sp := storeAt(t)
	spool, h := spooled(t, 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Fatal("cancelled commit succeeded")
	}
	if _, err := sp.Stat(context.Background(), backup.PrimaryPath(h.Deployment, h.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cancelled commit left a manifest: %v", err)
	}
}

// breakSpillDirs makes every spill directory under dir read-only, so the
// next run file a hash set tries to create fails.
func breakSpillDirs(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			p := filepath.Join(dir, e.Name())
			if err := os.Chmod(p, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
			n++
		}
	}
	if n == 0 {
		t.Fatal("fixture: no spill directory yet")
	}
	probe := filepath.Join(dir, ents[0].Name(), "probe")
	if f, err := os.Create(probe); err == nil {
		f.Close()
		t.Skip("directory permissions are not enforced (running as root?)")
	}
}

func spillFileEntry(i int) backup.FileEntry {
	return backup.FileEntry{
		Path: fmt.Sprintf("base/16384/%d", i), Size: 10, Mode: 0o600,
		Chunks: []backup.ChunkRef{{Hash: sha256.Sum256([]byte(fmt.Sprint("c", i))), Len: 10}},
	}
}

// A path set that lost a run file to a spill I/O error can no longer vouch
// for uniqueness, so CommitSpooled must refuse to publish.
func TestCommitSpooled_PathSetSpillErrorRefusesCommit(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	_, sp := storeAt(t)
	store := backup.NewManifestStore(sp)
	dir := t.TempDir()
	spool, err := backup.NewManifestSpool(dir, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	for i := 0; i < 5; i++ { // fills the heap budget: the first run is written
		fe := spillFileEntry(i)
		fe.Chunks[0].Hash = [32]byte{9} // one shared chunk: only the path set spills
		if err := spool.Add(&fe); err != nil {
			t.Fatal(err)
		}
	}
	breakSpillDirs(t, dir)
	var addErr error
	for i := 5; i < 40 && addErr == nil; i++ {
		fe := spillFileEntry(i)
		fe.Chunks[0].Hash = [32]byte{9}
		addErr = spool.Add(&fe)
	}
	h := validManifest()
	h.Files = nil
	if err := store.CommitSpooled(context.Background(), h, spool, signer, backup.CommitOptions{SkipReplica: true}); err == nil {
		t.Fatal("CommitSpooled published a manifest whose path set lost spill data")
	}
}

// A damaged path set answers "present" for a path it cannot read; Add must
// say it hit an I/O error, not claim the path is a duplicate.
func TestManifestSpool_SpillErrorIsNotReportedAsDuplicate(t *testing.T) {
	dir := t.TempDir()
	spool, err := backup.NewManifestSpool(dir, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	for i := 0; i < 5; i++ {
		fe := spillFileEntry(i)
		if err := spool.Add(&fe); err != nil {
			t.Fatal(err)
		}
	}
	// Damage the run file itself: every lookup that reaches it now fails.
	var damaged bool
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasPrefix(filepath.Base(p), "run-") {
			if os.Truncate(p, fi.Size()/2) == nil {
				damaged = true
			}
		}
		return nil
	})
	if !damaged {
		t.Fatal("fixture: no run file to damage")
	}
	var got error
	for i := 0; i < 5 && got == nil; i++ {
		fe := spillFileEntry(i + 100)
		got = spool.Add(&fe)
	}
	if got == nil {
		t.Skip("no lookup reached the damaged run (bloom filter said absent)")
	}
	if strings.Contains(got.Error(), "duplicate file path") {
		t.Fatalf("I/O failure reported as a duplicate: %v", got)
	}
}

// The unique-chunk counters feed the manifest's totals; when the chunk set
// loses spill data they are wrong, and CommitSpooled must not publish them.
func TestCommitSpooled_ChunkSetSpillErrorRefusesCommit(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	_, sp := storeAt(t)
	store := backup.NewManifestStore(sp)
	dir := t.TempDir()
	spool, err := backup.NewManifestSpool(dir, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	// One file at a time with many distinct chunks: only the chunk set
	// outgrows its heap budget; the path set stays tiny and never spills.
	file := func(i, chunks int) backup.FileEntry {
		fe := backup.FileEntry{Path: fmt.Sprintf("base/1/%d", i), Size: int64(chunks), Mode: 0o600}
		for c := 0; c < chunks; c++ {
			fe.Chunks = append(fe.Chunks, backup.ChunkRef{
				Hash: sha256.Sum256([]byte(fmt.Sprint("f", i, "c", c))), Offset: int64(c), Len: 1})
		}
		return fe
	}
	fe := file(0, 5)
	if err := spool.Add(&fe); err != nil {
		t.Fatal(err)
	}
	breakSpillDirs(t, dir)
	fe = file(1, 40)
	_ = spool.Add(&fe)
	h := validManifest()
	h.Files = nil
	if err := store.CommitSpooled(context.Background(), h, spool, signer, backup.CommitOptions{SkipReplica: true}); err == nil {
		t.Fatal("CommitSpooled published a manifest whose unique-chunk counters lost spill data")
	}
}

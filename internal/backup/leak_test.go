package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

func leakCycles(small, big int) int {
	if os.Getenv("PGHS_SCALE") == "1" {
		return big
	}
	return small
}

// TestLeak_SpoolLifecycle drives the whole large-manifest lifecycle many
// times in one process (spool -> sign via mmap -> commit -> streamed
// verify -> EachFile -> Close), mixing in every abandoned/failed variant,
// and proves fds, mmaps, goroutines, temp files and heap all return to
// their baseline.
func TestLeak_SpoolLifecycle(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	ctx := context.Background()
	store, sp := storeAt(t)
	_ = sp
	spoolDir := t.TempDir()

	base := leakcheck.Take()
	n := leakCycles(150, 1500)
	leakcheck.Plateau(t, n, n/16, 16<<20, func(i int) {
		spool, err := backup.NewManifestSpool(spoolDir, 40) // tiny: force the path set + chunk set to spill
		if err != nil {
			t.Fatal(err)
		}
		files := manyFiles(150)
		for j := range files {
			files[j].Chunks = []backup.ChunkRef{{Hash: sha(i*1000 + j), Offset: 0, Len: 30}}
			if err := spool.Add(&files[j]); err != nil {
				t.Fatal(err)
			}
		}
		// One streamed file too.
		if err := spool.BeginFile(&backup.FileEntry{Path: "base/streamed", Size: 20, Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		_ = spool.AddChunk(backup.ChunkRef{Hash: sha(-i - 1), Offset: 0, Len: 20})
		if err := spool.EndFile(); err != nil {
			t.Fatal(err)
		}
		switch i % 5 {
		case 0, 1: // normal commit + streamed read + walk
			h := validManifest()
			h.Files = nil
			h.BackupID = fmt.Sprintf("db1.full.20260506T120000Z.%04d", i)
			if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{SkipReplica: true}); err != nil {
				t.Fatalf("commit: %v", err)
			}
			m, err := store.ReadStreaming(ctx, h.Deployment, h.BackupID, verifier)
			if err != nil {
				t.Fatal(err)
			}
			c := 0
			if err := m.EachFile(ctx, func(*backup.FileEntry) error { c++; return nil }); err != nil || c != 151 {
				t.Fatalf("walk: %v %d", err, c)
			}
			if err := m.Validate(); err != nil {
				t.Fatal(err)
			}
			// delete so the repo itself does not grow the heap measurement
			_ = sp.Delete(ctx, backup.PrimaryPath(h.Deployment, h.BackupID))
		case 2: // abandoned: never committed
		case 3: // torn streamed entry then commit refused
			_ = spool.BeginFile(&backup.FileEntry{Path: "base/torn", Size: 20, Mode: 0o600})
			h := validManifest()
			h.Files = nil
			if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{SkipReplica: true}); err == nil {
				t.Fatal("torn spool committed")
			}
		case 4: // commit that fails at upload
			flaky := backup.NewManifestStore(&flakyPut{StoragePlugin: sp, failures: 5})
			h := validManifest()
			h.Files = nil
			if err := flaky.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{SkipReplica: true}); err == nil {
				t.Fatal("expected upload failure")
			}
		}
		if err := spool.Close(); err != nil {
			t.Fatal(err)
		}
		_ = spool.Close()
	})
	if left := leakcheck.DirEntries(spoolDir); len(left) != 0 {
		t.Fatalf("spool temp files left behind: %v", left)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

// StreamManifest on a damaged / aborted / tampered document must clean up
// its canonical spool and path set on every exit path.
func TestLeak_StreamManifestErrorPaths(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	m := validManifest()
	m.Files = manyFiles(400)
	store, sp0 := storeAt(t)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{SkipReplica: true}); err != nil {
		t.Fatal(err)
	}
	body := getRaw(t, sp0, backup.PrimaryPath(m.Deployment, m.BackupID))
	var err error
	dir := t.TempDir()
	base := leakcheck.Take()
	n := leakCycles(200, 2000)
	for i := 0; i < n; i++ {
		opts := backup.StreamOptions{Verifier: verifier, Validate: true, SpoolDir: dir, PathSetMemEntries: 30}
		switch i % 4 {
		case 0: // truncated mid-entries
			_, err = backup.StreamManifest(bytes.NewReader(body[:len(body)/2]), opts, nil)
		case 1: // callback aborts
			cnt := 0
			_, err = backup.StreamManifest(bytes.NewReader(body), opts, func(*backup.FileEntry) error {
				cnt++
				if cnt == 100 {
					return errors.New("abort")
				}
				return nil
			})
		case 2: // tampered byte -> signature failure at the very end
			b := append([]byte(nil), body...)
			j := bytes.Index(b, []byte("base/16384/<5>"))
			b[j+10] = '9'
			_, err = backup.StreamManifest(bytes.NewReader(b), opts, nil)
		case 3: // reader error
			_, err = backup.StreamManifest(io.MultiReader(bytes.NewReader(body[:len(body)/3]), errReader{}), opts, nil)
		}
		if err == nil {
			t.Fatalf("case %d: expected error", i%4)
		}
	}
	if left := leakcheck.DirEntries(dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

func sha(i int) repo.Hash { return repo.HashOf([]byte(fmt.Sprint("leak-", i))) }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("injected read error") }

package restore_test

// restore_parallel_test.go — issue #90: Restore writes files on a
// worker pool instead of one at a time, and verify --full's scratch
// restores skip the per-file fsync. These tests pin what must not
// change: every byte, the counters, resume after a mid-restore failure,
// and that the streamed manifest's per-entry buffers are copied before
// they reach a worker (Restore reads manifests with ReadStreaming).

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	mathrand "math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

const manyBackupID = "db1.full.20260428T130000Z.0002"

type manyFixture struct {
	root     string
	repoURL  string
	verifier *backup.Verifier
	files    map[string][]byte
	order    []string            // manifest order
	chunks   map[string][][]byte // path -> chunk bodies, in order
	hashes   map[string][]repo.Hash
}

// newManyFileFixture commits a manifest of n files: empty files,
// one-chunk files, multi-chunk files (up to 6 chunks with real
// offsets), and files that share chunk content (dedup), spread over
// several directories.
func newManyFileFixture(t *testing.T, n int) *manyFixture {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)

	r := mathrand.New(mathrand.NewSource(0x90))
	shared := randomBytes(r, 8192)
	fx := &manyFixture{root: root, repoURL: repoURL, files: map[string][]byte{},
		chunks: map[string][][]byte{}, hashes: map[string][]repo.Hash{}}
	var entries []backup.FileEntry
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("base/%d/%d", 16384+i%7, 20000+i)
		var parts [][]byte
		switch {
		case i%11 == 0: // empty
		case i%5 == 0: // multi-chunk
			for k := 0; k < 2+i%5; k++ {
				parts = append(parts, randomBytes(r, 1+r.Intn(40_000)))
			}
		case i%13 == 0: // dedup: identical content across files
			parts = [][]byte{shared}
		default:
			parts = [][]byte{randomBytes(r, 1+r.Intn(20_000))}
		}
		var body []byte
		entry := backup.FileEntry{Path: path, Mode: 0o600}
		for _, p := range parts {
			info, err := cas.PutChunk(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			entry.Chunks = append(entry.Chunks, backup.ChunkRef{Hash: info.Hash, Offset: int64(len(body)), Len: info.Size})
			fx.hashes[path] = append(fx.hashes[path], info.Hash)
			body = append(body, p...)
		}
		entry.Size = int64(len(body))
		entries = append(entries, entry)
		fx.files[path] = body
		fx.chunks[path] = parts
		fx.order = append(fx.order, path)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	fx.verifier, _ = backup.LoadVerifier(pub)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         manyBackupID,
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            entries,
		Dirs:             []backup.DirEntry{{Path: "pg_wal", Mode: 0o700}},
		BackupLabel:      "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n",
	}
	plantArchivedWAL(t, sp, m.Deployment, m.Timeline)
	if err := backup.NewManifestStore(sp).Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return fx
}

func (fx *manyFixture) assertRestored(t *testing.T, target string) {
	t.Helper()
	for _, path := range fx.order {
		got, err := os.ReadFile(filepath.Join(target, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(got, fx.files[path]) {
			t.Fatalf("%s: restored %d bytes differ from the backed-up %d", path, len(got), len(fx.files[path]))
		}
	}
}

func (fx *manyFixture) totals() (bytes int64, chunks int) {
	for _, p := range fx.order {
		bytes += int64(len(fx.files[p]))
		chunks += len(fx.chunks[p])
	}
	return
}

// Every file, byte for byte, with exact counters — durable and scratch.
// Run under -race: the counters and the checkpoint are updated from the
// worker pool.
func TestRestore_ManyFiles_ParallelRoundTrip(t *testing.T) {
	fx := newManyFileFixture(t, 600)
	wantBytes, wantChunks := fx.totals()
	for _, scratch := range []bool{false, true} {
		t.Run(fmt.Sprintf("scratch=%v", scratch), func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "restored")
			res, err := restore.Restore(context.Background(), restore.Options{
				RepoURL:       fx.repoURL,
				Deployment:    "db1",
				BackupID:      manyBackupID,
				TargetDir:     target,
				Verifier:      fx.verifier,
				ScratchTarget: scratch,
			})
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if res.FileCount != len(fx.order) || res.BytesWritten != wantBytes || res.ChunksFetched != wantChunks {
				t.Fatalf("Result files=%d bytes=%d chunks=%d; want %d/%d/%d",
					res.FileCount, res.BytesWritten, res.ChunksFetched, len(fx.order), wantBytes, wantChunks)
			}
			fx.assertRestored(t, target)
		})
	}
}

// A chunk that fails verification mid-restore stops the restore with
// the failing file named. Files that finished are checkpointed; files
// in flight are not. Once the chunk is repaired, re-running resumes:
// the result is byte-exact and no byte is counted twice.
func TestRestore_ManyFiles_ResumeAfterMidwayFailure(t *testing.T) {
	fx := newManyFileFixture(t, 600)
	wantBytes, wantChunks := fx.totals()

	// Corrupt a chunk used by exactly one file, two thirds of the way in.
	var victim string
	var victimHash repo.Hash
	var good []byte
	used := map[repo.Hash]int{}
	for _, p := range fx.order {
		for _, h := range fx.hashes[p] {
			used[h]++
		}
	}
	for _, p := range fx.order[400:] {
		if hs := fx.hashes[p]; len(hs) == 1 && used[hs[0]] == 1 {
			victim, victimHash = p, hs[0]
			break
		}
	}
	if victim == "" {
		t.Fatal("fixture has no single-use chunk to corrupt")
	}
	chunkPath := filepath.Join(fx.root, filepath.FromSlash(repo.ChunkKey(victimHash)))
	good, err := os.ReadFile(chunkPath)
	if err != nil {
		t.Fatalf("chunk file %s: %v", chunkPath, err)
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xff
	if err := os.WriteFile(chunkPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	opts := restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   manyBackupID,
		TargetDir:  target,
		Verifier:   fx.verifier,
	}
	_, err = restore.Restore(context.Background(), opts)
	if err == nil {
		t.Fatal("Restore succeeded over a corrupted chunk")
	}
	if !strings.Contains(err.Error(), victim) {
		t.Fatalf("error does not name the failing file %s: %v", victim, err)
	}

	if err := os.WriteFile(chunkPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := restore.Restore(context.Background(), opts)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.BytesWritten != wantBytes || res.ChunksFetched != wantChunks {
		t.Fatalf("resumed Result bytes=%d chunks=%d; want %d/%d (a file counted twice or not at all)",
			res.BytesWritten, res.ChunksFetched, wantBytes, wantChunks)
	}
	fx.assertRestored(t, target)
}

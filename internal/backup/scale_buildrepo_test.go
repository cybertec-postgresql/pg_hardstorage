package backup_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// TestScale_BuildHugeRepo populates an existing, `repo init`-ed repository
// with huge manifests so the real CLI can be pointed at it and measured
// (peak RSS, wall time) command by command. It is a fixture builder, not a
// test: PGHS_SCALE_REPO=<repo dir> and PGHS_SCALE_KEYRING=<keyring dir>
// select the repo and the keyring that signs it.
//
//	big   a full backup of PGHS_SCALE_CHUNKS chunk references (default 6M,
//	      over the 1 GiB held-reader cap) and an incremental child of it
//	      with 1/6 as many; a sample of the referenced chunks exist
//	many  a full backup of PGHS_SCALE_FILES files (default 300k) of one
//	      real chunk each, drawn from 1000 real chunk bodies
func TestScale_BuildHugeRepo(t *testing.T) {
	dir, keyring := os.Getenv("PGHS_SCALE_REPO"), os.Getenv("PGHS_SCALE_KEYRING")
	if dir == "" || keyring == "" {
		t.Skip("set PGHS_SCALE_REPO and PGHS_SCALE_KEYRING to build the huge-repo fixture")
	}
	n, nf := 6_000_000, 300_000
	if v := os.Getenv("PGHS_SCALE_CHUNKS"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("PGHS_SCALE_FILES"); v != "" {
		nf, _ = strconv.Atoi(v)
	}
	ctx := context.Background()
	signer, _, err := keystore.LoadOrGenerate(keyring)
	if err != nil {
		t.Fatal(err)
	}
	_, sp, err := repo.Open(ctx, "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	store := backup.NewManifestStore(sp)
	cas := casdefault.New(sp)

	chunkHash := func(i int) repo.Hash {
		var b [9]byte
		b[0] = 'c'
		for k := 0; k < 8; k++ {
			b[1+k] = byte(i >> (8 * k))
		}
		return repo.HashOf(b[:])
	}
	commit := func(h *backup.Manifest, spillMem int, gen func(emit func(backup.FileEntry))) {
		t.Helper()
		spool, err := backup.NewManifestSpool(t.TempDir(), spillMem)
		if err != nil {
			t.Fatal(err)
		}
		defer spool.Close()
		gen(func(fe backup.FileEntry) {
			if err := spool.Add(&fe); err != nil {
				t.Fatal(err)
			}
		})
		h.Files = nil
		if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{SkipReplica: true}); err != nil {
			t.Fatalf("CommitSpooled %s: %v", h.BackupID, err)
		}
	}
	// chunkRun emits files of 16384 chunks drawn from [lo,hi).
	chunkRun := func(lo, hi int) func(func(backup.FileEntry)) {
		return func(emit func(backup.FileEntry)) {
			for f, i := 0, lo; i < hi; f++ {
				c := min(16384, hi-i)
				fe := backup.FileEntry{Path: fmt.Sprintf("base/16384/%d", 100000+lo/16384+f), Mode: 0o600, Chunks: make([]backup.ChunkRef, c)}
				var off int64
				for k := range fe.Chunks {
					fe.Chunks[k] = backup.ChunkRef{Hash: chunkHash(i + k), Offset: off, Len: 65536}
					off += 65536
				}
				fe.Size = off
				i += c
				emit(fe)
			}
		}
	}
	now := time.Now().UTC()
	hdr := func(dep, id, parent string, t0 time.Time) *backup.Manifest {
		h := validManifest()
		h.Deployment, h.BackupID = dep, id
		h.StartedAt, h.StoppedAt = t0, t0.Add(time.Minute)
		if parent != "" {
			h.Type, h.ParentBackupID = backup.BackupTypeIncremental, parent
		}
		return h
	}

	const f1, f2 = "big.full.20260101T000000Z.0001", "big.incr.20260102T000000Z.0002"
	commit(hdr("big", f1, "", now.Add(-48*time.Hour)), 500_000, chunkRun(0, n))
	commit(hdr("big", f2, f1, now.Add(-24*time.Hour)), 500_000, chunkRun(n-n/6, n+n/6))
	// A sample of A's chunks exist as objects (same layout the gc proof uses).
	for i := 0; i < n; i += max(n/20000, 1) {
		h := chunkHash(i)
		if _, err := sp.Put(ctx, repo.ChunkKey(h), bytes.NewReader([]byte("x")), storage.PutOptions{ContentLength: 1}); err != nil {
			t.Fatal(err)
		}
	}

	// many: real chunks, many files.
	bodies := make([]repo.ChunkInfo, 1000)
	for i := range bodies {
		ci, err := cas.PutChunk(ctx, []byte(fmt.Sprintf("real chunk body %d %0100d", i, i)))
		if err != nil {
			t.Fatal(err)
		}
		bodies[i] = ci
	}
	commit(hdr("many", "many.full.20260103T000000Z.0001", "", now.Add(-time.Hour)), 500_000, func(emit func(backup.FileEntry)) {
		for i := 0; i < nf; i++ {
			ci := bodies[i%len(bodies)]
			emit(backup.FileEntry{
				Path: fmt.Sprintf("base/16384/t%07d/f%d", i/100, i), Mode: 0o600, Size: int64(ci.Size),
				Chunks: []backup.ChunkRef{{Hash: ci.Hash, Offset: 0, Len: int64(ci.Size)}},
			})
		}
	})
	t.Logf("built: big=%d chunk refs (+%d incremental), many=%d files", n, n/3, nf)
}

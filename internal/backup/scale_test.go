package backup_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestScale_SpoolCommitStreamVerify proves the whole large-manifest path
// holds only a bounded amount of heap: it spools PGHS_SCALE_CHUNKS chunk
// references (default 6M, a ~1.4 GB manifest — over the 1 GiB cap every
// held reader enforces), signs and commits them, then verifies and walks
// them as a stream, tracking the peak heap throughout. Opt-in:
// PGHS_SCALE=1.
func TestScale_SpoolCommitStreamVerify(t *testing.T) {
	if os.Getenv("PGHS_SCALE") != "1" {
		t.Skip("set PGHS_SCALE=1 to run the large-manifest proof")
	}
	n := 6_000_000
	if v := os.Getenv("PGHS_SCALE_CHUNKS"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	const chunksPerFile = 16384 // one 1 GiB relation segment
	files := (n + chunksPerFile - 1) / chunksPerFile

	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store, _ := storeAt(t)
	ctx := context.Background()

	var peak uint64
	sample := func() {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapInuse > peak {
			peak = ms.HeapInuse
		}
	}
	runtime.GC()

	spool, err := backup.NewManifestSpool(t.TempDir(), 500_000)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	var seed [8]byte
	made := 0
	for f := 0; f < files; f++ {
		c := chunksPerFile
		if made+c > n {
			c = n - made
		}
		fe := backup.FileEntry{Path: fmt.Sprintf("base/16384/%d", 100000+f), Mode: 0o600, Chunks: make([]backup.ChunkRef, c)}
		var off int64
		for i := range fe.Chunks {
			seed[0], seed[1], seed[2], seed[3] = byte(made>>24), byte(made>>16), byte(made>>8), byte(made)
			made++
			fe.Chunks[i] = backup.ChunkRef{Hash: repo.HashOf(seed[:]), Offset: off, Len: 65536}
			off += 65536
		}
		fe.Size = off
		if err := spool.Add(&fe); err != nil {
			t.Fatal(err)
		}
		if f%64 == 0 {
			sample()
		}
	}
	h := validManifest()
	h.Files = nil
	if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("CommitSpooled: %v", err)
	}
	sample()

	m, err := store.ReadStreaming(ctx, h.Deployment, h.BackupID, verifier)
	if err != nil {
		t.Fatalf("ReadStreaming (verifies the signature over the whole manifest): %v", err)
	}
	sample()
	var refs, count int64
	if err := m.EachFile(ctx, func(fe *backup.FileEntry) error {
		refs += int64(len(fe.Chunks))
		count++
		if count%64 == 0 {
			sample()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if refs != int64(n) || m.NumChunkRefs() != int64(n) {
		t.Fatalf("chunk refs: walked %d, header %d, want %d", refs, m.NumChunkRefs(), n)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("streamed Validate: %v", err)
	}
	sample()
	t.Logf("%d chunk refs in %d files: peak heap %d MiB", n, files, peak>>20)
	if limit := uint64(256 << 20); peak > limit {
		t.Errorf("peak heap %d MiB over the %d MiB bound", peak>>20, limit>>20)
	}
}

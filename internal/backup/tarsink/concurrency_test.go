package tarsink_test

import (
	"bytes"
	"context"
	"math/rand"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// sinkWithConcurrency builds a Sink whose per-file chunk pool runs at
// the given concurrency.
func sinkWithConcurrency(t *testing.T, n int) (*tarsink.Sink, *repo.CAS) {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	return tarsink.New(context.Background(), cas, tarsink.WithChunkConcurrency(n)), cas
}

// The per-file worker pool finishes chunks out of order; entry.Chunks
// must still come back in strict file-offset order, contiguous, and
// reconstruct byte-for-byte. Run across a range of concurrencies so
// the serial path (1) and a deep pool (16) are both covered.
func TestConcurrency_ChunkOrderAndReconstruction(t *testing.T) {
	// ~2 MiB of incompressible random data => many chunks, so the
	// pool genuinely runs out of order.
	body := make([]byte, 2<<20)
	rand.New(rand.NewSource(0x5EED)).Read(body)
	tarBytes := buildTar(t, []fileSpec{{name: "base/16384/99999", body: body}})

	for _, conc := range []int{1, 2, 8, 16} {
		t.Run("concurrency="+itoa(conc), func(t *testing.T) {
			sink, cas := sinkWithConcurrency(t, conc)
			if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 64*1024); err != nil {
				t.Fatalf("drive: %v", err)
			}
			files := sink.Files(0)
			if len(files) != 1 {
				t.Fatalf("Files(0) len = %d, want 1", len(files))
			}
			chunks := files[0].Chunks
			if len(chunks) < 4 {
				t.Fatalf("only %d chunks — too few to exercise the pool", len(chunks))
			}

			// Offsets must be strictly increasing AND contiguous:
			// chunk[i+1].Offset == chunk[i].Offset + chunk[i].Len.
			var want int64
			var rebuilt bytes.Buffer
			for i, ref := range chunks {
				if ref.Offset != want {
					t.Fatalf("chunk %d: Offset = %d, want %d (out-of-order reassembly)",
						i, ref.Offset, want)
				}
				want += ref.Len
				bs, err := cas.GetChunkBytes(context.Background(), ref.Hash)
				if err != nil {
					t.Fatalf("GetChunkBytes chunk %d: %v", i, err)
				}
				rebuilt.Write(bs)
			}
			if !bytes.Equal(rebuilt.Bytes(), body) {
				t.Errorf("reconstruction mismatch at concurrency %d", conc)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

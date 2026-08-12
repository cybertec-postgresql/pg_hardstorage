package partial

// materialise_offset_internal_test.go — the byte-scramble guard for the
// partial table-extraction path.
//
// materialiseOneFile writes a FileEntry's chunks in slice order. bug #32
// established that a manifest can carry chunks whose offsets don't march
// monotonically from the running write position; writing such a manifest
// in slice order produces a byte-scrambled file that still passes the
// total-size check. The full restore path guards this; this internal
// test pins that the partial mirror refuses it too, so `partial dump` /
// `partial restore` never hand back corrupt table data that looks
// correct.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestMaterialiseOneFile_RejectsReorderedChunks(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := casdefault.New(sp)

	a, err := cas.PutChunk(context.Background(), []byte("AAAA"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := cas.PutChunk(context.Background(), []byte("BBBB"))
	if err != nil {
		t.Fatal(err)
	}

	// Reordered: the offset-4 chunk is listed FIRST. Writing in slice
	// order would produce "BBBBAAAA" — 8 bytes, matching f.Size — silently
	// scrambled. The guard must refuse before the first write.
	scrambled := &backup.FileEntry{
		Path: "base/16384/2619", Size: 8, Mode: 0o600,
		Chunks: []backup.ChunkRef{
			{Hash: b.Hash, Offset: 4, Len: 4},
			{Hash: a.Hash, Offset: 0, Len: 4},
		},
	}
	target := t.TempDir()
	if _, err := materialiseOneFile(context.Background(), cas, target, scrambled); err == nil {
		t.Fatal("reordered chunks ACCEPTED — partial extraction would hand back a byte-scrambled " +
			"table file that passes the total-size check")
	} else if !strings.Contains(err.Error(), "out of order") {
		t.Errorf("wrong error for reordered chunks: %v", err)
	}

	// Positive control: correctly-ordered chunks materialise byte-exact.
	ordered := &backup.FileEntry{
		Path: "base/16384/2620", Size: 8, Mode: 0o600,
		Chunks: []backup.ChunkRef{
			{Hash: a.Hash, Offset: 0, Len: 4},
			{Hash: b.Hash, Offset: 4, Len: 4},
		},
	}
	n, err := materialiseOneFile(context.Background(), cas, target, ordered)
	if err != nil || n != 8 {
		t.Fatalf("correctly-ordered chunks failed: n=%d err=%v", n, err)
	}
	got, err := os.ReadFile(filepath.Join(target, "base/16384/2620"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "AAAABBBB" {
		t.Errorf("materialised bytes = %q, want AAAABBBB", got)
	}
}

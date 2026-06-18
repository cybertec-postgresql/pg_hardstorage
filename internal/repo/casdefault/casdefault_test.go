package casdefault_test

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func newSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func TestCasDefault_RoundTripCompressedChunk(t *testing.T) {
	sp := newSP(t)
	cas := casdefault.New(sp)

	// A redundant payload so zstd actually compresses.
	body := bytes.Repeat([]byte("backup data, backup data, backup data\n"), 1024)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("info.Size = %d, want %d (plaintext)", info.Size, len(body))
	}

	got, err := cas.GetChunkBytes(context.Background(), info.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("round-trip differs")
	}

	// On-disk size should be smaller than plaintext for a redundant payload.
	rc, err := cas.GetChunk(context.Background(), info.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var disk bytes.Buffer
	_, _ = disk.ReadFrom(rc)
	if disk.Len() >= len(body) {
		t.Errorf("on-disk size %d should be smaller than plaintext %d for redundant payload",
			disk.Len(), len(body))
	}
}

func TestCasDefault_RoundsTripVerbatimViaPlainCAS(t *testing.T) {
	// Chunks written by casdefault.New (zstd) MUST round-trip via a
	// CAS with zstd registered for read. A bare repo.NewCAS — which
	// only knows AlgoNone — should fail to read them, proving the
	// envelope is honoured.
	sp := newSP(t)
	cas := casdefault.New(sp)

	body := bytes.Repeat([]byte("compress me, please. "), 2000)
	info, _ := cas.PutChunk(context.Background(), body)

	plain := repo.NewCAS(sp) // no zstd registered
	if _, err := plain.GetChunkBytes(context.Background(), info.Hash); err == nil {
		t.Error("plain CAS should not be able to decode a zstd-compressed chunk")
	}

	// Same data through casdefault.New — round-trips fine.
	if got, err := cas.GetChunkBytes(context.Background(), info.Hash); err != nil {
		t.Errorf("default CAS round-trip: %v", err)
	} else if !bytes.Equal(got, body) {
		t.Error("default CAS round-trip differs")
	}
}

func TestCasDefault_TinyChunkUsesAlgoNone(t *testing.T) {
	// Tiny chunk: zstd's short-circuit path stores it as AlgoNone.
	// Round-trip must still work.
	sp := newSP(t)
	cas := casdefault.New(sp)

	body := []byte("tiny")
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cas.GetChunkBytes(context.Background(), info.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("tiny round-trip differs")
	}
}

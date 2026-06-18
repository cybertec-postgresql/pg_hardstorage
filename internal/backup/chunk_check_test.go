package backup_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// fsRepoForCheck builds a fresh fs-backed repo + CAS, returns
// the storage plugin so tests can directly Stat/Delete chunk
// keys after they've been planted via cas.PutChunk.
func fsRepoForCheck(t *testing.T) (storage.StoragePlugin, *repo.CAS) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp, repo.NewCAS(sp)
}

// manifestWithChunks constructs an unsigned in-memory Manifest
// referencing the supplied chunks across one file each. The
// CheckChunkExistence helper doesn't need signed manifests —
// it walks Files + Chunks directly.
func manifestWithChunks(infos ...repo.ChunkInfo) *backup.Manifest {
	files := make([]backup.FileEntry, 0, len(infos))
	for i, info := range infos {
		files = append(files, backup.FileEntry{
			Path: "data/" + string(rune('a'+i)),
			Size: info.Size,
			Chunks: []backup.ChunkRef{
				{Hash: info.Hash, Offset: 0, Len: info.Size},
			},
		})
	}
	return &backup.Manifest{
		Schema:   backup.Schema,
		BackupID: "check-test",
		Type:     backup.BackupTypeFull,
		Files:    files,
	}
}

// TestCheckChunkExistence_AllPresent: every referenced chunk
// exists in the repo → Present=N, Missing=∅, AllPresent=true.
func TestCheckChunkExistence_AllPresent(t *testing.T) {
	sp, cas := fsRepoForCheck(t)
	infoA, err := cas.PutChunk(context.Background(), []byte("alpha-content"))
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := cas.PutChunk(context.Background(), []byte("bravo-content"))
	if err != nil {
		t.Fatal(err)
	}
	m := manifestWithChunks(infoA, infoB)

	res, err := backup.CheckChunkExistence(context.Background(), sp, m)
	if err != nil {
		t.Fatalf("CheckChunkExistence: %v", err)
	}
	if res.TotalUnique != 2 {
		t.Errorf("TotalUnique = %d, want 2", res.TotalUnique)
	}
	if res.Present != 2 {
		t.Errorf("Present = %d, want 2", res.Present)
	}
	if len(res.Missing) != 0 {
		t.Errorf("Missing should be empty; got %v", res.Missing)
	}
	if !res.AllPresent() {
		t.Errorf("AllPresent should be true")
	}
}

// TestCheckChunkExistence_OneMissing: simulate chunk-GC by
// deleting one chunk via the storage plugin; the check
// reports Missing=[that hash], Present=N-1, AllPresent=false.
func TestCheckChunkExistence_OneMissing(t *testing.T) {
	sp, cas := fsRepoForCheck(t)
	infoA, _ := cas.PutChunk(context.Background(), []byte("present-bytes"))
	infoB, _ := cas.PutChunk(context.Background(), []byte("about-to-be-gced"))
	m := manifestWithChunks(infoA, infoB)

	if err := sp.Delete(context.Background(), repo.ChunkKey(infoB.Hash)); err != nil {
		t.Fatalf("simulate GC: %v", err)
	}

	res, err := backup.CheckChunkExistence(context.Background(), sp, m)
	if err != nil {
		t.Fatalf("CheckChunkExistence: %v", err)
	}
	if res.Present != 1 {
		t.Errorf("Present = %d, want 1", res.Present)
	}
	if len(res.Missing) != 1 || res.Missing[0] != infoB.Hash {
		t.Errorf("Missing = %v, want [%s]", res.Missing, infoB.Hash)
	}
	if res.AllPresent() {
		t.Errorf("AllPresent should be false")
	}
}

// TestCheckChunkExistence_DeduplicatesAcrossFiles: a chunk
// referenced by 3 files counts as 1 unique. CAS dedup is
// path-blind; the check honours that.
func TestCheckChunkExistence_DeduplicatesAcrossFiles(t *testing.T) {
	sp, cas := fsRepoForCheck(t)
	info, _ := cas.PutChunk(context.Background(), []byte("shared-content"))

	// Three files, all referencing the same chunk.
	m := &backup.Manifest{
		Schema:   backup.Schema,
		BackupID: "shared",
		Type:     backup.BackupTypeFull,
		Files: []backup.FileEntry{
			{Path: "data/a", Size: info.Size,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: info.Size}}},
			{Path: "data/b", Size: info.Size,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: info.Size}}},
			{Path: "data/c", Size: info.Size,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: info.Size}}},
		},
	}
	res, err := backup.CheckChunkExistence(context.Background(), sp, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalUnique != 1 {
		t.Errorf("TotalUnique = %d, want 1 (CAS-deduped)", res.TotalUnique)
	}
	if res.Present != 1 {
		t.Errorf("Present = %d, want 1", res.Present)
	}
}

// TestCheckChunkExistence_RejectsNil: defensive — nil manifest
// or storage plugin is a programmer error.
func TestCheckChunkExistence_RejectsNil(t *testing.T) {
	sp, _ := fsRepoForCheck(t)
	if _, err := backup.CheckChunkExistence(context.Background(), nil, &backup.Manifest{}); err == nil {
		t.Error("nil sp should error")
	}
	if _, err := backup.CheckChunkExistence(context.Background(), sp, nil); err == nil {
		t.Error("nil manifest should error")
	}
}

// TestCheckChunkExistence_EmptyManifest: a manifest with no
// files (e.g., a logical-only edge case) returns Total=Present=0
// and AllPresent=true. Vacuously satisfied.
func TestCheckChunkExistence_EmptyManifest(t *testing.T) {
	sp, _ := fsRepoForCheck(t)
	m := &backup.Manifest{Schema: backup.Schema, BackupID: "empty", Type: backup.BackupTypeFull}
	res, err := backup.CheckChunkExistence(context.Background(), sp, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalUnique != 0 || res.Present != 0 || len(res.Missing) != 0 {
		t.Errorf("expected zero counts; got %+v", res)
	}
	if !res.AllPresent() {
		t.Errorf("empty manifest should be vacuously AllPresent")
	}
}

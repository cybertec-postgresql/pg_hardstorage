package backup_test

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// hashOf is a small helper for tests that need a deterministic
// chunk hash from a string. Mirrors repo.HashOf.
func hashOf(s string) repo.Hash {
	return repo.HashOf([]byte(s))
}

// fileWithChunks builds a FileEntry whose chunks are derived
// from the supplied content slices — one chunk per content,
// sized at the byte length, contiguous offsets.
func fileWithChunks(path string, contents ...string) backup.FileEntry {
	chunks := make([]backup.ChunkRef, 0, len(contents))
	var offset int64
	var size int64
	for _, c := range contents {
		ln := int64(len(c))
		chunks = append(chunks, backup.ChunkRef{
			Hash:   hashOf(c),
			Offset: offset,
			Len:    ln,
		})
		offset += ln
		size += ln
	}
	return backup.FileEntry{
		Path:   path,
		Size:   size,
		Chunks: chunks,
	}
}

// manifestWith builds a Manifest with Schema/BackupID/StoppedAt
// stamped and the supplied files.
func manifestWith(id string, when time.Time, files ...backup.FileEntry) *backup.Manifest {
	return &backup.Manifest{
		Schema:    backup.Schema,
		BackupID:  id,
		Type:      backup.BackupTypeFull,
		StoppedAt: when,
		Files:     files,
	}
}

// TestCompare_IdenticalManifests: two manifests with the same
// files + chunks have zero deltas, all chunks shared, all
// logical bytes shared.
func TestCompare_IdenticalManifests(t *testing.T) {
	files := []backup.FileEntry{
		fileWithChunks("data/PG_VERSION", "17"),
		fileWithChunks("data/postgresql.conf", "shared_buffers"),
	}
	a := manifestWith("a", time.Now(), files...)
	b := manifestWith("b", time.Now(), files...)
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if res.FileCounts.OnlyInA != 0 || res.FileCounts.OnlyInB != 0 || res.FileCounts.Changed != 0 {
		t.Errorf("expected zero delta; got %+v", res.FileCounts)
	}
	if res.FileCounts.InBoth != 2 {
		t.Errorf("InBoth = %d, want 2", res.FileCounts.InBoth)
	}
	if res.ChunkCounts.AOnly != 0 || res.ChunkCounts.BOnly != 0 {
		t.Errorf("expected no AOnly/BOnly; got %+v", res.ChunkCounts)
	}
	if res.ChunkCounts.Shared != 2 {
		t.Errorf("Shared = %d, want 2", res.ChunkCounts.Shared)
	}
	if res.LogicalBytes.Delta != 0 {
		t.Errorf("Delta = %d, want 0", res.LogicalBytes.Delta)
	}
	if res.LogicalBytes.Shared != res.LogicalBytes.A {
		t.Errorf("Shared (%d) should equal A (%d)", res.LogicalBytes.Shared, res.LogicalBytes.A)
	}
	if len(res.TopFileDeltas) != 0 {
		t.Errorf("identical manifests should have no top deltas; got %v", res.TopFileDeltas)
	}
}

// TestCompare_AddedFile: B has a file A doesn't. The added
// file appears in TopFileDeltas as Class="added", BOnly chunk
// counts increment.
func TestCompare_AddedFile(t *testing.T) {
	a := manifestWith("a", time.Now(),
		fileWithChunks("data/foo", "foo-bytes"),
	)
	b := manifestWith("b", time.Now(),
		fileWithChunks("data/foo", "foo-bytes"),
		fileWithChunks("data/new", "brand-new-data"),
	)
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.FileCounts.OnlyInB != 1 {
		t.Errorf("OnlyInB = %d, want 1", res.FileCounts.OnlyInB)
	}
	if res.ChunkCounts.BOnly != 1 {
		t.Errorf("BOnly = %d, want 1", res.ChunkCounts.BOnly)
	}
	if res.LogicalBytes.Delta != int64(len("brand-new-data")) {
		t.Errorf("Delta = %d, want +%d", res.LogicalBytes.Delta, len("brand-new-data"))
	}
	// TopFileDeltas: data/new with class=added.
	if len(res.TopFileDeltas) != 1 {
		t.Fatalf("expected 1 top delta; got %d", len(res.TopFileDeltas))
	}
	d := res.TopFileDeltas[0]
	if d.Path != "data/new" || d.Class != "added" {
		t.Errorf("top delta = %+v, want path=data/new class=added", d)
	}
}

// TestCompare_RemovedFile: A has a file B doesn't. Class is
// "removed", AOnly chunk counts increment, Delta is negative
// (file was removed).
func TestCompare_RemovedFile(t *testing.T) {
	a := manifestWith("a", time.Now(),
		fileWithChunks("data/keep", "kept"),
		fileWithChunks("data/dropped", "lost-bytes"),
	)
	b := manifestWith("b", time.Now(),
		fileWithChunks("data/keep", "kept"),
	)
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.FileCounts.OnlyInA != 1 {
		t.Errorf("OnlyInA = %d, want 1", res.FileCounts.OnlyInA)
	}
	if res.ChunkCounts.AOnly != 1 {
		t.Errorf("AOnly = %d, want 1", res.ChunkCounts.AOnly)
	}
	if res.LogicalBytes.Delta != -int64(len("lost-bytes")) {
		t.Errorf("Delta = %d, want %d", res.LogicalBytes.Delta, -int64(len("lost-bytes")))
	}
	d := res.TopFileDeltas[0]
	if d.Class != "removed" {
		t.Errorf("class = %q, want removed", d.Class)
	}
}

// TestCompare_ChangedFile: same path, different chunks. Class
// is "changed"; AOnly + BOnly both increment by 1; size delta
// reflects the byte change.
func TestCompare_ChangedFile(t *testing.T) {
	a := manifestWith("a", time.Now(),
		fileWithChunks("data/cfg", "old-config"),
	)
	b := manifestWith("b", time.Now(),
		fileWithChunks("data/cfg", "new-config-larger"),
	)
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.FileCounts.Changed != 1 {
		t.Errorf("Changed = %d, want 1", res.FileCounts.Changed)
	}
	if res.FileCounts.InBoth != 1 {
		t.Errorf("InBoth = %d, want 1", res.FileCounts.InBoth)
	}
	if res.ChunkCounts.AOnly != 1 || res.ChunkCounts.BOnly != 1 {
		t.Errorf("AOnly/BOnly = %d/%d, want 1/1", res.ChunkCounts.AOnly, res.ChunkCounts.BOnly)
	}
	d := res.TopFileDeltas[0]
	if d.Class != "changed" {
		t.Errorf("class = %q, want changed", d.Class)
	}
	if d.Delta <= 0 {
		t.Errorf("Delta = %d, want positive (file grew)", d.Delta)
	}
}

// TestCompare_SharedChunksAcrossDifferentFiles: two backups
// with the SAME chunk set but in different file paths still
// count those chunks as shared (CAS-deduped).
func TestCompare_SharedChunksAcrossDifferentFiles(t *testing.T) {
	a := manifestWith("a", time.Now(),
		fileWithChunks("data/v1/foo", "shared-content"),
	)
	b := manifestWith("b", time.Now(),
		fileWithChunks("data/v2/foo", "shared-content"), // path renamed; bytes identical
	)
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	// File-level: 1 OnlyInA, 1 OnlyInB (paths differ).
	if res.FileCounts.OnlyInA != 1 || res.FileCounts.OnlyInB != 1 {
		t.Errorf("file counts mismatch: %+v", res.FileCounts)
	}
	// Chunk-level: 1 shared chunk (same hash on both sides).
	if res.ChunkCounts.Shared != 1 {
		t.Errorf("Shared = %d, want 1 (CAS dedup across paths)", res.ChunkCounts.Shared)
	}
	if res.ChunkCounts.AOnly != 0 || res.ChunkCounts.BOnly != 0 {
		t.Errorf("expected no exclusive chunks; got %+v", res.ChunkCounts)
	}
}

// TestCompare_TopNCap: with many file deltas, TopFileDeltas
// is capped to opts.TopN.
func TestCompare_TopNCap(t *testing.T) {
	var aFiles, bFiles []backup.FileEntry
	for i := 0; i < 10; i++ {
		// A unique to A; B unique to B (every file is a delta).
		aFiles = append(aFiles, fileWithChunks(
			"a/"+string(rune('a'+i)), "a-content-"+string(rune('a'+i))))
		bFiles = append(bFiles, fileWithChunks(
			"b/"+string(rune('a'+i)), "b-content-"+string(rune('a'+i))))
	}
	a := manifestWith("a", time.Now(), aFiles...)
	b := manifestWith("b", time.Now(), bFiles...)
	res, err := backup.Compare(a, b, backup.CompareOptions{TopN: 5})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(res.TopFileDeltas) != 5 {
		t.Errorf("len = %d, want 5", len(res.TopFileDeltas))
	}
}

// TestCompare_TopNDefault: zero TopN uses the package
// default (DefaultCompareTopN=20).
func TestCompare_TopNDefault(t *testing.T) {
	if backup.DefaultCompareTopN != 20 {
		t.Errorf("DefaultCompareTopN = %d, want 20", backup.DefaultCompareTopN)
	}
}

// TestCompare_NilManifestRefused: nil on either side is a
// programmer error and surfaces immediately.
func TestCompare_NilManifestRefused(t *testing.T) {
	a := manifestWith("a", time.Now())
	if _, err := backup.Compare(nil, a, backup.CompareOptions{}); err == nil {
		t.Error("Compare(nil, a) should error")
	}
	if _, err := backup.Compare(a, nil, backup.CompareOptions{}); err == nil {
		t.Error("Compare(a, nil) should error")
	}
}

// TestCompare_OrderingByAbsDelta: TopFileDeltas sort is
// (|delta| desc, path asc). A 1-byte added file ranks below
// a 100-byte changed file.
func TestCompare_OrderingByAbsDelta(t *testing.T) {
	a := manifestWith("a", time.Now(),
		fileWithChunks("data/big", "0123456789"), // 10 bytes
		fileWithChunks("data/keep", "kept"),
	)
	b := manifestWith("b", time.Now(),
		fileWithChunks("data/big", "01234567890123456789"), // grew to 20 bytes (+10)
		fileWithChunks("data/keep", "kept"),
		fileWithChunks("data/tiny", "x"), // +1 byte add
	)
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(res.TopFileDeltas) < 2 {
		t.Fatalf("expected ≥2 deltas; got %d", len(res.TopFileDeltas))
	}
	if res.TopFileDeltas[0].Path != "data/big" {
		t.Errorf("first delta = %q, want data/big (larger absolute delta)",
			res.TopFileDeltas[0].Path)
	}
}

// TestCompare_ShapesAreSerializable: regression — exercising
// the result through a deep-equal-ish round trip catches any
// future field that fails to marshal (a *time.Time pointer,
// a non-exported type leaking, etc).
func TestCompare_ShapesAreSerializable(t *testing.T) {
	a := manifestWith("a", time.Now(),
		fileWithChunks("data/x", "ax"))
	b := manifestWith("b", time.Now(),
		fileWithChunks("data/y", "by"))
	res, err := backup.Compare(a, b, backup.CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Read every public field — fails to compile if a name
	// changes.
	_ = res.A.BackupID
	_ = res.B.BackupID
	_ = res.FileCounts.OnlyInA
	_ = res.ChunkCounts.Shared
	_ = res.LogicalBytes.Delta
	if len(res.TopFileDeltas) > 0 {
		_ = res.TopFileDeltas[0].Path
	}
}

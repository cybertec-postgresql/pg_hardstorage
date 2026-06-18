package restore

// Tests for the chain-restore resume primitives added in audit
// v23 #8 — chain-staging path derivation, per-link completion
// markers, total-chunk-count helper.  These exercise the internal
// helpers directly rather than the full pg_combinebackup pipeline,
// because end-to-end chain restore requires the PG 17+ binary on
// PATH.

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

func TestChainStagingPath_OperatorPinnedHonoured(t *testing.T) {
	want := "/srv/pg_hardstorage/staging/db1"
	got, err := chainStagingPath(Options{
		ChainStagingRoot: want,
		Deployment:       "db1",
		BackupID:         "db1.full.20260501T0900Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("operator-pinned path mangled: got %q, want %q", got, want)
	}
}

func TestChainStagingPath_DefaultIsStable(t *testing.T) {
	a, err := chainStagingPath(Options{Deployment: "db1", BackupID: "db1.incremental.20260501T0900Z.aa"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := chainStagingPath(Options{Deployment: "db1", BackupID: "db1.incremental.20260501T0900Z.aa"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("default path should be stable across calls; got %q vs %q", a, b)
	}
	if !strings.Contains(a, "db1") || !strings.Contains(a, "db1.incremental.20260501T0900Z.aa") {
		t.Errorf("default path should embed deployment + backup_id; got %q", a)
	}
}

func TestChainStagingPath_DifferentBackupsDifferentPaths(t *testing.T) {
	a, _ := chainStagingPath(Options{Deployment: "db1", BackupID: "leaf-A"})
	b, _ := chainStagingPath(Options{Deployment: "db1", BackupID: "leaf-B"})
	if a == b {
		t.Fatalf("different backup IDs should derive different paths; got %q == %q", a, b)
	}
}

func TestChainStagingPath_RequiresDeploymentAndBackupID(t *testing.T) {
	if _, err := chainStagingPath(Options{Deployment: "db1"}); err == nil {
		t.Error("missing BackupID: expected error")
	}
	if _, err := chainStagingPath(Options{BackupID: "x"}); err == nil {
		t.Error("missing Deployment: expected error")
	}
}

func TestChainLinkMarker_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := chainLinkMarker{
		Schema:       chainLinkMarkerSchema,
		BackupID:     "db1.full.20260501T0900Z",
		ChunkCount:   42,
		BytesWritten: 12345,
	}
	if err := writeChainLinkMarker(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := readChainLinkMarker(dir)
	if !ok {
		t.Fatal("read: marker should be present")
	}
	if got != want {
		t.Fatalf("round-trip lost data: got %+v, want %+v", got, want)
	}
}

func TestChainLinkMarker_AbsentReturnsFalse(t *testing.T) {
	if _, ok := readChainLinkMarker(t.TempDir()); ok {
		t.Error("absent marker should return ok=false")
	}
}

func TestChainLinkMarker_MalformedJSONRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, chainLinkCompleteFilename), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readChainLinkMarker(dir); ok {
		t.Error("malformed JSON should return ok=false (forces re-materialise)")
	}
}

func TestChainLinkMarker_WrongSchemaRejected(t *testing.T) {
	dir := t.TempDir()
	body, _ := stdjson.Marshal(map[string]any{
		"schema":      "pg_hardstorage.restore.chain_link_marker.v999",
		"backup_id":   "x",
		"chunk_count": 1,
	})
	if err := os.WriteFile(filepath.Join(dir, chainLinkCompleteFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readChainLinkMarker(dir); ok {
		t.Error("schema mismatch should return ok=false")
	}
}

func TestTotalChunkCountForManifest(t *testing.T) {
	cases := []struct {
		name string
		m    *backup.Manifest
		want int
	}{
		{"nil", nil, 0},
		{"empty", &backup.Manifest{}, 0},
		{"one file three chunks", &backup.Manifest{
			Files: []backup.FileEntry{{
				Chunks: []backup.ChunkRef{{}, {}, {}},
			}},
		}, 3},
		{"two files mixed", &backup.Manifest{
			Files: []backup.FileEntry{
				{Chunks: []backup.ChunkRef{{}, {}}},
				{Chunks: []backup.ChunkRef{{}}},
			},
		}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := totalChunkCountForManifest(tc.m); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

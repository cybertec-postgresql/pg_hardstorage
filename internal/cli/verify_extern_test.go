package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// commitVerifiableBackup writes a real chunk into the CAS and commits
// a manifest that references it. The existing readWorld.commitManifest
// only writes the manifest (since list / show / status don't need chunk
// bytes); verify needs the round-trip path so we extend that here.
func commitVerifiableBackup(t *testing.T, w *readWorld, deployment string, idx int, body []byte) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("put chunk: %v", err)
	}
	// ts is relative to time.Now() so the SLO test (RPO target 24h)
	// stays "met" no matter when the suite runs. idx adds minutes
	// so callers that want N distinct backups get them.
	//
	// We round to the second so the formatted backup ID stays
	// stable for the duration of the test (sub-second drift would
	// produce two different IDs for the same call site, which
	// tests expect to be deterministic across re-runs in the same
	// process).
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(time.Duration(idx) * time.Minute)
	id := deployment + ".verify." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{
				Path: "data/file", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{
					{Hash: info.Hash, Offset: 0, Len: int64(len(body))},
				},
			},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func TestVerify_RequiresRepoFlag(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "verify", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", errb)
	}
}

func TestVerify_NoBackups_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "verify", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.backup") {
		t.Errorf("expected notfound.backup: %s", errb)
	}
}

func TestVerify_HappyPath_ReportsChunksVerified(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("hello world from verify test"))

	stdout, _, exit := runCLI(t, "verify", "db1", id, "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, want 0\nstdout: %s", exit, stdout)
	}
	var view struct {
		Deployment        string   `json:"deployment"`
		BackupID          string   `json:"backup_id"`
		ManifestSignature string   `json:"manifest_signature"`
		ChunksReferenced  int      `json:"chunks_referenced"`
		ChunksUnique      int      `json:"chunks_unique"`
		ChunksVerified    int      `json:"chunks_verified"`
		ChunksMismatched  int      `json:"chunks_mismatched"`
		BytesVerified     int64    `json:"bytes_verified"`
		Mismatches        []string `json:"mismatches"`
	}
	bodyOf(t, stdout, &view)
	if view.Deployment != "db1" || view.BackupID != id {
		t.Errorf("identity mismatch: got %s/%s", view.Deployment, view.BackupID)
	}
	if view.ManifestSignature != "valid" {
		t.Errorf("manifest_signature = %q, want \"valid\"", view.ManifestSignature)
	}
	if view.ChunksUnique != 1 || view.ChunksVerified != 1 {
		t.Errorf("counts off: unique=%d verified=%d (want 1/1)",
			view.ChunksUnique, view.ChunksVerified)
	}
	if view.ChunksMismatched != 0 {
		t.Errorf("ChunksMismatched = %d", view.ChunksMismatched)
	}
	if view.BytesVerified == 0 {
		t.Errorf("BytesVerified should be non-zero")
	}
}

func TestVerify_Latest_ResolvesNewest(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("first"))
	expectLatest := commitVerifiableBackup(t, w, "db1", 5, []byte("second-and-newer"))

	stdout, _, exit := runCLI(t, "verify", "db1", "latest", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		BackupID string `json:"backup_id"`
	}
	bodyOf(t, stdout, &view)
	if view.BackupID != expectLatest {
		t.Errorf("latest = %q, want %q", view.BackupID, expectLatest)
	}
}

func TestVerify_Default_ResolvesLatest(t *testing.T) {
	// Omitting the backup-id argument should be equivalent to "latest".
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("first"))
	expectLatest := commitVerifiableBackup(t, w, "db1", 7, []byte("newer"))

	stdout, _, exit := runCLI(t, "verify", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		BackupID string `json:"backup_id"`
	}
	bodyOf(t, stdout, &view)
	if view.BackupID != expectLatest {
		t.Errorf("default → latest = %q, want %q", view.BackupID, expectLatest)
	}
}

func TestVerify_MissingChunk_ReportsVerifyFailed(t *testing.T) {
	// Commit a backup, delete the chunk out from under it. verify
	// must report the mismatch and exit ExitVerifyFailed (9).
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("doomed-chunk"))

	cas := casdefault.New(w.sp)
	// We need the hash to delete it; the helper doesn't return it,
	// so derive from the body the helper used.
	hash := repo.HashOf([]byte("doomed-chunk"))
	if err := cas.DeleteChunk(context.Background(), hash); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	_, errb, exit := runCLI(t, "verify", "db1", id, "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed(%d)\nstderr: %s",
			exit, output.ExitVerifyFailed, errb)
	}
	if !strings.Contains(errb, "verify.chunk_mismatch") {
		t.Errorf("expected verify.chunk_mismatch in stderr:\n%s", errb)
	}
}

func TestVerify_Sample_LimitsWork(t *testing.T) {
	// --sample 1 should attempt only 1 chunk even when the manifest
	// references several. The body's chunks_sampled reflects the cap.
	w := newReadWorld(t)
	cas := casdefault.New(w.sp)
	bodies := [][]byte{[]byte("alpha"), []byte("bravo"), []byte("charlie")}
	chunks := make([]backup.ChunkRef, 0, len(bodies))
	var totalSize int64
	for _, b := range bodies {
		info, err := cas.PutChunk(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, backup.ChunkRef{
			Hash:   info.Hash,
			Offset: totalSize, // contiguous offsets — Validate requires this
			Len:    int64(len(b)),
		})
		totalSize += int64(len(b))
	}
	ts := time.Date(2026, 4, 28, 15, 0, 0, 0, time.UTC)
	id := "db1.sample." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "data/multi", Size: totalSize, Mode: 0o600, Chunks: chunks},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	stdout, _, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "--sample", "1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		ChunksUnique   int `json:"chunks_unique"`
		ChunksSampled  int `json:"chunks_sampled"`
		ChunksVerified int `json:"chunks_verified"`
	}
	bodyOf(t, stdout, &view)
	if view.ChunksUnique != 3 {
		t.Errorf("ChunksUnique = %d, want 3", view.ChunksUnique)
	}
	if view.ChunksSampled != 1 {
		t.Errorf("ChunksSampled = %d, want 1 (--sample limits work)", view.ChunksSampled)
	}
	if view.ChunksVerified != 1 {
		t.Errorf("ChunksVerified = %d, want 1", view.ChunksVerified)
	}
}

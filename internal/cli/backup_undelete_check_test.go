package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestBackupUndelete_CheckChunks_HappyPath: --check-chunks
// against a tombstoned manifest whose chunks are all present
// undeletes successfully + emits chunk_checks in the body.
func TestBackupUndelete_CheckChunks_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("present-chunks"))
	if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "backup", "undelete", "db1", id,
		"--repo", w.repoURL, "--check-chunks", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"chunk_checks"`,
		`"present": 1`,
		`"missing": 0`,
		`"restored": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupUndelete_CheckChunks_RefusesMissingChunks:
// --check-chunks against a tombstoned manifest whose chunks
// have been GC'd surfaces conflict.chunks_missing AND does
// NOT remove the tombstone (atomic refuse).
func TestBackupUndelete_CheckChunks_RefusesMissingChunks(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("about-to-be-gced"))
	if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
		t.Fatal(err)
	}
	// Find the chunk and delete it via the storage plugin
	// (simulate chunk-GC).
	m, _, err := w.store.ReadIncludingTombstoned(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) == 0 || len(m.Files[0].Chunks) == 0 {
		t.Fatal("unexpectedly empty manifest")
	}
	chunkHash := m.Files[0].Chunks[0].Hash
	if err := w.sp.Delete(context.Background(), repo.ChunkKey(chunkHash)); err != nil {
		t.Fatal(err)
	}

	_, stderr, exit := runCLI(t, "backup", "undelete", "db1", id,
		"--repo", w.repoURL, "--check-chunks", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("expected refusal; got OK\nstderr=%s", stderr)
	}
	for _, want := range []string{
		"conflict.chunks_missing",
		id,
		"--skip-missing",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	// Tombstone NOT removed (atomic-batch refusal).
	dead, derr := w.store.IsTombstoned(context.Background(), "db1", id)
	if derr != nil {
		t.Fatal(derr)
	}
	if !dead {
		t.Errorf("manifest should still be tombstoned after refused undelete")
	}
}

// TestBackupUndelete_CheckChunks_SkipMissing_PartialBatch:
// --skip-missing turns the batch refusal into a partial
// success — manifests with intact chunks undelete; the ones
// with missing chunks stay tombstoned and surface in
// outcomes[i].chunks_missing=true.
func TestBackupUndelete_CheckChunks_SkipMissing_PartialBatch(t *testing.T) {
	w := newReadWorld(t)
	idGood := commitVerifiableBackup(t, w, "db1", 0, []byte("good-chunks"))
	idBad := commitVerifiableBackup(t, w, "db1", 1, []byte("bad-chunks"))
	for _, id := range []string{idGood, idBad} {
		if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
			t.Fatal(err)
		}
	}
	// Delete the bad-side chunk.
	mBad, _, err := w.store.ReadIncludingTombstoned(context.Background(), "db1", idBad, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.sp.Delete(context.Background(), repo.ChunkKey(mBad.Files[0].Chunks[0].Hash)); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "backup", "undelete", "db1", idGood, idBad,
		"--repo", w.repoURL, "--check-chunks", "--skip-missing", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("--skip-missing should succeed partially; exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		idGood,
		idBad,
		`"chunks_missing": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
	// Good is live; bad still tombstoned.
	deadGood, _ := w.store.IsTombstoned(context.Background(), "db1", idGood)
	deadBad, _ := w.store.IsTombstoned(context.Background(), "db1", idBad)
	if deadGood {
		t.Errorf("idGood should be live after partial undelete")
	}
	if !deadBad {
		t.Errorf("idBad should remain tombstoned (chunks missing)")
	}
}

// TestBackupUndelete_SkipMissing_RequiresCheckChunks:
// --skip-missing without --check-chunks is a usage error
// because there's nothing to skip without the pre-flight.
func TestBackupUndelete_SkipMissing_RequiresCheckChunks(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("dummy"))
	_, stderr, exit := runCLI(t, "backup", "undelete", "db1", id,
		"--repo", w.repoURL, "--skip-missing", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse; got %d", exit)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", stderr)
	}
}

// TestBackupUndelete_DefaultBodyShape_NoChunkChecks:
// regression — without --check-chunks the chunk_checks key is
// omitted (omitempty). Default-mode body byte-identical to
// pre-change v0.6+ (24-month JSON-compat).
func TestBackupUndelete_DefaultBodyShape_NoChunkChecks(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("dummy"))
	if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := runCLI(t, "backup", "undelete", "db1", id,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	if strings.Contains(stdout, `"chunk_checks"`) {
		t.Errorf("default mode should not include chunk_checks key:\n%s", stdout)
	}
	if strings.Contains(stdout, `"chunks_missing"`) {
		t.Errorf("default mode should not include chunks_missing:\n%s", stdout)
	}
}

// TestBackupUndelete_CheckChunks_Discoverable: --help
// advertises both new flags + the use case.
func TestBackupUndelete_CheckChunks_Discoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "backup", "undelete", "--help")
	for _, want := range []string{
		"--check-chunks",
		"--skip-missing",
		"verify --existence-only",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("undelete --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupUndelete_DefaultRefusesMissingChunks pins the safe-by-
// default behavior (data-loss path #2): a PLAIN `backup undelete`
// (no --check-chunks) of a backup whose chunks were swept must
// refuse with conflict.chunks_missing and leave the tombstone in
// place — the operator never gets a healthy-looking but
// un-restorable backup, even without opting into the check.
func TestBackupUndelete_DefaultRefusesMissingChunks(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("about-to-be-gced"))
	if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
		t.Fatal(err)
	}
	// Simulate chunk-GC: delete the manifest's only chunk.
	m, _, err := w.store.ReadIncludingTombstoned(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.sp.Delete(context.Background(), repo.ChunkKey(m.Files[0].Chunks[0].Hash)); err != nil {
		t.Fatal(err)
	}

	// NOTE: no --check-chunks flag — the core guard must still fire.
	_, stderr, exit := runCLI(t, "backup", "undelete", "db1", id,
		"--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("plain undelete should refuse missing chunks; got OK\nstderr=%s", stderr)
	}
	for _, want := range []string{"conflict.chunks_missing", "--force"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	dead, derr := w.store.IsTombstoned(context.Background(), "db1", id)
	if derr != nil {
		t.Fatal(derr)
	}
	if !dead {
		t.Error("manifest should still be tombstoned after a refused plain undelete")
	}
}

// TestBackupUndelete_ForceBypassesMissingChunks: --force resurrects
// the metadata even though the chunks are gone (forensic recovery).
func TestBackupUndelete_ForceBypassesMissingChunks(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("gone"))
	if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
		t.Fatal(err)
	}
	m, _, err := w.store.ReadIncludingTombstoned(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.sp.Delete(context.Background(), repo.ChunkKey(m.Files[0].Chunks[0].Hash)); err != nil {
		t.Fatal(err)
	}

	_, stderr, exit := runCLI(t, "backup", "undelete", "db1", id,
		"--repo", w.repoURL, "--force", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("--force undelete should succeed despite missing chunks; exit=%d\nstderr=%s", exit, stderr)
	}
	dead, derr := w.store.IsTombstoned(context.Background(), "db1", id)
	if derr != nil {
		t.Fatal(derr)
	}
	if dead {
		t.Error("manifest should be live after --force undelete")
	}
}

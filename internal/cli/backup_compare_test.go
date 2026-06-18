package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitBackupWithFiles is a small helper: commit a manifest
// with the supplied path → content map. Each content string
// becomes a single chunk via repo.HashOf. Used to build
// fixtures for the compare CLI tests with predictable file
// + chunk shapes.
func commitBackupWithFiles(t *testing.T, w *readWorld, deployment, backupID string, when time.Time, files map[string]string) {
	t.Helper()
	entries := make([]backup.FileEntry, 0, len(files))
	for path, content := range files {
		ln := int64(len(content))
		entries = append(entries, backup.FileEntry{
			Path: path,
			Size: ln,
			Mode: 0o600,
			Chunks: []backup.ChunkRef{
				{Hash: repo.HashOf([]byte(content)), Offset: 0, Len: ln},
			},
		})
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        when,
		StoppedAt:        when.Add(time.Minute),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            entries,
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", backupID, err)
	}
}

// TestBackupCompare_HappyPath_JSONShape: end-to-end CLI happy
// path. Two backups with overlapping + distinct files. The
// result body has per-side summary, file-class counts,
// chunk-class counts, top-N deltas.
func TestBackupCompare_HappyPath_JSONShape(t *testing.T) {
	w := newReadWorld(t)
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	commitBackupWithFiles(t, w, "db1", "db1.full.A", when,
		map[string]string{
			"data/keep":    "shared-bytes",
			"data/dropped": "removed-bytes",
		})
	commitBackupWithFiles(t, w, "db1", "db1.full.B", when.Add(time.Hour),
		map[string]string{
			"data/keep":  "shared-bytes",
			"data/added": "new-bytes",
		})

	stdout, _, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.A", "db1.full.B",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("compare exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"backup_id": "db1.full.A"`,
		`"backup_id": "db1.full.B"`,
		`"only_in_a": 1`,
		`"only_in_b": 1`,
		`"in_both": 1`,
		`"shared": 1`, // shared chunk count
		`"a_only": 1`,
		`"b_only": 1`,
		`"top_file_deltas"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupCompare_IdenticalManifests_NoDeltas: two
// identical manifests show zero file deltas, zero exclusive
// chunks. Validates the no-change path.
func TestBackupCompare_IdenticalManifests_NoDeltas(t *testing.T) {
	w := newReadWorld(t)
	files := map[string]string{
		"data/foo": "foo-bytes",
		"data/bar": "bar-bytes",
	}
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	commitBackupWithFiles(t, w, "db1", "db1.full.A", when, files)
	commitBackupWithFiles(t, w, "db1", "db1.full.B", when.Add(time.Hour), files)

	stdout, _, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.A", "db1.full.B",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"only_in_a": 0`) ||
		!strings.Contains(stdout, `"only_in_b": 0`) {
		t.Errorf("expected zero exclusive files for identical manifests:\n%s", stdout)
	}
	// top_file_deltas should be omitempty (omitted) when no
	// deltas exist.
	if strings.Contains(stdout, `"top_file_deltas"`) {
		t.Errorf("identical manifests should NOT include top_file_deltas:\n%s", stdout)
	}
}

// TestBackupCompare_NotFound_FirstSide: structured
// notfound.backup tagged with the offending side.
func TestBackupCompare_NotFound_FirstSide(t *testing.T) {
	w := newReadWorld(t)
	commitBackupWithFiles(t, w, "db1", "db1.full.real",
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		map[string]string{"data/x": "ax"})

	_, stderr, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.missing", "db1.full.real",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit=%d, want ExitNotFound", exit)
	}
	for _, want := range []string{
		"notfound.backup",
		"A side",
		"db1.full.missing",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestBackupCompare_TombstonedSide_HelpfulRefusal: compare
// against a tombstoned manifest surfaces
// notfound.backup_tombstoned with a Suggestion pointing at
// `backup show --include-deleted` / `backup undelete`.
func TestBackupCompare_TombstonedSide_HelpfulRefusal(t *testing.T) {
	w := newReadWorld(t)
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	commitBackupWithFiles(t, w, "db1", "db1.full.A", when,
		map[string]string{"data/x": "ax"})
	commitBackupWithFiles(t, w, "db1", "db1.full.B", when.Add(time.Hour),
		map[string]string{"data/x": "ax"})
	// Tombstone B.
	if err := w.store.SoftDelete(context.Background(), "db1", "db1.full.B", "manual", "test"); err != nil {
		t.Fatal(err)
	}

	_, stderr, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.A", "db1.full.B",
		"--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("expected refusal on tombstoned side; stderr=%s", stderr)
	}
	for _, want := range []string{
		"notfound.backup_tombstoned",
		"B side",
		"backup undelete",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestBackupCompare_SameID_RefusedAtUsage: passing the same
// id for both sides is a usage error caught up-front.
func TestBackupCompare_SameID_RefusedAtUsage(t *testing.T) {
	w := newReadWorld(t)
	commitBackupWithFiles(t, w, "db1", "db1.full.A",
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		map[string]string{"data/x": "ax"})

	_, stderr, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.A", "db1.full.A",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.same_id") {
		t.Errorf("expected usage.same_id:\n%s", stderr)
	}
}

// TestBackupCompare_TopNFlag: --top-n caps the deltas list.
func TestBackupCompare_TopNFlag(t *testing.T) {
	w := newReadWorld(t)
	// Build A and B with 5 disjoint files each — 10 file
	// deltas total.
	aFiles := map[string]string{}
	bFiles := map[string]string{}
	for i := 0; i < 5; i++ {
		aFiles["a/"+string(rune('a'+i))] = "a-content-" + string(rune('a'+i))
		bFiles["b/"+string(rune('a'+i))] = "b-content-" + string(rune('a'+i))
	}
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	commitBackupWithFiles(t, w, "db1", "db1.full.A", when, aFiles)
	commitBackupWithFiles(t, w, "db1", "db1.full.B", when.Add(time.Hour), bFiles)

	stdout, _, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.A", "db1.full.B",
		"--repo", w.repoURL, "--top-n", "3", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	// 3 entries in top_file_deltas — we count by counting
	// `"path"` occurrences inside the deltas array. A simpler
	// proxy: check the count by class entries.
	classCount := strings.Count(stdout, `"class":`)
	if classCount != 3 {
		t.Errorf("expected 3 top deltas with --top-n 3; got %d in:\n%s", classCount, stdout)
	}
}

// TestBackupCompare_TextRendering: text mode renders the
// header, per-side table, file/chunk/logical-bytes blocks,
// and the top-deltas table.
func TestBackupCompare_TextRendering(t *testing.T) {
	w := newReadWorld(t)
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	commitBackupWithFiles(t, w, "db1", "db1.full.A", when,
		map[string]string{"data/x": "ax", "data/dropped": "removed"})
	commitBackupWithFiles(t, w, "db1", "db1.full.B", when.Add(time.Hour),
		map[string]string{"data/x": "ax", "data/added": "new"})

	stdout, _, exit := runCLI(t, "backup", "compare",
		"db1", "db1.full.A", "db1.full.B",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"compare db1/db1.full.A ↔ db1.full.B",
		"FILES",
		"in_both:",
		"only_in_a:",
		"only_in_b:",
		"CHUNKS (CAS-deduped)",
		"shared:",
		"LOGICAL BYTES",
		"TOP",
		"FILE DELTA",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// TestBackupCompare_RequiresRepo: structured usage error for
// missing --repo.
func TestBackupCompare_RequiresRepo(t *testing.T) {
	_, stderr, exit := runCLI(t, "backup", "compare",
		"db1", "a", "b", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", stderr)
	}
}

// TestBackupCompare_DiscoverableFromHelp: subcommand listed
// in `backup --help`; own help advertises --top-n and use
// cases.
func TestBackupCompare_DiscoverableFromHelp(t *testing.T) {
	stdout, _, _ := runCLI(t, "backup", "--help")
	if !strings.Contains(stdout, "compare") {
		t.Errorf("backup --help missing compare:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "backup", "compare", "--help")
	for _, want := range []string{"--top-n", "metadata-only", "Incremental forensics"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backup compare --help missing %q:\n%s", want, stdout)
		}
	}
}

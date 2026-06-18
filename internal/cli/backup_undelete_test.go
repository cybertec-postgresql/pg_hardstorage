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

// commitFullBackup commits a single full backup with the given ID
// for tests that need a live target before tombstoning.
func commitFullBackup(t *testing.T, w *readWorld, deployment, id string, when time.Time) {
	t.Helper()
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
		StartedAt:        when,
		StoppedAt:        when.Add(time.Minute),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: 3, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
		},
	}
	// Store the referenced chunk so the backup is genuinely
	// restorable — `backup undelete`'s restorability pre-flight
	// Stats every chunk before resurrecting a tombstone.
	if _, err := repo.NewCAS(w.sp).PutChunk(context.Background(), []byte("17\n")); err != nil {
		t.Fatalf("seed chunk for %s: %v", id, err)
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
}

// TestBackupUndelete_RestoresAfterDelete: SoftDelete via CLI,
// undelete via CLI, confirm the manifest is live again.
func TestBackupUndelete_RestoresAfterDelete(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))

	// Delete first.
	if _, _, exit := runCLI(t,
		"backup", "delete", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"--reason", "test",
	); exit != int(output.ExitOK) {
		t.Fatalf("backup delete exit=%d", exit)
	}

	// Confirm tombstoned.
	dead, err := w.store.IsTombstoned(context.Background(), "db1", "db1.full.A")
	if err != nil {
		t.Fatal(err)
	}
	if !dead {
		t.Fatal("expected tombstoned post-delete")
	}

	// Undelete.
	stdout, _, exit := runCLI(t,
		"backup", "undelete", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"--reason", "operator-error",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("backup undelete exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"db1.full.A"`,
		`"restored": true`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in undelete output:\n%s", want, stdout)
		}
	}

	// Confirm live again.
	dead, err = w.store.IsTombstoned(context.Background(), "db1", "db1.full.A")
	if err != nil {
		t.Fatal(err)
	}
	if dead {
		t.Errorf("manifest should be live after undelete")
	}
}

// TestBackupUndelete_AlreadyLive_NoOp: undelete a manifest that
// was never tombstoned. CLI returns ExitOK with restored=false.
func TestBackupUndelete_AlreadyLive_NoOp(t *testing.T) {
	w := newReadWorld(t)
	commitFullBackup(t, w, "db1", "db1.full.A", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))

	stdout, _, exit := runCLI(t,
		"backup", "undelete", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("undelete on live manifest: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"restored": false`) {
		t.Errorf("expected restored=false in JSON for already-live undelete:\n%s", stdout)
	}
}

// TestBackupUndelete_MultipleIDs_UnwindsCascade: cascade-delete
// a chain (A→B→C), then undelete with all three IDs in one call.
// Validates the operational pattern: the cascade response gives
// the operator the deletion-order list; passing it back to
// undelete restores everything.
func TestBackupUndelete_MultipleIDs_UnwindsCascade(t *testing.T) {
	w := newReadWorld(t)
	base := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	commitFullBackup(t, w, "db1", "db1.full.A", base)
	// Build a 2-link descendant chain on top.
	for i, id := range []string{"db1.inc.B", "db1.inc.C"} {
		parent := "db1.full.A"
		if i > 0 {
			parent = "db1.inc.B"
		}
		m := &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         id,
			Deployment:       "db1",
			Tenant:           "default",
			Type:             backup.BackupTypeIncremental,
			ParentBackupID:   parent,
			PGVersion:        17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000200",
			StopLSN:          "0/3000300",
			Timeline:         1,
			StartedAt:        base.Add(time.Hour * time.Duration(i+1)),
			StoppedAt:        base.Add(time.Hour*time.Duration(i+1) + time.Minute),
			BackupLabel:      "START WAL LOCATION: 0/3000200\n",
			Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files: []backup.FileEntry{
				{Path: "PG_VERSION", Size: 3, Mode: 0o600,
					Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
			},
		}
		if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	// Cascade delete.
	if _, _, exit := runCLI(t,
		"backup", "delete", "db1", "db1.full.A",
		"--repo", w.repoURL,
		"--cascade",
	); exit != int(output.ExitOK) {
		t.Fatalf("cascade delete exit=%d", exit)
	}
	// Undelete all three in one CLI call.
	stdout, _, exit := runCLI(t,
		"backup", "undelete", "db1",
		"db1.full.A", "db1.inc.B", "db1.inc.C",
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("undelete multi exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"db1.full.A"`, `"db1.inc.B"`, `"db1.inc.C"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in undelete output:\n%s", want, stdout)
		}
	}

	// All three live.
	for _, id := range []string{"db1.full.A", "db1.inc.B", "db1.inc.C"} {
		dead, err := w.store.IsTombstoned(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if dead {
			t.Errorf("%s should be live after undelete", id)
		}
	}
}

// TestBackupUndelete_RequiresFlags: structured-error guards for
// missing flags / args.
func TestBackupUndelete_RequiresFlags(t *testing.T) {
	// Missing --repo.
	_, stderr, exit := runCLI(t,
		"backup", "undelete", "db1", "id",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo: exit=%d, want %d\nstderr=%s", exit, output.ExitMisuse, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; stderr=%s", stderr)
	}

	// Cobra-level: too few args (need at least deployment + 1 id).
	_, _, exit = runCLI(t,
		"backup", "undelete", "db1",
		"--repo", "file:///tmp/nope",
	)
	if exit == int(output.ExitOK) {
		t.Errorf("undelete with no IDs should not succeed; exit=%d", exit)
	}
}

// TestBackupUndelete_FlagDiscoverable: `backup undelete --help`
// (and the parent's command list) advertises the subcommand and
// its purpose so an operator unwinding a wrong cascade finds it.
func TestBackupUndelete_FlagDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "backup", "undelete", "--help")
	for _, want := range []string{
		"undelete",
		"--repo",
		"--reason",
		"tombstone",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backup undelete --help missing %q:\n%s", want, stdout)
		}
	}

	// Parent listing.
	stdout, _, _ = runCLI(t, "backup", "--help")
	if !strings.Contains(stdout, "undelete") {
		t.Errorf("backup --help should list undelete subcommand:\n%s", stdout)
	}
}

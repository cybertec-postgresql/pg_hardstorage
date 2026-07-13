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

// commitChainCLI commits a 3-link chain (full → inc1 → inc2)
// for the hold-protection CLI tests. Mirror of the cascade
// fixture in backup_delete_test.go.
func commitChainCLI(t *testing.T, w *readWorld, deployment string) (rootID, midID, leafID string) {
	t.Helper()
	rootID = deployment + ".full.A"
	midID = deployment + ".inc.B"
	leafID = deployment + ".inc.C"
	for i, spec := range []struct {
		id, parent string
		t          backup.BackupType
		when       time.Time
	}{
		{rootID, "", backup.BackupTypeFull, time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)},
		{midID, rootID, backup.BackupTypeIncremental, time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)},
		{leafID, midID, backup.BackupTypeIncremental, time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)},
	} {
		_ = i
		m := &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         spec.id,
			Deployment:       deployment,
			Tenant:           "default",
			Type:             spec.t,
			ParentBackupID:   spec.parent,
			PGVersion:        17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028",
			StopLSN:          "0/30001A0",
			Timeline:         1,
			StartedAt:        spec.when,
			StoppedAt:        spec.when.Add(time.Minute),
			BackupLabel:      "START WAL LOCATION: 0/3000028\n",
			Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files: []backup.FileEntry{
				{Path: "PG_VERSION", Size: 3, Mode: 0o600,
					Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
			},
		}
		if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", spec.id, err)
		}
	}
	return rootID, midID, leafID
}

// TestBackupDelete_RefusesHeldManifest: `backup delete` against
// a manifest with a hold returns the structured
// `conflict.manifest_held` error and a copy-pasteable
// `backup hold remove` Suggestion.
func TestBackupDelete_RefusesHeldManifest(t *testing.T) {
	w := newReadWorld(t)
	rootID, _, _ := commitChainCLI(t, w, "db1")
	if err := w.store.PutHold(context.Background(), "db1", rootID,
		"ops@acme.com", "GDPR-art-17-#9999"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runCLI(t,
		"backup", "delete", "db1", rootID, "--yes",
		"--repo", w.repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitConflict) && exit != int(output.ExitError) {
		t.Fatalf("expected non-zero exit; got %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	for _, want := range []string{
		"conflict.manifest_held",
		"ops@acme.com",
		"GDPR-art-17-#9999",
		"backup hold remove",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	// Manifest is NOT tombstoned.
	if dead, _ := w.store.IsTombstoned(context.Background(), "db1", rootID); dead {
		t.Errorf("held manifest should not be tombstoned after refused delete")
	}
}

// TestBackupDelete_HoldReleased_AllowsDelete: clear the hold
// and `backup delete` succeeds. Round-trip through the CLI.
func TestBackupDelete_HoldReleased_AllowsDelete(t *testing.T) {
	w := newReadWorld(t)
	rootID, _, _ := commitChainCLI(t, w, "db1")
	// Place hold, attempt delete (refused), release hold,
	// retry — the second attempt should win.
	if err := w.store.PutHold(context.Background(), "db1", rootID, "ops", "test"); err != nil {
		t.Fatal(err)
	}
	// Need to drain descendants first since they exist; cascade
	// would refuse on hold; let's hold only the leaf.
	_, _, leafID := commitChainCLI(t, w, "db2")
	if err := w.store.PutHold(context.Background(), "db2", leafID, "ops", "test"); err != nil {
		t.Fatal(err)
	}
	// Refused first.
	_, _, exit := runCLI(t,
		"backup", "delete", "db2", leafID, "--yes",
		"--repo", w.repoURL,
	)
	if exit == int(output.ExitOK) {
		t.Fatal("expected refusal while held")
	}
	// Release.
	if err := w.store.RemoveHold(context.Background(), "db2", leafID); err != nil {
		t.Fatal(err)
	}
	// Now succeeds.
	_, _, exit = runCLI(t,
		"backup", "delete", "db2", leafID, "--yes",
		"--repo", w.repoURL,
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("expected delete to succeed after RemoveHold; exit=%d", exit)
	}
	if dead, _ := w.store.IsTombstoned(context.Background(), "db2", leafID); !dead {
		t.Errorf("expected leaf tombstoned after hold release + delete")
	}
}

// TestBackupDelete_CascadeRefusesIfAnyLinkHeld: cascade aborts
// up-front when any link is held, with the structured
// `conflict.chain_has_held_links` error listing every held ID.
// No manifest is tombstoned (refusal is up-front, not partial).
func TestBackupDelete_CascadeRefusesIfAnyLinkHeld(t *testing.T) {
	w := newReadWorld(t)
	rootID, midID, leafID := commitChainCLI(t, w, "db1")
	// Hold the middle link.
	if err := w.store.PutHold(context.Background(), "db1", midID,
		"compliance", "litigation-#42"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runCLI(t,
		"backup", "delete", "db1", rootID, "--yes",
		"--repo", w.repoURL,
		"--cascade",
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("expected cascade refusal; got OK\nstdout=%s", stdout)
	}
	for _, want := range []string{
		"conflict.chain_has_held_links",
		midID,
		"backup hold remove",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	// No tombstones — refusal is up-front.
	for _, id := range []string{rootID, midID, leafID} {
		if dead, _ := w.store.IsTombstoned(context.Background(), "db1", id); dead {
			t.Errorf("%s should NOT be tombstoned after refused cascade", id)
		}
	}
}

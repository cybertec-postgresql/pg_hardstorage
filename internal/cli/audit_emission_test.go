package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestHoldAdd_EmitsAudit pins observability audit #3: placing a legal hold
// writes an audit record (previously only the automatic purge-expired path
// did, leaving operator hold placements untraceable).
func TestHoldAdd_EmitsAudit(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))

	if _, _, exit := runCLI(t, "hold", "add", "db1", id,
		"--repo", w.repoURL,
		"--holder", "ops@acme",
		"--reason", "litigation-X-100",
		"-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("hold add exit=%d", exit)
	}

	out, _, exit := runCLI(t, "audit", "search",
		"--repo", w.repoURL, "--action", "hold.add", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search exit=%d", exit)
	}
	for _, want := range []string{`"count": 1`, `"action": "hold.add"`, id, "ops@acme"} {
		if !strings.Contains(out, want) {
			t.Errorf("hold.add audit missing %q:\n%s", want, out)
		}
	}
}

// TestHoldRemove_EmitsAudit: releasing a hold writes an audit record (the
// command's help already promised it was "auditable"); the record carries
// the holder of the hold that was released.
func TestHoldRemove_EmitsAudit(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))
	if err := w.store.PutHold(context.Background(), "db1", id, "compliance@acme", "GDPR-7"); err != nil {
		t.Fatal(err)
	}

	if _, _, exit := runCLI(t, "hold", "remove", "db1", id,
		"--repo", w.repoURL, "--yes", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("hold remove exit=%d", exit)
	}

	out, _, exit := runCLI(t, "audit", "search",
		"--repo", w.repoURL, "--action", "hold.remove", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search exit=%d", exit)
	}
	for _, want := range []string{`"count": 1`, `"action": "hold.remove"`, id, "compliance@acme"} {
		if !strings.Contains(out, want) {
			t.Errorf("hold.remove audit missing %q:\n%s", want, out)
		}
	}
}

// TestRotateApply_EmitsAuditPerDelete pins observability audit #4: a
// retention rotation writes one audit record per soft-deleted backup
// (previously zero, unlike `backup delete`).
func TestRotateApply_EmitsAuditPerDelete(t *testing.T) {
	repoURL := initRepoForTest(t)

	// Seed 3 daily backups; --keep-daily 1 deletes 2.
	now := time.Now().UTC()
	stamps := []time.Time{
		now.Add(-1 * 24 * time.Hour),
		now.Add(-2 * 24 * time.Hour),
		now.Add(-3 * 24 * time.Hour),
	}
	{
		_, sp, err := repo.Open(context.Background(), repoURL)
		if err != nil {
			t.Fatal(err)
		}
		store := backup.NewManifestStore(sp)
		p, err := paths.Resolve(paths.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		signer, _, err := keystore.LoadOrGenerate(p.Keyring.Value)
		if err != nil {
			t.Fatal(err)
		}
		for _, ts := range stamps {
			id := "db1.full." + ts.UTC().Format("20060102T150405Z")
			m := &backup.Manifest{
				Schema:           backup.Schema,
				BackupID:         id,
				Deployment:       "db1",
				Type:             backup.BackupTypeFull,
				PGVersion:        170,
				SystemIdentifier: "7388123",
				StartLSN:         "0/0",
				StopLSN:          "0/0",
				Timeline:         1,
				StartedAt:        ts.Add(-time.Minute),
				StoppedAt:        ts,
				BackupLabel:      "START WAL LOCATION: 0/0\n",
				Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
				Files:            []backup.FileEntry{},
			}
			if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		sp.Close()
	}

	out, _, exit := runCmd(t,
		"rotate", "db1", "--repo", repoURL, "--policy", "gfs",
		"--keep-daily", "1", "--keep-weekly", "0", "--keep-monthly", "0", "--keep-yearly", "0",
		"--apply", "--output", "json")
	if exit != 0 {
		t.Fatalf("rotate --apply exit=%d\n%s", exit, out)
	}
	if !strings.Contains(out, `"applied": 2`) {
		t.Fatalf("expected applied:2; got:\n%s", out)
	}

	auditOut, _, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--action", "backup.rotate_delete", "--output", "json")
	if exit != 0 {
		t.Fatalf("audit search exit=%d", exit)
	}
	for _, want := range []string{`"count": 2`, `"action": "backup.rotate_delete"`, `db1.full.`} {
		if !strings.Contains(auditOut, want) {
			t.Errorf("rotate audit missing %q:\n%s", want, auditOut)
		}
	}
}

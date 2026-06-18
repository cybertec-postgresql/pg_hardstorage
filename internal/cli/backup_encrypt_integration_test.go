// CLI-level integration test for the encryption end-to-end story:
//   pg_hardstorage init --encrypt → KEK file written
//   pg_hardstorage backup db1     → manifest carries EncryptionInfo
//   pg_hardstorage restore        → decrypts via the resolver
//
//go:build integration

package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestIntegration_BackupEncrypted_CLIRoundTrip(t *testing.T) {
	srv := testkit.StartPostgres(t)

	cfgDir := t.TempDir()
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	repoURL := "file://" + t.TempDir()

	// Init with default encryption (--encrypt is true by default).
	out, stderr, exit := runCmd(t,
		"init", "--yes",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--deployment", "db1",
		"--skip-backup",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("init exit = %d\nstdout: %s\nstderr: %s", exit, out, stderr)
	}
	if !strings.Contains(out, `"encryption_enabled": true`) {
		t.Errorf("init result should report encryption_enabled=true:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(keyringDir, keystore.KEKFileName)); err != nil {
		t.Fatalf("KEK file missing: %v", err)
	}

	// Take a backup. Should auto-encrypt because the KEK is present.
	out, stderr, exit = runCmd(t,
		"backup", "db1",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--fast",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("backup exit = %d\nstdout: %s\nstderr: %s", exit, out, stderr)
	}
	if !strings.Contains(out, `"encrypted": true`) {
		t.Errorf("backup result should report encrypted=true:\n%s", out)
	}

	// Pull the BackupID from the JSON.  The output.Result envelope
	// puts the body directly under `result` — no nested `body`
	// wrapper, despite the field name on the Go side (see
	// output.Result: WithBody assigns to .Result).  An older
	// shape did wrap under `body`; the test was carrying that
	// assumption.
	var resultDoc struct {
		Result struct {
			BackupID string `json:"backup_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resultDoc); err != nil {
		t.Fatal(err)
	}
	backupID := resultDoc.Result.BackupID
	if backupID == "" {
		t.Fatalf("no backup_id in result:\n%s", out)
	}

	// Verify the manifest on disk records EncryptionInfo.
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	manifestKey := backup.PrimaryPath("db1", backupID)
	rc, err := sp.Get(context.Background(), manifestKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var manifestRaw map[string]any
	if err := json.NewDecoder(rc).Decode(&manifestRaw); err != nil {
		t.Fatal(err)
	}
	encField, ok := manifestRaw["encryption"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.encryption missing; manifest: %v", manifestRaw)
	}
	if encField["scheme"] != "aes-256-gcm" {
		t.Errorf("scheme = %v, want aes-256-gcm", encField["scheme"])
	}
	if encField["kek_ref"] != keystore.KEKRefLocal {
		t.Errorf("kek_ref = %v, want %s", encField["kek_ref"], keystore.KEKRefLocal)
	}
	if encField["wrapped_dek"] == "" {
		t.Error("wrapped_dek empty")
	}

	// Restore — keystore.KEKResolver gets wired in automatically.
	// --verify=skip turns off the pg_verifybackup gate; the
	// separate --verify-restore=off knob turns off the post-
	// restore pg_ctl-start smoke test.  Both matter for this
	// scenario: the runner produces a PG 17 PGDATA (the
	// testcontainers postgres:17-alpine), and the CI host (or
	// the local dev machine) frequently has a different PG
	// major's pg_ctl on PATH, so the smoke test would refuse
	// with "database files are incompatible with server".  That's
	// orthogonal to what this test is verifying — namely that the
	// restore decrypts and writes the manifest's files into the
	// target dir.
	target := filepath.Join(t.TempDir(), "restored")
	rOut, rErr, exit := runCmd(t,
		"restore", "db1", backupID,
		"--repo", repoURL,
		"--target", target,
		"--verify", "skip",
		"--verify-restore", "off",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("restore exit = %d\nstdout:\n%s\nstderr:\n%s", exit, rOut, rErr)
	}
	if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
		t.Errorf("PG_VERSION not present after restore: %v", err)
	}
}

func TestIntegration_BackupNoEncrypt_OverridesKEK(t *testing.T) {
	srv := testkit.StartPostgres(t)
	cfgDir := t.TempDir()
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)
	repoURL := "file://" + t.TempDir()

	// Set up KEK via init.
	if _, _, exit := runCmd(t,
		"init", "--yes",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--deployment", "db1",
		"--skip-backup",
		"--output", "json",
	); exit != 0 {
		t.Fatal("init failed")
	}

	// Backup with --no-encrypt should produce an unencrypted manifest.
	out, _, exit := runCmd(t,
		"backup", "db1",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--no-encrypt",
		"--fast",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("backup exit = %d", exit)
	}
	if !strings.Contains(out, `"encrypted": false`) {
		t.Errorf("expected encrypted=false; got:\n%s", out)
	}
}

func TestIntegration_BackupConflictingFlags_ExitMisuse(t *testing.T) {
	cfgDir := t.TempDir()
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	_, _, exit := runCmd(t,
		"backup", "db1",
		"--pg-connection", "postgres://x",
		"--repo", "file://"+t.TempDir(),
		"--encrypt", "--no-encrypt",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (ExitMisuse for conflicting flags)", exit)
	}
	_ = errors.Is // keep compilable if test grows
}

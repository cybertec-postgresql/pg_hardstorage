//go:build integration

package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestIntegration_BackupTDE_StampsManifest: a --tde backup stamps source_tde
// on the manifest and surfaces it in the backup result (issue #48). TDE is
// operator-declared metadata, so a vanilla postgres source is sufficient —
// no PGEE image needed; the flag records posture for restore-time safety.
func TestIntegration_BackupTDE_StampsManifest(t *testing.T) {
	srv := testkit.StartPostgres(t)

	cfgDir := t.TempDir()
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)
	repoURL := "file://" + t.TempDir()

	if _, _, exit := runCmd(t, "init", "--yes", "--encrypt=false",
		"--pg-connection", srv.DSN, "--repo", repoURL,
		"--deployment", "db1", "--skip-backup", "--output", "json"); exit != 0 {
		t.Fatalf("init exit %d", exit)
	}

	out, stderr, exit := runCmd(t, "backup", "db1",
		"--pg-connection", srv.DSN, "--repo", repoURL, "--fast",
		"--tde", "--tde-engine", "cybertec_enterprise",
		"--tde-key-ref", "kms-secret://prod/pgee", "--output", "json")
	if exit != 0 {
		t.Fatalf("backup exit %d\nstdout:%s\nstderr:%s", exit, out, stderr)
	}
	// Result surfaces source_tde (issue #48 asked for this).
	if !strings.Contains(out, `"source_tde"`) || !strings.Contains(out, "cybertec_enterprise") {
		t.Errorf("backup result should surface source_tde with the engine:\n%s", out)
	}

	var doc struct {
		Result struct {
			BackupID  string `json:"backup_id"`
			SourceTDE *struct {
				Engine string `json:"engine"`
				KeyRef string `json:"key_ref"`
			} `json:"source_tde"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Result.SourceTDE == nil || doc.Result.SourceTDE.Engine != "cybertec_enterprise" {
		t.Fatalf("result.source_tde wrong: %+v", doc.Result.SourceTDE)
	}

	// The persisted manifest carries source_tde (the docs' promise; the
	// original bug was that this was always null).
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	rc, err := sp.Get(context.Background(), backup.PrimaryPath("db1", doc.Result.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var m map[string]any
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}
	tde, ok := m["source_tde"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.source_tde missing (issue #48); manifest keys: %v", keysOf(m))
	}
	if tde["engine"] != "cybertec_enterprise" || tde["key_ref"] != "kms-secret://prod/pgee" {
		t.Errorf("manifest.source_tde = %v, want engine=cybertec_enterprise key_ref=kms-secret://prod/pgee", tde)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

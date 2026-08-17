package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// deploymentTDE resolves source-TDE from flags and/or the deployment's
// tde: block. Issue #48: before the fix, the tde: block and --tde flag were
// ignored on the backup path, so source_tde was always null.
func TestDeploymentTDE_Resolution(t *testing.T) {
	// A config dir with a deployment declaring a tde: block.
	cfgDir := t.TempDir()
	yaml := `schema: pg_hardstorage.config.v1
deployments:
    db1:
        pg_connection: host=localhost user=postgres dbname=postgres
        repo: file:///tmp/repo
        tde:
            enabled: true
            engine: cybertec_enterprise
            key_ref: kms-secret://prod/pgee
    plain:
        pg_connection: host=localhost user=postgres dbname=postgres
        repo: file:///tmp/repo2
`
	if err := os.WriteFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)

	t.Run("config block honoured (the #48 repro)", func(t *testing.T) {
		got := deploymentTDE("db1", false, "", "")
		if got == nil {
			t.Fatal("tde: block ignored — source_tde would be null (issue #48)")
		}
		if got.Engine != "cybertec_enterprise" || got.KeyRef != "kms-secret://prod/pgee" {
			t.Fatalf("got %+v, want engine=cybertec_enterprise key_ref=kms-secret://prod/pgee", got)
		}
	})
	t.Run("no tde declared -> nil", func(t *testing.T) {
		if got := deploymentTDE("plain", false, "", ""); got != nil {
			t.Fatalf("plain deployment must yield nil source_tde, got %+v", got)
		}
	})
	t.Run("flag alone (no config)", func(t *testing.T) {
		got := deploymentTDE("plain", true, "", "")
		if got == nil || got.Engine != "unspecified" {
			t.Fatalf("--tde alone must stamp engine=unspecified, got %+v", got)
		}
	})
	t.Run("flag overrides config engine/keyref", func(t *testing.T) {
		got := deploymentTDE("db1", true, "pg_tde", "vault://k")
		if got == nil || got.Engine != "pg_tde" || got.KeyRef != "vault://k" {
			t.Fatalf("flags must win over config block, got %+v", got)
		}
	})
	t.Run("engine flag implies enabled", func(t *testing.T) {
		got := deploymentTDE("plain", false, "edb_tde", "")
		if got == nil || got.Engine != "edb_tde" {
			t.Fatalf("--tde-engine must imply --tde, got %+v", got)
		}
	})
}

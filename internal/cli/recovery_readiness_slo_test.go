package cli_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression (M7): `recovery readiness` help says --rpo-seconds /
// --rto-seconds "default: read from the deployment's SLO config when
// present", but the config was never read — targets stayed 0 and a
// deployment far outside its declared RPO read as ready. An explicit
// flag still wins (including 0 = "no target check").
func TestRecoveryReadiness_DefaultsTargetsFromSLOConfig(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()
	body := "schema: pg_hardstorage.config.v1\ndeployments:\n  db1:\n    repo: " + w.repoURL +
		"\n    slo:\n      rpo_seconds: 3600\n      rto_seconds: 900\n"
	if err := os.MkdirAll(w.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.configDir, "pg_hardstorage.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	commitBackupLSN(t, w, "db1", "b1", "0/3000028", "0/30001A0", time.Now().UTC().Add(-5*time.Hour))

	var rep struct {
		RPO *struct {
			TargetSeconds int64 `json:"target_seconds"`
			Met           bool  `json:"met"`
		} `json:"rpo"`
		RTO *struct {
			TargetSeconds int64 `json:"target_seconds"`
		} `json:"rto"`
	}
	stdout, stderr, _ := runCLI(t, "recovery", "readiness", "db1", "--repo", w.repoURL, "--no-encryption", "-o", "json")
	bodyOf(t, stdout+stderr, &rep)
	if rep.RPO == nil || rep.RPO.TargetSeconds != 3600 || rep.RPO.Met {
		t.Errorf("rpo = %+v, want target 3600 from the SLO config and not met (backup is 5h old)", rep.RPO)
	}
	if rep.RTO == nil || rep.RTO.TargetSeconds != 900 {
		t.Errorf("rto = %+v, want target 900 from the SLO config", rep.RTO)
	}

	// An explicit flag overrides the config.
	stdout, stderr, _ = runCLI(t, "recovery", "readiness", "db1", "--repo", w.repoURL, "--no-encryption",
		"--rpo-seconds", "0", "-o", "json")
	rep.RPO = nil
	bodyOf(t, stdout+stderr, &rep)
	if rep.RPO != nil && rep.RPO.TargetSeconds != 0 {
		t.Errorf("--rpo-seconds 0 must override the config; rpo = %+v", rep.RPO)
	}
}

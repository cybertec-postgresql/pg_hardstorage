package cli_test

import (
	"strings"
	"testing"
)

// (runCLI lives in listshowstatus_test.go: returns stdout, stderr, exit.)

// TestDeploymentFlagResolution_Matrix proves the systemic fix (#12): every
// deployment-scoped command resolves --repo / --pg-connection from the
// named deployment in pg_hardstorage.yaml, instead of demanding the flag.
// Each command still fails afterwards (dead PG port / empty repo dir), but
// it must NOT fail with usage.missing_flag — that's the regression signal.
func TestDeploymentFlagResolution_Matrix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	repoDir := t.TempDir()
	// Port :1 is a closed port → connection refused immediately, so PG-
	// touching commands fail fast and never hit a real local server.
	t.Setenv("PG_HARDSTORAGE_CONFIG",
		"deployments:\n  mytest:\n"+
			"    pg_connection: postgresql://postgres@127.0.0.1:1/postgres\n"+
			"    repo: file://"+repoDir+"\n")

	target := t.TempDir() + "/restore"
	cmds := [][]string{
		// MarkFlagRequired("repo") family:
		{"list", "mytest"},
		{"status", "mytest"},
		{"show", "mytest", "someid"},
		{"hold", "list", "mytest"},
		{"rotate", "mytest"},
		{"recovery", "readiness", "mytest"},
		{"kms", "verify", "mytest"},
		{"wal", "list", "mytest"},
		{"wal", "audit", "mytest"},
		{"wal", "gaps", "mytest"},
		// MarkFlagRequired("pg-connection"):
		{"wal", "preflight", "mytest"},
		// requireFlags() family (the originally-reported path):
		{"backup", "mytest"},
		{"verify", "mytest", "latest"},
		{"restore", "mytest", "latest", "--target", target},
	}
	for _, c := range cmds {
		t.Run(strings.Join(c, "_"), func(t *testing.T) {
			stdout, stderr, _ := runCLI(t, append(append([]string{}, c...), "-o", "json")...)
			combined := stdout + stderr
			if strings.Contains(combined, "usage.missing_flag") {
				t.Fatalf("`pg_hardstorage %s` demanded a flag the configured deployment provides (#12):\n%s",
					strings.Join(c, " "), combined)
			}
		})
	}
}

// TestDeploymentFlagResolution_NoConfigStillRequires is the negative
// control: with no config and no flags, the requirement must still fire,
// so the resolver never papers over a genuine omission.
func TestDeploymentFlagResolution_NoConfigStillRequires(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG", "")

	cmds := [][]string{
		{"list", "ghost"},
		{"status", "ghost"},
		{"wal", "preflight", "ghost"},
		{"backup", "ghost"},
	}
	for _, c := range cmds {
		t.Run(strings.Join(c, "_"), func(t *testing.T) {
			stdout, stderr, _ := runCLI(t, append(append([]string{}, c...), "-o", "json")...)
			combined := stdout + stderr
			if !strings.Contains(combined, "usage.missing_flag") {
				t.Fatalf("`pg_hardstorage %s` with no config/flag should still require the flag:\n%s",
					strings.Join(c, " "), combined)
			}
		})
	}
}

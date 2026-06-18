package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func seedResidencyConfig(t *testing.T, dir, name string) {
	t.Helper()
	body := `schema: pg_hardstorage.config.v1
deployments:
  ` + name + `:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResidency_Set_HappyPath(t *testing.T) {
	dir := configDir(t)
	seedResidencyConfig(t, dir, "db1")
	out, _, exit := runCmd(t, "residency", "set", "db1", "EU", "us-east-1", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	// case-insensitive normalization: EU → eu
	for _, want := range []string{
		`"current": [`,
		`"eu"`,
		`"us-east-1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestResidency_Clear(t *testing.T) {
	dir := configDir(t)
	seedResidencyConfig(t, dir, "db1")
	_, _, exit := runCmd(t, "residency", "set", "db1", "eu", "--output", "json")
	if exit != 0 {
		t.Fatalf("set exit = %d", exit)
	}
	out, _, exit := runCmd(t, "residency", "clear", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("clear exit = %d", exit)
	}
	if !strings.Contains(out, `"current": []`) && !strings.Contains(out, `"current": null`) {
		t.Errorf("clear should leave current empty:\n%s", out)
	}
}

func TestResidency_Check_NoConstraint_AlwaysCompliant(t *testing.T) {
	dir := configDir(t)
	seedResidencyConfig(t, dir, "db1")
	// Even with a local-fs repo (region unknown), no constraint = compliant.
	out, _, exit := runCmd(t, "residency", "check", "db1", "--output", "json")
	// Make the test independent of the seeded repo URL — write a real
	// file:// repo so the check doesn't fail at openRepo.
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(
		"schema: pg_hardstorage.config.v1\n"+
			"deployments:\n"+
			"  db1:\n"+
			"    pg_connection: postgres://x@h/db\n"+
			"    repo: "+repoURL+"\n"), 0o644)

	out, _, exit = runCmd(t, "residency", "check", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	if !strings.Contains(out, `"compliant": true`) {
		t.Errorf("no-constraint should be compliant:\n%s", out)
	}
}

func TestResidency_Check_FSRepo_FailsAnyConstraint(t *testing.T) {
	// fs plugin reports RegionUnknown — local-disk repo cannot
	// enforce residency, so any non-empty policy must FAIL the
	// check (better to surface the impossibility loudly than
	// silently treat unknown as a pass).
	dir := configDir(t)
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(
		"schema: pg_hardstorage.config.v1\n"+
			"deployments:\n"+
			"  db1:\n"+
			"    pg_connection: postgres://x@h/db\n"+
			"    repo: "+repoURL+"\n"+
			"    residency: [eu]\n"), 0o644)

	_, errb, exit := runCmd(t, "residency", "check", "db1", "--output", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("fs repo with residency should exit ExitVerifyFailed(%d); got %d\nstderr: %s",
			output.ExitVerifyFailed, exit, errb)
	}
	if !strings.Contains(errb, "verify.residency_violation") {
		t.Errorf("expected verify.residency_violation:\n%s", errb)
	}
}

func TestResidency_List_StableOrder(t *testing.T) {
	dir := configDir(t)
	body := `schema: pg_hardstorage.config.v1
deployments:
  beta:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    residency: [eu, us]
  alpha:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "residency", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	posAlpha := strings.Index(out, `"deployment": "alpha"`)
	posBeta := strings.Index(out, `"deployment": "beta"`)
	if posAlpha < 0 || posBeta < 0 {
		t.Fatalf("deployments missing:\n%s", out)
	}
	if !(posAlpha < posBeta) {
		t.Errorf("residency list must sort alphabetically (alpha < beta); got pos: alpha=%d beta=%d",
			posAlpha, posBeta)
	}
}

// Direct-API check of the matcher logic — exercise every shape so the
// behaviour is pinned independently of the CLI plumbing.
func TestResidency_Matcher(t *testing.T) {
	cases := []struct {
		name      string
		region    string
		allowed   []string
		compliant bool
	}{
		{"no policy", "us-east-1", nil, true},
		{"empty policy slice", "us-east-1", []string{}, true},
		{"region unknown + policy", "", []string{"eu"}, false},
		{"exact match", "eu-west-1", []string{"eu-west-1"}, true},
		{"prefix match", "eu-west-1", []string{"eu"}, true},
		{"prefix match alt", "eu-central-1", []string{"eu"}, true},
		{"non-match", "us-east-1", []string{"eu"}, false},
		{"non-match exact", "eu-west-1", []string{"eu-central-1"}, false},
		{"multi-allow first matches", "us-east-1", []string{"us", "eu"}, true},
		{"multi-allow second matches", "eu-west-1", []string{"us", "eu"}, true},
		{"case-insensitive policy", "eu-west-1", []string{"EU"}, true},
		{"case-insensitive region", "EU-West-1", []string{"eu"}, true},
		{"prefix not pseudo-match", "europa", []string{"eu"}, false}, // "europa" does NOT match "eu" prefix because we require "eu-"
	}
	// Build a fake StoragePlugin reporting `region` so we exercise
	// the public API path. The fs plugin doesn't implement RegionAware,
	// so we build a tiny shim type.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := fakeRegionStorage{region: c.region}
			ok, _ := cli.CheckDeploymentResidency(sp, config.DeploymentConfig{
				Residency: c.allowed,
			})
			if ok != c.compliant {
				t.Errorf("region=%q allowed=%v: got %v, want %v",
					c.region, c.allowed, ok, c.compliant)
			}
		})
	}
}

// fakeRegionStorage is the minimal StoragePlugin shim that implements
// only the methods + RegionAware needed by checkResidency. We don't
// build a real plugin because residency only consults Region().
type fakeRegionStorage struct {
	storage.StoragePlugin
	region string
}

func (f fakeRegionStorage) Region() string { return f.region }

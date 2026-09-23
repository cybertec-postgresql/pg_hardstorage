package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// Issue #66 follow-up, reported by @marsqd with this exact config:
//
//	local:
//	  retention:
//	    policy: count
//	    keep_fulls: 2
//
// `pg_hardstorage rotate local` answered "Policy: gfs" and planned to
// keep 7 of 8 backups. The scheduled rotate task read the block; the
// manual command did not.

func flagDefaults() rotateOpts {
	// What cobra hands runRotate when no retention flag is passed.
	return rotateOpts{policy: "gfs", keepDaily: 7, keepWeekly: 4, keepMonthly: 12,
		keepYearly: 5, keepFor: 30 * 24 * time.Hour, keepFulls: 14}
}

func TestRotatePolicy_ReportersConfigIsHonoured(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"local": {Retention: config.RetentionConfig{Policy: "count", KeepFulls: 2}},
	}
	p, src, err := resolveRotatePolicy("local", false, flagDefaults(), deps)
	if err != nil {
		t.Fatal(err)
	}
	want := retention.CountPolicy{KeepFulls: 2}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("manual rotate must apply the deployment's declared policy: got %#v, want %#v "+
			"(this is the plan @marsqd saw — GFS instead of count/2)", p, want)
	}
	if src != policySourceConfig {
		t.Errorf("source = %q, want %q — the plan must say where its policy came from", src, policySourceConfig)
	}
}

func TestRotatePolicy_ExplicitFlagsWin(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"local": {Retention: config.RetentionConfig{Policy: "count", KeepFulls: 2}},
	}
	o := flagDefaults()
	o.policy, o.keepFor = "simple", 48*time.Hour
	p, src, err := resolveRotatePolicy("local", true, o, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, retention.SimplePolicy{KeepFor: 48 * time.Hour}) || src != policySourceFlags {
		t.Fatalf("an explicit flag must override config for this run: got %#v from %q", p, src)
	}
}

func TestRotatePolicy_UndeclaredFallsBackAndSaysSo(t *testing.T) {
	deps := map[string]config.DeploymentConfig{"bare": {}}
	for _, d := range []struct {
		name string
		deps map[string]config.DeploymentConfig
	}{{"bare", deps}, {"not-in-config", deps}, {"no-config-file", nil}} {
		p, src, err := resolveRotatePolicy(d.name, false, flagDefaults(), d.deps)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		if p.Name() != "gfs" || src != policySourceDefault {
			t.Errorf("%s: got %s from %q, want gfs from %q", d.name, p.Name(), src, policySourceDefault)
		}
	}
}

// A retention block that does not parse must stop the run. Quietly
// falling back to GFS would be the original bug with a worse excuse.
func TestRotatePolicy_InvalidConfigRefuses(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"local": {Retention: config.RetentionConfig{Policy: "simple", KeepFor: "a fortnight"}},
	}
	if _, _, err := resolveRotatePolicy("local", false, flagDefaults(), deps); err == nil {
		t.Fatal("an unparsable retention block must refuse, not fall back to the default policy")
	}
	deps["local"] = config.DeploymentConfig{Retention: config.RetentionConfig{Policy: "fifo"}}
	if _, _, err := resolveRotatePolicy("local", false, flagDefaults(), deps); err == nil {
		t.Fatal("an unknown policy name must refuse")
	}
}

// TestRotatePolicy_ManualMatchesScheduled is the drift guard. The bug
// existed because two code paths answered "what is this deployment's
// retention?" differently. For any declared block, manual rotate and the
// agent's scheduled task must now arrive at the same policy.
func TestRotatePolicy_ManualMatchesScheduled(t *testing.T) {
	for _, r := range []config.RetentionConfig{
		{Policy: "count", KeepFulls: 2},
		{Policy: "count"},
		{Policy: "simple", KeepFor: "7d"},
		{Policy: "gfs", KeepDaily: 3, KeepMonthly: 1},
		{KeepDaily: 10}, // policy omitted → gfs, with the override
	} {
		deps := map[string]config.DeploymentConfig{"d": {Retention: r}}
		manual, _, err := resolveRotatePolicy("d", false, flagDefaults(), deps)
		if err != nil {
			t.Fatalf("%+v: manual: %v", r, err)
		}
		scheduled, err := buildRetentionPolicy(r)
		if err != nil {
			t.Fatalf("%+v: scheduled: %v", r, err)
		}
		if !reflect.DeepEqual(manual, scheduled) {
			t.Errorf("%+v: manual rotate chose %#v, the scheduled task chooses %#v — "+
				"one deployment, two retention policies", r, manual, scheduled)
		}
	}
}

// Found by ultrareview: runRotate discarded config.Load errors, so a
// pg_hardstorage.yaml that exists but will not parse made every
// deployment look unconfigured, and rotate planned with the built-in
// GFS defaults — the #66 bug again, reached through a bad edit. It must
// refuse unless explicit retention flags make the config irrelevant.
func TestRotate_BrokenConfigRefusesInsteadOfDefaulting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "deployments:\n  local:\n    retention:\n      policy: count\n      keep_fulls: [unclosed\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "pg_hardstorage.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, exit := rotateCLI(t, "rotate", "local", "--repo", "file://"+filepath.Join(root, "repo"))
	if exit == 0 || !strings.Contains(stderr, "config.load_failed") {
		t.Fatalf("broken config must refuse with config.load_failed, got exit %d:\n%s", exit, stderr)
	}

	// With an explicit policy the config is not consulted, so the run is
	// NOT refused for that reason (it may fail later on the empty repo).
	_, stderr, _ = rotateCLI(t, "rotate", "local", "--repo", "file://"+filepath.Join(root, "repo"),
		"--policy", "count", "--keep-fulls", "2")
	if strings.Contains(stderr, "config.load_failed") {
		t.Errorf("explicit retention flags must bypass the config; got:\n%s", stderr)
	}
}

// rotateCLI runs the real root command in-package.
func rotateCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	root := NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	exit := Run(root)
	return out.String(), errb.String(), exit
}

package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runRestore is a small helper for restore-command tests.
func runRestore(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"restore"}, args...))
	exit = cli.Run(root)
	return out.String(), errb.String(), exit
}

func TestRestore_RequiresPositionalArgs(t *testing.T) {
	// No args at all.
	_, _, exit := runRestore(t)
	if exit == int(output.ExitOK) {
		t.Error("restore with no args should not succeed")
	}
	// Only one positional.
	_, _, exit = runRestore(t, "db1")
	if exit == int(output.ExitOK) {
		t.Error("restore with one arg should not succeed")
	}
}

func TestRestore_RequiresRepo(t *testing.T) {
	// Isolate from ambient config so `restore db1` has no deployment to
	// resolve --repo from (it now reads the config — #12).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG", "")
	stdout, errb, exit := runRestore(t, "db1", "abc123", "--target", "/tmp/x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should map to ExitMisuse(%d); got %d", output.ExitMisuse, exit)
	}
	if stdout != "" {
		t.Errorf("usage error must not land on stdout: %q", stdout)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if !res.IsError() {
		t.Fatal("Result should carry an error")
	}
	if res.Error.Code != "usage.missing_flag" {
		t.Errorf("code = %q", res.Error.Code)
	}
	if !strings.Contains(res.Error.Message, "--repo") {
		t.Errorf("message should mention --repo: %q", res.Error.Message)
	}
}

func TestRestore_RequiresTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG", "")
	_, errb, exit := runRestore(t, "db1", "abc123", "--repo", "file:///tmp/x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --target should map to ExitMisuse; got %d", exit)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if !strings.Contains(res.Error.Message, "--target") {
		t.Errorf("message should mention --target: %q", res.Error.Message)
	}
}

// Regression for #12: `restore <deployment> <id> --target ...` must
// resolve --repo from the named deployment in config when omitted.
func TestRestore_ResolvesRepoFromDeployment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	repoDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG",
		"deployments:\n  mytest:\n    repo: file://"+repoDir+"\n")

	// No --repo: it must be taken from the deployment. The restore still
	// fails (empty dir isn't a repo), but NOT with usage.missing_flag.
	_, errb, _ := runRestore(t, "mytest", "latest", "--target", t.TempDir()+"/r", "-o", "json")
	if strings.Contains(errb, "usage.missing_flag") {
		t.Fatalf("restore of a configured deployment must not demand --repo (issue #12); stderr:\n%s", errb)
	}
}

func TestRestore_RepoNotARepo_NotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	emptyDir := t.TempDir()
	_, errb, exit := runRestore(t,
		"db1", "abc123",
		"--repo", "file://"+emptyDir,
		"--target", t.TempDir()+"/restored",
		"-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound(%d)", exit, output.ExitNotFound)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if res.Error.Code != "notfound.repo" {
		t.Errorf("code = %q", res.Error.Code)
	}
}

func TestRestore_BadVerifyMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	_, errb, exit := runRestore(t,
		"db1", "abc",
		"--repo", "file://"+t.TempDir(),
		"--target", t.TempDir()+"/restored",
		"--verify", "bogus",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--verify=bogus should map to ExitMisuse; got %d", exit)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if !strings.Contains(res.Error.Code, "bad_verify_mode") {
		t.Errorf("code = %q", res.Error.Code)
	}
}

func TestRestore_LatestKeyword_NoBackups(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	// Build an empty (initialised but no backups) repo via the CLI's
	// repo-init code path, then ask for `restore ... latest`.
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir

	// repo init through the CLI ensures HSREPO is written.
	{
		root := cli.NewRoot()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"repo", "init", repoURL})
		if exit := cli.Run(root); exit != int(output.ExitOK) {
			t.Fatalf("repo init failed: exit=%d", exit)
		}
	}

	_, errb, exit := runRestore(t,
		"db1", "latest",
		"--repo", repoURL,
		"--target", t.TempDir()+"/restored",
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("expected error for empty repo")
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if res.Error.Code != "notfound.backup" {
		t.Errorf("code = %q, want notfound.backup", res.Error.Code)
	}
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
}

// TestRestore_SkipGapCheckFlagDiscoverable: regression guard
// that the v0.6+ override flag is wired into the CLI surface.
// An operator hitting a known-safe gap-affected PITR needs to
// be able to find the bypass at 3am without reading source.
func TestRestore_SkipGapCheckFlagDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "restore", "--help")
	if !strings.Contains(stdout, "--skip-gap-check") {
		t.Errorf("restore --help should advertise --skip-gap-check:\n%s", stdout)
	}
}

// Issue #78 regression: --to-lsn must reject non-LSN values up front
// (was silently accepted; --preview reported a plan and the GUC was
// only refused much later by PG when recovery actually started).
func TestRestore_ToLSN_RejectsGarbage(t *testing.T) {
	cases := []string{
		"hm",         // the report's literal example
		"0/",         // empty low half
		"/0",         // empty high half
		"GHIJ/KLMN",  // non-hex
		"123",        // missing slash
		"0/3000028x", // trailing garbage
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			_, errb, exit := runRestore(t,
				"db1", "latest",
				"--repo", "file:///tmp/repo",
				"--target", "/tmp/x",
				"--to-lsn", bad,
				"--preview",
				"-o", "json",
			)
			if exit != int(output.ExitMisuse) {
				t.Fatalf("expected ExitMisuse(%d); got %d\nstderr=%s",
					output.ExitMisuse, exit, errb)
			}
			var res output.Result
			if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
				t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
			}
			if res.Error.Code != "usage.bad_lsn" {
				t.Errorf("code = %q (want usage.bad_lsn); message=%q",
					res.Error.Code, res.Error.Message)
			}
			if !strings.Contains(res.Error.Message, "--to-lsn") {
				t.Errorf("message should mention --to-lsn: %q", res.Error.Message)
			}
		})
	}
}

// TestRestore_ToLSN_AcceptsValid keeps the happy-path open so the
// guard above isn't over-eager. Mixed case is folded to upper.
func TestRestore_ToLSN_AcceptsValid(t *testing.T) {
	// Use control-plane mode to short-circuit before any repo I/O;
	// we only care that the LSN parser doesn't refuse a real LSN.
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{
		"restore", "db1", "latest",
		"--control-plane", "http://127.0.0.1:1", // unreachable; we just need to clear validation
		"--target", "/tmp/x",
		"--to-lsn", "0/3000028",
		"-o", "json",
	})
	_ = cli.Run(root)
	if strings.Contains(errb.String(), "usage.bad_lsn") {
		t.Errorf("valid LSN rejected: %s", errb.String())
	}
}

// --to-timeline must be "latest" or a positive integer. PG would
// otherwise accept the GUC at config-load time and refuse later.
func TestRestore_ToTimeline_RejectsGarbage(t *testing.T) {
	cases := []string{"foo", "-1", "0", "1.5", "9999999999999999999"}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			_, errb, exit := runRestore(t,
				"db1", "latest",
				"--repo", "file:///tmp/repo",
				"--target", "/tmp/x",
				"--to-timeline", bad,
				"--preview",
				"-o", "json",
			)
			if exit != int(output.ExitMisuse) {
				t.Fatalf("expected ExitMisuse(%d); got %d\nstderr=%s",
					output.ExitMisuse, exit, errb)
			}
			var res output.Result
			if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
				t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
			}
			if res.Error.Code != "usage.bad_timeline" {
				t.Errorf("code = %q (want usage.bad_timeline)", res.Error.Code)
			}
		})
	}
}

// --to-action enum must reject typos at the CLI boundary in both
// local and control-plane modes (controlplane previously forwarded
// the raw string to the agent).
func TestRestore_ToAction_RejectsGarbage(t *testing.T) {
	_, errb, exit := runRestore(t,
		"db1", "latest",
		"--repo", "file:///tmp/repo",
		"--target", "/tmp/x",
		"--to-lsn", "0/3000028",
		"--to-action", "burn-it-down",
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Fatalf("expected ExitMisuse(%d); got %d\nstderr=%s",
			output.ExitMisuse, exit, errb)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if res.Error.Code != "usage.bad_action" {
		t.Errorf("code = %q (want usage.bad_action)", res.Error.Code)
	}
}

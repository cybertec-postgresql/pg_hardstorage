package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

func invokeVerify(t *testing.T, args ...string) (stderr string, exit int) {
	t.Helper()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"verify"}, args...))
	exit = cli.Run(root)
	return errb.String(), exit
}

// Regression for #12: `verify <deployment>` must resolve --repo from the
// named deployment in config when omitted, not demand the flag.
func TestVerify_ResolvesRepoFromDeployment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	repoDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG",
		"deployments:\n  mytest:\n    repo: file://"+repoDir+"\n")

	// No --repo: it must come from the deployment. Verify still fails
	// (empty dir isn't a repo), but NOT with usage.missing_flag.
	errb, _ := invokeVerify(t, "mytest", "latest", "-o", "json")
	if strings.Contains(errb, "usage.missing_flag") {
		t.Fatalf("verify of a configured deployment must not demand --repo (issue #12); stderr:\n%s", errb)
	}
}

// Without a configured deployment or --repo, verify still errors with
// usage.missing_flag (the resolution must not paper over a real omission).
func TestVerify_RequiresRepo_NoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG", "")
	errb, _ := invokeVerify(t, "db1", "latest", "-o", "json")
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Fatalf("verify without --repo or config should be usage.missing_flag; stderr:\n%s", errb)
	}
}

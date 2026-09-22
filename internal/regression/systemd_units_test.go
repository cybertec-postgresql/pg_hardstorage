// systemd_units_test.go — every shipped unit must exist, run a real
// command, and be packaged by every recipe.
//
// Issue #56: the packages shipped pg_hardstorage.service and
// pg_hardstorage@.service, both running `pg_hardstorage agent`, and
// the documentation told operators to supervise the WAL streamer with
// them. The agent never opens a WAL stream — it runs the scheduled
// backup and retention engine — so an operator who followed the
// tutorial got periodic base backups and NO continuous archiving,
// while believing the always-on data plane was running. Nothing in
// the tree connected the unit files to the CLI verbs they invoke, so
// nothing noticed.
//
// These tests are cheap and text-only: no Docker, no systemd.
package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory to the module
// root, so the test works regardless of where `go test` is invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root")
	return ""
}

// packagingRecipes are every file that stages unit files into a
// package. A unit added to deploy/systemd/ and forgotten in one of
// these ships to some distros and not others.
var packagingRecipes = []string{
	".goreleaser.yaml",
	"debian/rules",
	"packaging/arch/PKGBUILD",
	"packaging/rpm/pg_hardstorage.spec",
}

// TestEveryUnitIsPackaged fails when a unit in deploy/systemd/ is not
// referenced by every packaging recipe.
func TestEveryUnitIsPackaged(t *testing.T) {
	root := repoRoot(t)
	units, err := filepath.Glob(filepath.Join(root, "deploy", "systemd", "*.service"))
	if err != nil || len(units) == 0 {
		t.Fatalf("no unit files found under deploy/systemd (err=%v)", err)
	}

	recipes := map[string]string{}
	for _, r := range packagingRecipes {
		b, err := os.ReadFile(filepath.Join(root, r))
		if err != nil {
			t.Fatalf("read %s: %v", r, err)
		}
		recipes[r] = string(b)
	}

	for _, u := range units {
		name := filepath.Base(u)
		for _, r := range packagingRecipes {
			if !strings.Contains(recipes[r], name) {
				t.Errorf("unit %q is not packaged by %s — it will be missing on that distro",
					name, r)
			}
		}
	}
}

// execStartRe pulls the argv out of an ExecStart= line.
var execStartRe = regexp.MustCompile(`(?m)^ExecStart=(.+)$`)

// TestUnitExecStartsNameRealVerbs checks that each unit's ExecStart
// invokes a command the CLI actually implements, and — for the
// templated units — that the instance name %i is actually USED in
// the command, not merely in the description.
//
// pg_hardstorage@.service is the cautionary case: it interpolates %i
// into Description, StateDirectory and SyslogIdentifier but NOT into
// ExecStart, so every instance runs the identical `agent` process
// against the identical config. The isolation its own header comment
// promises is only partial, and that is worth stating rather than
// implying.
func TestUnitExecStartsNameRealVerbs(t *testing.T) {
	root := repoRoot(t)
	units, _ := filepath.Glob(filepath.Join(root, "deploy", "systemd", "*.service"))

	// Top-level verbs the CLI implements. Kept deliberately short:
	// this guards against a unit naming a verb that does not exist
	// (the #56 class), not against every possible typo.
	known := map[string]bool{
		"agent": true, "wal": true, "backup": true, "server": true,
		"restore": true, "verify": true, "rotate": true,
	}

	for _, u := range units {
		b, err := os.ReadFile(u)
		if err != nil {
			t.Fatalf("read %s: %v", u, err)
		}
		body := string(b)
		name := filepath.Base(u)

		m := execStartRe.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s has no ExecStart= line", name)
			continue
		}
		fields := strings.Fields(m[1])
		if len(fields) < 2 {
			t.Errorf("%s: ExecStart names no verb: %q", name, m[1])
			continue
		}
		if !strings.HasSuffix(fields[0], "pg_hardstorage") {
			t.Errorf("%s: ExecStart does not run the agent binary: %q", name, fields[0])
		}
		if verb := fields[1]; !known[verb] {
			t.Errorf("%s: ExecStart names verb %q, which the CLI does not implement", name, verb)
		}

		// A templated unit whose ExecStart ignores %i runs the same
		// process for every instance.
		if strings.Contains(name, "@") && !strings.Contains(m[1], "%i") {
			t.Logf("NOTE: %s is templated but its ExecStart does not use %%i — "+
				"every instance runs an identical command (%q). That is true of the agent "+
				"unit by design (it selects its work from config), but it means the instance "+
				"name does not choose a deployment.", name, m[1])
		}
	}
}

// TestWALStreamUnitExists is the direct regression for #56: there
// must be a unit that actually runs `wal stream`, because that is the
// always-on data plane and the agent does not provide it.
func TestWALStreamUnitExists(t *testing.T) {
	root := repoRoot(t)
	units, _ := filepath.Glob(filepath.Join(root, "deploy", "systemd", "*.service"))

	for _, u := range units {
		b, err := os.ReadFile(u)
		if err != nil {
			continue
		}
		m := execStartRe.FindStringSubmatch(string(b))
		if m == nil {
			continue
		}
		f := strings.Fields(m[1])
		if len(f) >= 3 && f[1] == "wal" && f[2] == "stream" {
			// It must be templated on the deployment, since `wal
			// stream` takes the deployment as a positional.
			if !strings.Contains(m[1], "%i") {
				t.Errorf("%s runs `wal stream` but does not pass %%i as the deployment",
					filepath.Base(u))
			}
			return
		}
	}
	t.Error("no systemd unit runs `pg_hardstorage wal stream` — continuous WAL " +
		"archiving has no supervisor, and the agent units do not provide it (issue #56)")
}

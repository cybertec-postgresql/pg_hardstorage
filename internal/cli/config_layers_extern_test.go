package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layeredConfig plants a config spread over the three layers config.Load
// merges: the env var PG_HARDSTORAGE_CONFIG (lowest), the main file, and
// a conf.d drop-in (highest). Credentials live in the env and drop-in
// layers so a write-back that flattens the merge is visible.
func layeredConfig(t *testing.T) (dir, mainPath, dropInPath string) {
	t.Helper()
	dir = configDir(t)
	t.Setenv("PG_HARDSTORAGE_CONFIG_FILE", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG", `deployments:
  envdb:
    pg_connection: "host=h user=u password=ENVSECRET"
    repo: file:///tmp/env
`)
	mainPath = filepath.Join(dir, "pg_hardstorage.yaml")
	if err := os.WriteFile(mainPath, []byte(`schema: pg_hardstorage.config.v1
deployments:
  maindb:
    pg_connection: postgres://u@h/db
    repo: file:///tmp/main
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	dropInPath = filepath.Join(dir, "conf.d", "50-extra.yaml")
	if err := os.WriteFile(dropInPath, []byte(`deployments:
  dropdb:
    pg_connection: "host=h user=u password=DROPSECRET"
    repo: file:///tmp/drop
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, mainPath, dropInPath
}

// TestConfigWriteBack_PersistsOnlyTheMainFile pins M48: an edit rewrites
// the main file with ITS OWN content plus the change — never the merged
// view, which would copy drop-in and env-var deployments (and their
// credentials) into the main file.
func TestConfigWriteBack_PersistsOnlyTheMainFile(t *testing.T) {
	_, mainPath, _ := layeredConfig(t)
	_, errb, exit := runCmd(t, "deployment", "add", "newdb",
		"--connection", "postgres://x@h/db", "--repo", "file:///tmp/x",
		"--skip-probe", "-o", "json")
	if exit != 0 {
		t.Fatalf("deployment add: exit %d\n%s", exit, errb)
	}
	body, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maindb:", "newdb:"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("main file lost %q:\n%s", want, body)
		}
	}
	for _, leaked := range []string{"envdb", "ENVSECRET", "dropdb", "DROPSECRET"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("main file now carries %q from another layer:\n%s", leaked, body)
		}
	}
}

// TestDeploymentRemove_DropInDeploymentIsRefused: removing a deployment
// that lives in a drop-in used to report "removed" while the drop-in
// kept defining it. It must refuse and name the file to edit.
func TestDeploymentRemove_DropInDeploymentIsRefused(t *testing.T) {
	_, mainPath, dropInPath := layeredConfig(t)
	before, _ := os.ReadFile(mainPath)
	_, errb, exit := runCmd(t, "deployment", "remove", "dropdb", "--yes", "-o", "json")
	if exit != 2 {
		t.Errorf("remove of a drop-in deployment: exit %d, want 2\n%s", exit, errb)
	}
	if !strings.Contains(errb, "config.defined_elsewhere") || !strings.Contains(errb, dropInPath) {
		t.Errorf("error should be config.defined_elsewhere naming %s; got\n%s", dropInPath, errb)
	}
	after, _ := os.ReadFile(mainPath)
	if string(before) != string(after) {
		t.Errorf("main file changed by a refused remove:\n%s", after)
	}
	// The env-var layer has no file to edit; it is refused the same way.
	_, errb, exit = runCmd(t, "deployment", "remove", "envdb", "--yes", "-o", "json")
	if exit != 2 || !strings.Contains(errb, "PG_HARDSTORAGE_CONFIG") {
		t.Errorf("remove of an env-var deployment: exit %d, want 2 naming PG_HARDSTORAGE_CONFIG\n%s", exit, errb)
	}
	// A main-file deployment still removes normally.
	if _, errb, exit := runCmd(t, "deployment", "remove", "maindb", "--yes", "-o", "json"); exit != 0 {
		t.Fatalf("remove maindb: exit %d\n%s", exit, errb)
	}
	body, _ := os.ReadFile(mainPath)
	if strings.Contains(string(body), "maindb") {
		t.Errorf("maindb still in main file after remove:\n%s", body)
	}
}

// TestDeploymentList_StillShowsEveryLayer: reads keep the merged view.
func TestDeploymentList_StillShowsEveryLayer(t *testing.T) {
	layeredConfig(t)
	out, errb, exit := runCmd(t, "deployment", "list", "-o", "json")
	if exit != 0 {
		t.Fatalf("exit %d\n%s", exit, errb)
	}
	for _, want := range []string{"maindb", "dropdb", "envdb"} {
		if !strings.Contains(out, want) {
			t.Errorf("deployment list lost %q:\n%s", want, out)
		}
	}
}

// TestConfigFlag_MissingExplicitFileIsAnError pins M15/M38: `-c <file>`
// naming a file that does not exist used to load an empty config, so
// lint reported "valid, 0 deployments" and the agent ran with nothing
// to do. Only the default location may be absent.
func TestConfigFlag_MissingExplicitFileIsAnError(t *testing.T) {
	configDir(t)
	t.Setenv("PG_HARDSTORAGE_CONFIG_FILE", "")
	missing := filepath.Join(t.TempDir(), "typo.yaml")
	out, errb, exit := runCmd(t, "lint", "-c", missing, "-o", "json")
	if exit != 2 {
		t.Errorf("lint -c <missing>: exit %d, want 2\nstdout: %s\nstderr: %s", exit, out, errb)
	}
	if !strings.Contains(errb, "config.load_failed") || !strings.Contains(errb, missing) {
		t.Errorf("want config.load_failed naming %s; got\n%s", missing, errb)
	}
	// The default location being absent is still fine.
	t.Setenv("PG_HARDSTORAGE_CONFIG_FILE", "")
	if _, errb, exit := runCmd(t, "lint", "-o", "json"); exit != 0 {
		t.Errorf("lint with no config at the default path: exit %d\n%s", exit, errb)
	}
}

// TestConfigFlag_EditCreatesExplicitFile: an editing command may create
// the file -c names — that is how a new config is started.
func TestConfigFlag_EditCreatesExplicitFile(t *testing.T) {
	dir := configDir(t)
	t.Setenv("PG_HARDSTORAGE_CONFIG_FILE", "")
	target := filepath.Join(t.TempDir(), "staging.yaml")
	_, errb, exit := runCmd(t, "deployment", "add", "db1", "-c", target,
		"--connection", "postgres://x@h/db", "--repo", "file:///tmp/x",
		"--skip-probe", "-o", "json")
	if exit != 0 {
		t.Fatalf("deployment add -c <new file>: exit %d\n%s", exit, errb)
	}
	if body, err := os.ReadFile(target); err != nil || !strings.Contains(string(body), "db1:") {
		t.Errorf("explicit file not written: %v\n%s", err, body)
	}
	if _, err := os.Stat(filepath.Join(dir, "pg_hardstorage.yaml")); err == nil {
		t.Error("default config file written although -c named another file")
	}
}

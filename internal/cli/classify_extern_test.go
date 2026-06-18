package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedClassifyConfig(t *testing.T, dir string, deployments ...string) {
	t.Helper()
	body := "schema: pg_hardstorage.config.v1\ndeployments:\n"
	for _, name := range deployments {
		body += "  " + name + ":\n"
		body += "    pg_connection: postgres://x@h/db\n"
		body += "    repo: file:///tmp/x\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClassify_Set_HappyPath(t *testing.T) {
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1")
	out, _, exit := runCmd(t,
		"classify", "set", "db1", "confidential",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"current": "confidential"`,
		`"previous": "internal"`,
		`"deployment": "db1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if !strings.Contains(string(body), "classification: confidential") {
		t.Errorf("config should be persisted:\n%s", body)
	}
}

func TestClassify_Set_RejectsUnknownLevel(t *testing.T) {
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1")
	_, errb, exit := runCmd(t,
		"classify", "set", "db1", "top-secret",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (Misuse)", exit)
	}
	if !strings.Contains(errb, "usage.bad_classification") {
		t.Errorf("expected usage.bad_classification:\n%s", errb)
	}
	// Allowed list must be in the error so the operator sees what's valid.
	for _, want := range []string{"public", "internal", "confidential", "restricted"} {
		if !strings.Contains(errb, want) {
			t.Errorf("error should list %q as a valid level:\n%s", want, errb)
		}
	}
}

func TestClassify_Set_NoSuchDeployment(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t,
		"classify", "set", "ghost", "confidential",
		"--output", "json",
	)
	if exit != 6 {
		t.Errorf("missing deployment should exit 6 (NotFound); got %d", exit)
	}
}

func TestClassify_Set_NormalisesCase(t *testing.T) {
	// `RESTRICTED` and `Restricted` etc must all normalise to "restricted".
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1")
	for _, in := range []string{"RESTRICTED", "Restricted", "  restricted  "} {
		out, _, exit := runCmd(t,
			"classify", "set", "db1", in,
			"--output", "json",
		)
		if exit != 0 {
			t.Errorf("input %q: exit=%d", in, exit)
		}
		if !strings.Contains(out, `"current": "restricted"`) {
			t.Errorf("input %q: should normalise to restricted:\n%s", in, out)
		}
	}
}

func TestClassify_List_DefaultsToInternal(t *testing.T) {
	// Deployment with no explicit classification should appear with
	// classification="internal" and explicit=false.
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1", "db2")
	out, _, exit := runCmd(t, "classify", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"deployment": "db2"`,
		`"classification": "internal"`,
		`"explicit": false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestClassify_List_SortsBySensitivityDescending(t *testing.T) {
	// Operators auditing "what's restricted?" want those at the top.
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1", "db2", "db3")
	_, _, _ = runCmd(t, "classify", "set", "db1", "public", "--output", "json")
	_, _, _ = runCmd(t, "classify", "set", "db3", "restricted", "--output", "json")
	// db2 stays default = internal

	out, _, exit := runCmd(t, "classify", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	// Find positions of each deployment in the JSON array.
	posDB1 := strings.Index(out, `"deployment": "db1"`)
	posDB2 := strings.Index(out, `"deployment": "db2"`)
	posDB3 := strings.Index(out, `"deployment": "db3"`)
	if posDB1 < 0 || posDB2 < 0 || posDB3 < 0 {
		t.Fatalf("not all deployments appear:\n%s", out)
	}
	// Order by sensitivity descending: db3 (restricted) → db2 (internal) → db1 (public).
	if !(posDB3 < posDB2 && posDB2 < posDB1) {
		t.Errorf("expected restricted > internal > public order; got positions: db1=%d db2=%d db3=%d\n%s",
			posDB1, posDB2, posDB3, out)
	}
}

func TestClassify_List_Empty(t *testing.T) {
	configDir(t)
	out, _, exit := runCmd(t, "classify", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"count": 0`) {
		t.Errorf("empty config should report count: 0:\n%s", out)
	}
}

// Hand-edited YAML can introduce typo'd classifications. The CLI's
// `classify set` rejects bad input at write time, but a YAML edit
// or a future-version tag we don't recognize bypasses that gate.
//
// Pre-fix: classOrder["typo"] returned 0 (Go zero value), tying with
// ClassPublic — so unknowns silently sorted as least-sensitive,
// the OPPOSITE of safe defensive behavior for a compliance feature.
//
// Post-fix: unknowns get classRank == len(classOrder), sorting them
// ABOVE every known level. The result body's `valid` field also
// distinguishes them from legitimate values for monitoring tooling.
func TestClassify_List_UnknownValueSurfacedAndSortsToTop(t *testing.T) {
	dir := configDir(t)
	body := `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    classification: public
  db2:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    classification: top-secret
  db3:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    classification: restricted
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "classify", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	// db2 has the unknown value. The result must mark it valid=false.
	for _, want := range []string{
		`"deployment": "db2"`,
		`"classification": "top-secret"`,
		`"valid": false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// db1 and db3 have legitimate values — they should be valid=true.
	if strings.Count(out, `"valid": true`) < 2 {
		t.Errorf("db1 + db3 should be valid; got:\n%s", out)
	}
	// Sort order: db2 (unknown, defensive top) then db3 (restricted)
	// then db1 (public). Pre-fix this would have been db3, db1, db2
	// (with db2 silently treated as public).
	posDB1 := strings.Index(out, `"deployment": "db1"`)
	posDB2 := strings.Index(out, `"deployment": "db2"`)
	posDB3 := strings.Index(out, `"deployment": "db3"`)
	if posDB1 < 0 || posDB2 < 0 || posDB3 < 0 {
		t.Fatalf("missing deployments:\n%s", out)
	}
	if !(posDB2 < posDB3 && posDB3 < posDB1) {
		t.Errorf("expected unknown > restricted > public order; got positions: db1=%d db2=%d db3=%d\n%s",
			posDB1, posDB2, posDB3, out)
	}
}

// `deployment list` should surface the classification too — the
// operator inspecting per-deployment state shouldn't have to bounce
// to a separate command for compliance posture.
func TestDeployment_List_SurfacesClassification(t *testing.T) {
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1")
	_, _, _ = runCmd(t, "classify", "set", "db1", "restricted", "--output", "json")

	out, _, exit := runCmd(t, "deployment", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"classification": "restricted"`) {
		t.Errorf("deployment list should expose classification:\n%s", out)
	}
}

// doctor must report the per-deployment classification map so the
// operator's "what's my compliance posture?" question has a single
// canonical answer.
func TestDoctor_ReportsClassifications(t *testing.T) {
	dir := configDir(t)
	seedClassifyConfig(t, dir, "db1", "db2")
	_, _, _ = runCmd(t, "classify", "set", "db1", "confidential", "--output", "json")
	_, _, _ = runCmd(t, "classify", "set", "db2", "restricted", "--output", "json")

	out, _, exit := runCmd(t, "doctor", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"classifications"`) {
		t.Errorf("doctor JSON should expose `classifications` map:\n%s", out)
	}
	for _, want := range []string{
		`"db1": "confidential"`,
		`"db2": "restricted"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor should report %q:\n%s", want, out)
		}
	}
}

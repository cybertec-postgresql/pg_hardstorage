package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedRunbookConfig(t *testing.T, dir, deployment string) {
	t.Helper()
	body := `schema: pg_hardstorage.config.v1
deployments:
  ` + deployment + `:
    pg_connection: postgres://pgbackup:secret@db1.example.com/postgres
    repo: file:///var/lib/pg_hardstorage/repo
    tenant: prod
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunbook_List_EnumeratesScenarios(t *testing.T) {
	configDir(t)
	out, _, exit := runCmd(t, "runbook", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	// Every scenario the SPEC commits to v0.1 must appear.
	for _, want := range []string{
		`"corruption"`, `"dr"`, `"failover"`,
		`"kms-loss"`, `"repo-loss"`, `"upgrade"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing scenario %q:\n%s", want, out)
		}
	}
	// SPEC R-references (R1..R7) must be present for the disaster
	// scenarios so the runbook doc references stay traceable.
	for _, want := range []string{`"R1"`, `"R2"`, `"R3"`, `"R4"`, `"R6"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing SPEC ref %q:\n%s", want, out)
		}
	}
}

func TestRunbook_Generate_RequiresScenario(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	_, _, exit := runCmd(t, "runbook", "generate", "db1", "--output", "json")
	if exit != 2 {
		t.Errorf("missing --scenario should exit 2; got %d", exit)
	}
}

func TestRunbook_Generate_RejectsUnknownScenario(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	_, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "rugby", "--output", "json")
	if exit != 2 {
		t.Errorf("unknown scenario should exit 2 (Misuse); got %d", exit)
	}
}

func TestRunbook_Generate_NoSuchDeployment(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t, "runbook", "generate", "ghost",
		"--scenario", "dr", "--output", "json")
	if exit != 6 {
		t.Errorf("missing deployment should exit 6 (NotFound); got %d", exit)
	}
}

func TestRunbook_Generate_HappyPath_DR(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	out, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "dr", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	// Result body should carry the rendered Markdown.
	for _, want := range []string{
		`"scenario": "dr"`,
		`"deployment": "db1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in result body:\n%s", want, out)
		}
	}
	// The deployment name must appear in the rendered Markdown
	// so the operator can copy-paste without further substitution.
	if !strings.Contains(out, "db1") {
		t.Errorf("rendered Markdown should mention the deployment:\n%s", out)
	}
}

// The redaction we documented for `deployment list` must also apply
// here — runbooks are likely to be pasted into incident channels.
// Templates use {{ .PGConnection }} after redactDSN.
func TestRunbook_Generate_RedactsPasswords(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	out, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "failover", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("password leaked into runbook body:\n%s", out)
	}
}

func TestRunbook_Generate_AllScenariosRender(t *testing.T) {
	// Regression guard: every scenario in the catalog must template-
	// render cleanly given a minimal deployment. A typo in any
	// template (unknown variable, malformed syntax) would surface
	// as an internal error here.
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	for _, scenario := range []string{
		"corruption", "dr", "failover", "kms-loss", "repo-loss", "upgrade",
	} {
		t.Run(scenario, func(t *testing.T) {
			out, _, exit := runCmd(t, "runbook", "generate", "db1",
				"--scenario", scenario, "--output", "json")
			if exit != 0 {
				t.Errorf("scenario %q failed to render: exit=%d\n%s", scenario, exit, out)
			}
			if !strings.Contains(out, scenario) {
				t.Errorf("scenario %q's name missing from rendered body", scenario)
			}
		})
	}
}

// `--repo` overrides the deployment's configured repo so the operator
// can generate an "after switching to replica" runbook.
func TestRunbook_Generate_RepoOverride(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	out, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "dr",
		"--repo", "s3://failover-bucket/",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, "s3://failover-bucket/") {
		t.Errorf("--repo override should appear in rendered runbook:\n%s", out)
	}
	if strings.Contains(out, "file:///var/lib/pg_hardstorage/repo") {
		t.Errorf("default repo should NOT appear when --repo overrides:\n%s", out)
	}
}

func TestRunbook_Generate_TextRendererEmitsBareMarkdown(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	out, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "corruption", "--output", "text")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	// Text mode should produce the markdown directly, not the JSON
	// wrapper. So the first non-blank chars should be the H1 marker.
	trimmed := strings.TrimLeft(out, " \t\r\n")
	if !strings.HasPrefix(trimmed, "# Runbook:") {
		t.Errorf("text mode should emit bare Markdown starting with '# Runbook:'; got prefix: %q",
			firstLine(trimmed))
	}
}

// Empty Tenant in config means "default" per the single-org user
// model. Without substitution, the kms-loss template rendered the
// awkward `Backups for tenant **** can no longer be decrypted.` —
// bold formatting around an empty string. The fix substitutes
// "default" before render time.
func TestRunbook_Generate_EmptyTenant_RendersAsDefault(t *testing.T) {
	dir := configDir(t)
	// Note: pg_connection includes a colon-form password for the
	// redact regression to also fire on this path; tenant explicitly
	// omitted so we exercise the empty-tenant case.
	body := `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://u:p@h/db
    repo: file:///tmp/r
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "kms-loss", "--output", "text")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if strings.Contains(out, "tenant ****") {
		t.Errorf("empty Tenant must NOT render as bold-empty `tenant ****`:\n%s", out)
	}
	if !strings.Contains(out, "tenant **default**") {
		t.Errorf("empty Tenant should substitute to **default**; got:\n%s", out)
	}
}

// The runbook body's GeneratedAt MUST equal the timestamp embedded
// in the rendered Markdown — both come from the same time.Now() in
// the fix. Two independent calls would diverge by microseconds, so
// a JSON consumer reading body.generated_at would see a stamp that
// doesn't appear anywhere in body.markdown. (The outer Result
// envelope's `generated_at` is dispatcher-set and intentionally
// independent — not part of this consistency invariant.)
func TestRunbook_Generate_TimestampConsistency(t *testing.T) {
	dir := configDir(t)
	seedRunbookConfig(t, dir, "db1")
	stdout, _, exit := runCmd(t, "runbook", "generate", "db1",
		"--scenario", "dr", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		GeneratedAt string `json:"generated_at"`
		Markdown    string `json:"markdown"`
	}
	bodyOf(t, stdout, &view)

	if view.GeneratedAt == "" {
		t.Fatalf("body.generated_at is empty:\n%s", stdout)
	}
	// The Markdown contains "Generated <TS> for deployment ...".
	// The TS must match body.generated_at exactly — same now().
	marker := "Generated "
	idx := strings.Index(view.Markdown, marker)
	if idx < 0 {
		t.Fatalf("markdown does not contain %q:\n%s", marker, view.Markdown)
	}
	rest := view.Markdown[idx+len(marker):]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		t.Fatalf("could not find end-of-timestamp in markdown")
	}
	mdStamp := rest[:end]
	if mdStamp != view.GeneratedAt {
		t.Errorf("body.generated_at %q != markdown timestamp %q (must be the same now())",
			view.GeneratedAt, mdStamp)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

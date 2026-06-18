package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployment_Add_RequiresFlags(t *testing.T) {
	configDir(t)
	for _, args := range [][]string{
		{"deployment", "add", "db1", "--output", "json"},
		{"deployment", "add", "db1", "--repo", "file:///x", "--output", "json"},
		{"deployment", "add", "db1", "--connection", "postgres://x", "--output", "json"},
	} {
		_, _, exit := runCmd(t, args...)
		if exit != 2 {
			t.Errorf("args=%v should exit 2 (Misuse); got %d", args, exit)
		}
	}
}

func TestDeployment_Add_SkipProbe_Writes(t *testing.T) {
	dir := configDir(t)
	_, _, exit := runCmd(t,
		"deployment", "add", "db1",
		"--connection", "postgres://x@h/db",
		"--repo", "file:///tmp/x",
		"--schedule-backup", "every 6h",
		"--schedule-rotate", "daily_at 04:00",
		"--skip-probe",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	for _, want := range []string{
		"db1:",
		"pg_connection: postgres://x@h/db",
		"repo: file:///tmp/x",
		"every: 6h",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config missing %q:\n%s", want, body)
		}
	}
}

func TestDeployment_Add_DuplicateRequiresYes(t *testing.T) {
	dir := configDir(t)
	add := func(extra ...string) int {
		args := append([]string{
			"deployment", "add", "db1",
			"--connection", "postgres://x",
			"--repo", "file:///tmp/x",
			"--skip-probe",
			"--output", "json",
		}, extra...)
		_, _, exit := runCmd(t, args...)
		return exit
	}
	if exit := add(); exit != 0 {
		t.Fatalf("first add: exit = %d", exit)
	}
	if exit := add(); exit != 7 { // ExitConflict
		t.Errorf("duplicate add (no --yes) should exit 7; got %d", exit)
	}
	if exit := add("--yes"); exit != 0 {
		t.Errorf("duplicate with --yes should succeed; got %d", exit)
	}
	_ = dir
}

func TestDeployment_List_RedactsPassword(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://user:supersecret@host/db
    repo: file:///tmp/x
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "deployment", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if strings.Contains(out, "supersecret") {
		t.Errorf("password leaked into list output:\n%s", out)
	}
	if !strings.Contains(out, "user:****@host") {
		t.Errorf("password should be redacted as ****; got:\n%s", out)
	}
}

// libpq DSN strings come in two forms — URI and keyword. The list
// path must redact both; otherwise `deployment list` leaks credentials
// for keyword-style DSNs.
func TestDeployment_List_RedactsKeywordForm(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: "host=db1 user=u password=topsecret dbname=app"
    repo: file:///tmp/x
  db2:
    pg_connection: "host=db2 password='quoted secret' user=u"
    repo: file:///tmp/y
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "deployment", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, leak := range []string{"topsecret", "quoted secret"} {
		if strings.Contains(out, leak) {
			t.Errorf("keyword-form password %q leaked:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "password=****") {
		t.Errorf("keyword-form password should be redacted as ****; got:\n%s", out)
	}
}

func TestDeployment_Remove_RequiresYes(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1: { pg_connection: postgres://x, repo: file:///tmp/x }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, exit := runCmd(t, "deployment", "remove", "db1", "--output", "json")
	if exit != 5 { // ExitAborted
		t.Errorf("remove without --yes should exit 5 (Aborted); got %d", exit)
	}
	_, _, exit = runCmd(t, "deployment", "remove", "db1", "--yes", "--output", "json")
	if exit != 0 {
		t.Errorf("remove with --yes should succeed; got %d", exit)
	}
}

func TestDeployment_Remove_NonExistent_NotFound(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t, "deployment", "remove", "ghost", "--yes", "--output", "json")
	if exit != 6 {
		t.Errorf("missing deployment should exit 6 (NotFound); got %d", exit)
	}
}

func TestDeployment_Edit_OnlyTouchesPassedFlags(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    tenant: legacy
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, exit := runCmd(t,
		"deployment", "edit", "db1",
		"--repo", "file:///new/path",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	got := string(body)
	if !strings.Contains(got, "repo: file:///new/path") {
		t.Errorf("repo not updated\n%s", got)
	}
	if !strings.Contains(got, "tenant: legacy") {
		t.Errorf("tenant should be preserved (not passed):\n%s", got)
	}
	if !strings.Contains(got, "every: 6h") {
		t.Errorf("schedule should be preserved (not passed):\n%s", got)
	}
}

func TestDeployment_Test_NoSuchDeployment(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t, "deployment", "test", "ghost", "--output", "json")
	if exit != 6 {
		t.Errorf("missing deployment should exit 6 (NotFound); got %d", exit)
	}
}

// Issue #75: when a deployment has a patroni: block configured in the
// YAML, `deployment list` should render every documented field. Before
// the fix the block was silently dropped from the output.
func TestDeployment_List_RendersPatroniBlock(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  patroni-pg:
    pg_connection: postgres://postgres:secret@haproxy:5000/postgres
    repo: file:////var/lib/hardstorage
    patroni:
      url: http://haproxy:8008
      slot: pg_hardstorage_patroni
      interval: 5s
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Text mode renders each field on its own line.
	out, _, exit := runCmd(t, "deployment", "list", "--output", "text")
	if exit != 0 {
		t.Fatalf("exit = %d; out=%s", exit, out)
	}
	for _, want := range []string{
		"patroni-url: http://haproxy:8008",
		"slot:        pg_hardstorage_patroni",
		"interval:    5s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}

	// JSON mode exposes the structured fields.
	out, _, exit = runCmd(t, "deployment", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("json exit = %d", exit)
	}
	for _, want := range []string{
		`"url":"http://haproxy:8008"`,
		`"slot":"pg_hardstorage_patroni"`,
		`"interval":"5s"`,
	} {
		// Strip whitespace for the substring match — encoding/json may
		// insert spaces depending on the encoder used.
		stripped := strings.ReplaceAll(strings.ReplaceAll(out, " ", ""), "\n", "")
		if !strings.Contains(stripped, want) {
			t.Errorf("json output missing %q:\n%s", want, out)
		}
	}
}

// The multi-slot form must render too. The single-slot Slot field is
// empty in this case; the multi-slot Slots[] is what's set.
func TestDeployment_List_RendersPatroniMultiSlot(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    patroni:
      url: http://patroni:8008
      slots:
        - { name: pg_hardstorage_db1_primary, role: leader }
        - { name: pg_hardstorage_db1_replica, role: replica }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "deployment", "list", "--output", "text")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"patroni-url: http://patroni:8008",
		"slot:        pg_hardstorage_db1_primary (leader)",
		"slot:        pg_hardstorage_db1_replica (replica)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-slot output missing %q:\n%s", want, out)
		}
	}
}

// A deployment without a patroni: block must not emit any Patroni lines —
// regression guard against the "always rendered, looks empty" footgun.
func TestDeployment_List_OmitsPatroniWhenAbsent(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "deployment", "list", "--output", "text")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, banned := range []string{"patroni-url:", "slot:", "interval:"} {
		if strings.Contains(out, banned) {
			t.Errorf("non-patroni deployment leaked %q:\n%s", banned, out)
		}
	}
}

// `deployment edit --patroni-url ... --patroni-slot ... --patroni-interval ...`
// must write the block into pg_hardstorage.yaml and leave the rest of
// the deployment untouched.
func TestDeployment_Edit_WritesPatroniFields(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  patroni-pg:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, exit := runCmd(t,
		"deployment", "edit", "patroni-pg",
		"--patroni-url", "http://haproxy:8008",
		"--patroni-slot", "pg_hardstorage_patroni",
		"--patroni-interval", "5s",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	got := string(body)
	for _, want := range []string{
		"url: http://haproxy:8008",
		"slot: pg_hardstorage_patroni",
		"interval: 5s",
		// Everything else preserved.
		"pg_connection: postgres://x@h/db",
		"every: 6h",
		"daily_at: \"04:00\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("edit output missing %q:\n%s", want, got)
		}
	}
}

// Editing only one Patroni field (e.g. interval) must leave the others
// alone — same merge semantics as the existing connection/repo edits.
func TestDeployment_Edit_PatroniPartialUpdate(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    patroni:
      url: http://patroni:8008
      slot: pg_hardstorage_db1
      interval: 5s
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, exit := runCmd(t,
		"deployment", "edit", "db1",
		"--patroni-interval", "10s",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	for _, want := range []string{
		"url: http://patroni:8008", // preserved
		"slot: pg_hardstorage_db1", // preserved
		"interval: 10s",            // updated
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("partial edit missing %q:\n%s", want, got)
		}
	}
}

// Clearing the URL must wipe the whole block so we don't leave a stale
// slot/interval that no longer corresponds to any reachable Patroni.
func TestDeployment_Edit_PatroniURLEmptyClearsBlock(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    patroni:
      url: http://patroni:8008
      slot: pg_hardstorage_db1
      interval: 5s
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, exit := runCmd(t,
		"deployment", "edit", "db1",
		"--patroni-url", "",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	for _, banned := range []string{
		"url: http://patroni:8008",
		"slot: pg_hardstorage_db1",
		"interval: 5s",
	} {
		if strings.Contains(string(got), banned) {
			t.Errorf("clearing --patroni-url should have dropped %q:\n%s", banned, got)
		}
	}
}

// Operator error: --patroni-slot or --patroni-interval without a URL
// would silently land in the YAML but never take effect (IsEnabled()
// returns false). Refuse with exit 2 (Misuse) and a remediation hint.
func TestDeployment_Edit_PatroniSlotWithoutURL_Rejected(t *testing.T) {
	dir := configDir(t)
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, exit := runCmd(t,
		"deployment", "edit", "db1",
		"--patroni-slot", "pg_hardstorage_db1",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("slot-without-url should exit 2 (Misuse); got %d", exit)
	}
}

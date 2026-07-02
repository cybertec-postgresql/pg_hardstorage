package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// llmXDGHome wires a per-test HOME + XDG dirs and returns the
// pg_hardstorage config dir under it.  Mirrors airgap_test.go's
// setup so config-file-driven behaviour resolves against an
// isolated tree.
func llmXDGHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	cfgDir := filepath.Join(home, ".config", "pg_hardstorage")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfgDir
}

func writeLLMConfig(t *testing.T, cfgDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- bug 7 + bug 6: privacy endpoint resolution honours env vars ---

// TestLlmAsk_LocalOnlyRefusesEnvPublicEndpoint proves runLlmAsk now
// enforces llm.privacy (bug 7) AND resolves the endpoint through the
// env-var precedence chain (bug 6 shares the same resolution logic):
// with privacy=local-only and PG_HARDSTORAGE_URL pointing at a public
// host, the ask path must refuse to egress.  Pre-fix `llm ask` built
// the session without Privacy/PrivacyEndpoint so this call succeeded
// and data left the host.
func TestLlmAsk_LocalOnlyRefusesEnvPublicEndpoint(t *testing.T) {
	cfgDir := llmXDGHome(t)
	writeLLMConfig(t, cfgDir, `schema: pg_hardstorage.config.v1
llm:
  privacy: local-only
`)
	// Endpoint arrives ONLY via the env var — the yaml has none.
	// The flag->yaml-only resolution the bug describes would miss
	// this and wrongly allow egress.
	t.Setenv("PG_HARDSTORAGE_URL", "https://api.openai.com/v1")

	_, stderr, exit := runCLI(t, "llm", "ask", "hello", "--provider", "mock", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("local-only ask against a public env endpoint should refuse; exit=%d", exit)
	}
	if !strings.Contains(stderr, "local-only") {
		t.Errorf("expected a local-only refusal; stderr=%s", stderr)
	}
}

// TestLlmAsk_LocalOnlyAllowsEnvLocalEndpoint proves the mirror case:
// a loopback endpoint supplied via env passes the gate, so `llm ask`
// still works under local-only when pointed at a local runtime.
func TestLlmAsk_LocalOnlyAllowsEnvLocalEndpoint(t *testing.T) {
	cfgDir := llmXDGHome(t)
	writeLLMConfig(t, cfgDir, `schema: pg_hardstorage.config.v1
llm:
  privacy: local-only
`)
	t.Setenv("PG_HARDSTORAGE_URL", "http://127.0.0.1:11434/v1")

	_, stderr, exit := runCLI(t, "llm", "ask", "hello", "--provider", "mock", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("local-only ask against a loopback env endpoint should succeed; exit=%d stderr=%s", exit, stderr)
	}
}

// --- bug 69: REPL surfaces a scanner read error / oversized line ---

// TestLlmChat_OversizedLineSurfacesError feeds the REPL a single
// line larger than the 1 MiB scanner buffer.  bufio.Scanner returns
// ErrTooLong from Scan(), which pre-fix looked identical to a clean
// EOF (Ctrl-D) and exited 0 silently.  Post-fix the REPL checks
// scanner.Err() and surfaces a non-zero exit + a read-failure code.
func TestLlmChat_OversizedLineSurfacesError(t *testing.T) {
	llmXDGHome(t)
	// 2 MiB with no newline: exceeds the 1<<20 (1 MiB) buffer cap.
	big := strings.Repeat("x", 2*1024*1024)

	root := cli.NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(big))
	root.SetArgs([]string{"llm", "chat", "--provider", "mock"})
	exit := cli.Run(root)

	if exit == int(output.ExitOK) {
		t.Fatalf("oversized input line should not exit cleanly; out=%s", out.String())
	}
	if !strings.Contains(out.String(), "llm.chat_read_failed") &&
		!strings.Contains(out.String(), "read input") {
		t.Errorf("expected a surfaced read-failure; out=%s", out.String())
	}
}

// --- bug 42: doctor exits non-zero when a check fails ---

// TestLlmDoctor_ExitsNonZeroOnFailure drives `llm doctor` with no
// provider configured anywhere (no key, no --provider).  The
// provider-configuration check fails, so the command MUST exit
// non-zero (help promises this).  Pre-fix the RunE returned a nil
// error and exit was 0 despite the ✗ row.
func TestLlmDoctor_ExitsNonZeroOnFailure(t *testing.T) {
	llmXDGHome(t)
	// Scrub every key source so resolveLlmProviderFull refuses.
	t.Setenv("PG_HARDSTORAGE_LLM_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_PROVIDER", "")

	stdout, stderr, exit := runCLI(t, "llm", "doctor", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("doctor with a failing check must exit non-zero; exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	// The structured report is still emitted so the operator sees ✗ rows.
	if !strings.Contains(stdout, `"ok": false`) {
		t.Errorf("doctor report should carry ok=false; stdout=%s", stdout)
	}
}

// --- bug 68: doctor honours the inherited --provider flag ---

// TestLlmDoctor_HonoursProviderFlag passes --provider mock (a
// persistent flag inherited from the parent `llm` command).  Pre-fix
// runLlmDoctor called resolveLlmProviderFull("","","") and ignored
// the flag, so it would refuse (no key) instead of opening mock.
func TestLlmDoctor_HonoursProviderFlag(t *testing.T) {
	llmXDGHome(t)
	t.Setenv("PG_HARDSTORAGE_LLM_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_PROVIDER", "")

	stdout, _, _ := runCLI(t, "llm", "doctor", "--provider", "mock", "-o", "json")
	if !strings.Contains(stdout, `"provider": "mock"`) {
		t.Errorf("doctor should report the flag-supplied provider=mock; stdout=%s", stdout)
	}
	// The provider-configuration check must pass (mock opens cleanly),
	// which it can't if the flag was ignored and resolution refused.
	if !strings.Contains(stdout, "opened cleanly") {
		t.Errorf("mock provider should open cleanly under doctor; stdout=%s", stdout)
	}
}

// --- bug 67: validator-catch detail shows the broken message ---

// TestLlmDoctor_ValidatorCheckPasses exercises the doctor's
// validator check.  The planted flag IS caught by the live cobra
// tree, so verr != nil and the detail is the "validator said: ..."
// form — never the "<nil>"-yielding Sprintf that bug 67 fixed.  We
// assert the broken-marker text never surfaces on a healthy tree.
func TestLlmDoctor_ValidatorCheckPasses(t *testing.T) {
	llmXDGHome(t)
	stdout, _, _ := runCLI(t, "llm", "doctor", "--provider", "mock", "-o", "json")
	if strings.Contains(stdout, "did NOT catch the planted flag") {
		t.Errorf("validator caught the planted flag; the broken-marker must not appear; stdout=%s", stdout)
	}
	if !strings.Contains(stdout, "validator said:") {
		t.Errorf("expected the 'validator said:' detail when the flag is caught; stdout=%s", stdout)
	}
}

// --- bug 43: `--principal '*'` enumerates every principal ---

// TestLlmHistory_WildcardEnumeratesAllPrincipals writes a session
// under a NON-$USER principal, then lists with --principal '*'.  The
// wildcard must surface that other principal's session.  Pre-fix the
// wildcard mapped to "" which sanitised to the "_default" directory
// (never written), so the listing came back empty.
func TestLlmHistory_WildcardEnumeratesAllPrincipals(t *testing.T) {
	llmXDGHome(t)
	t.Setenv("USER", "alice")

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := keystore.LoadOrGenerateKEK(p.Keyring.Value); err != nil {
		t.Fatal(err)
	}

	// Seed a session under principal "bob" by writing the .meta.json
	// sidecar the way the history Writer would — listing reads only
	// the unencrypted meta, so no DEK / body is needed.
	convRoot := filepath.Join(p.State.Value, "llm", "conversations")
	bobDir := filepath.Join(convRoot, "bob", "_default")
	if err := os.MkdirAll(bobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"session_id": "20260101T000000-deadbeef",
		"principal":  "bob",
		"skill":      "ask",
		"started_at": "2026-01-01T00:00:00Z",
		"ended_at":   "2026-01-01T00:01:00Z",
		"entries":    1,
		"bytes":      42,
	}
	metaBytes, _ := stdjson.Marshal(meta)
	if err := os.WriteFile(filepath.Join(bobDir, "20260101T000000-deadbeef.meta.json"), metaBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Scoped to alice ($USER): must NOT see bob's session.
	stdout, stderr, exit := runCLI(t, "llm", "history", "list", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("scoped list exit=%d stderr=%s", exit, stderr)
	}
	if strings.Contains(stdout, "bob") {
		t.Errorf("alice's scoped listing must not include bob; stdout=%s", stdout)
	}

	// Wildcard: MUST enumerate bob.
	stdout, stderr, exit = runCLI(t, "llm", "history", "list", "--principal", "*", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("wildcard list exit=%d stderr=%s", exit, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unwrap: %v\n%s", err, stdout)
	}
	body := res.Result.(map[string]any)
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("wildcard should list bob's 1 session; got %d\n%s", len(sessions), stdout)
	}
	first := sessions[0].(map[string]any)
	if first["principal"] != "bob" {
		t.Errorf("wildcard session principal = %v, want bob", first["principal"])
	}
}

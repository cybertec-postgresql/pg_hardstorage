package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// withStubRepoResolver disables the config-based resolver for the
// duration of a test, so livestate_test.go can assert the exact
// CLI args the tools shell out without needing a configured repo
// in the test environment.  Returns a cleanup the caller defers.
func withStubRepoResolver(t *testing.T, stub func(string) string) {
	t.Helper()
	prev := repoResolver
	if stub == nil {
		repoResolver = func(string) string { return "" }
	} else {
		repoResolver = stub
	}
	t.Cleanup(func() { repoResolver = prev })
}

// stubRunner records the args of every invocation and returns
// canned responses keyed by the first arg (the subcommand).
type stubRunner struct {
	calls    [][]string
	stdout   map[string][]byte
	exitCode map[string]int
}

func (s *stubRunner) run(_ context.Context, args []string) ([]byte, []byte, int, error) {
	s.calls = append(s.calls, args)
	if len(args) == 0 {
		return nil, []byte("no args"), 1, nil
	}
	key := args[0]
	if v, ok := s.stdout[key]; ok {
		code := s.exitCode[key]
		return v, nil, code, nil
	}
	return nil, []byte("no canned response for " + key), 1, nil
}

func newStubbedRunner(canned map[string][]byte) *CLIRunner {
	s := &stubRunner{stdout: canned, exitCode: map[string]int{}}
	return &CLIRunner{
		Path:   "/usr/local/bin/pg_hardstorage", // placeholder; not used with Runner override
		Runner: s.run,
	}
}

func TestRegisterCoreTools_RegistersExpectedSet(t *testing.T) {
	reg := NewRegistry()
	r := newStubbedRunner(nil)
	RegisterCoreTools(reg, r)
	want := []string{
		"read_doctor", "read_status", "list_deployments", "list_backups",
		"read_backup", "read_repo_usage", "read_audit",
		"list_runbooks", "search_docs",
	}
	for _, n := range want {
		if _, err := reg.Get(n); err != nil {
			t.Errorf("expected tool %q in registry: %v", n, err)
		}
	}
}

func TestReadDoctor_Run(t *testing.T) {
	r := newStubbedRunner(map[string][]byte{
		"doctor": []byte(`{"schema":"pg_hardstorage.v1","result":{"healthy":true,"deployments":[]}}`),
	})
	tool := &readDoctor{runner: r}
	got, err := tool.Run(context.Background(), map[string]any{"deployment": "db1"})
	if err != nil {
		t.Fatal(err)
	}
	// Pilot run 20260514T085557Z case C1: read_doctor was passing
	// `-d <deployment>` which the doctor command rejects ("unknown
	// shorthand flag: 'd' in -d").  doctor takes <deployment> as
	// a POSITIONAL.  Assert the args we shell out match.
	if got.Body == nil {
		t.Fatal("nil body")
	}
	if got.Summary == "" {
		t.Error("Summary should be set")
	}
	body, ok := got.Body.(map[string]any)
	if !ok {
		t.Fatalf("Body should decode to map; got %T", got.Body)
	}
	if _, has := body["result"]; !has {
		t.Errorf("decoded body should preserve outer envelope; got keys %v", keysOf(body))
	}
}

func TestReadDoctor_NonZeroExitStillSurfacesBody(t *testing.T) {
	// doctor exits non-zero (10) when issues are present.  The tool
	// must still return the body so the LLM can read the issues.
	r := &CLIRunner{
		Path: "/usr/local/bin/pg_hardstorage",
		Runner: func(_ context.Context, _ []string) ([]byte, []byte, int, error) {
			return []byte(`{"schema":"pg_hardstorage.v1","result":{"healthy":false,"issues":[{"code":"wal.lag"}]}}`), nil, 10, nil
		},
	}
	tool := &readDoctor{runner: r}
	got, err := tool.Run(context.Background(), nil)
	// Non-zero exit on doctor is "issues present", not a tool error.
	if err != nil {
		t.Fatalf("tool should swallow doctor's exit-on-issues; got %v", err)
	}
	if !strings.Contains(got.Summary, "issues") {
		t.Errorf("Summary should mention issues; got %q", got.Summary)
	}
}

func TestReadStatus_PassesDeployment(t *testing.T) {
	withStubRepoResolver(t, nil) // no config in test env; status should run without --repo
	r := newStubbedRunner(map[string][]byte{
		"status": []byte(`{"schema":"pg_hardstorage.v1","result":{"deployments":[{"name":"db1"}]}}`),
	})
	tool := &readStatus{runner: r}
	if _, err := tool.Run(context.Background(), map[string]any{"deployment": "db1"}); err != nil {
		t.Fatal(err)
	}
	// Verify the args passed to the stub by re-running the tool against a
	// fresh stub that captures the call.
	captured := &stubRunner{stdout: map[string][]byte{
		"status": []byte(`{"result":{}}`),
	}, exitCode: map[string]int{}}
	r2 := &CLIRunner{Path: "/x", Runner: captured.run}
	tool2 := &readStatus{runner: r2}
	if _, err := tool2.Run(context.Background(), map[string]any{"deployment": "db7"}); err != nil {
		t.Fatal(err)
	}
	if len(captured.calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(captured.calls))
	}
	args := captured.calls[0]
	want := []string{"status", "db7", "-o", "json"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestReadBackup_RequiresDeploymentAndID(t *testing.T) {
	r := newStubbedRunner(nil)
	tool := &readBackup{runner: r}
	if _, err := tool.Run(context.Background(), map[string]any{"deployment": "db1"}); err == nil {
		t.Error("missing backup_id: expected error")
	}
	if _, err := tool.Run(context.Background(), map[string]any{"backup_id": "x"}); err == nil {
		t.Error("missing deployment: expected error")
	}
}

func TestListBackups_PassesRepoFlag(t *testing.T) {
	captured := &stubRunner{stdout: map[string][]byte{
		"list": []byte(`{"result":{}}`),
	}, exitCode: map[string]int{}}
	r := &CLIRunner{Path: "/x", Runner: captured.run}
	tool := &listBackups{runner: r}
	if _, err := tool.Run(context.Background(), map[string]any{
		"deployment": "db1",
		"repo":       "s3://acme/",
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"list", "db1", "--repo", "s3://acme/", "-o", "json"}
	if !equalStrings(captured.calls[0], want) {
		t.Errorf("args = %v, want %v", captured.calls[0], want)
	}
}

func TestReadAudit_RequiresRepo(t *testing.T) {
	tool := &readAudit{runner: newStubbedRunner(nil)}
	if _, err := tool.Run(context.Background(), nil); err == nil {
		t.Error("missing repo: expected error")
	}
}

func TestReadAudit_PassesFilters(t *testing.T) {
	captured := &stubRunner{stdout: map[string][]byte{
		"audit": []byte(`{"result":{"events":[]}}`),
	}, exitCode: map[string]int{}}
	r := &CLIRunner{Path: "/x", Runner: captured.run}
	tool := &readAudit{runner: r}
	_, err := tool.Run(context.Background(), map[string]any{
		"repo":       "s3://acme/",
		"action":     "kms.shred",
		"deployment": "db1",
		"since":      "2026-04-01",
		"limit":      float64(20),
	})
	if err != nil {
		t.Fatal(err)
	}
	args := captured.calls[0]
	wantSubstr := []string{"audit", "search", "--repo", "s3://acme/",
		"--action", "kms.shred", "--deployment", "db1",
		"--since", "2026-04-01", "--limit", "20"}
	for _, w := range wantSubstr {
		found := false
		for _, a := range args {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in args %v", w, args)
		}
	}
}

func TestListRunbooks_BundledIndex(t *testing.T) {
	tool := listRunbooks{}
	got, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := got.Body.(map[string]any)
	if !ok {
		t.Fatalf("Body type = %T, want map", got.Body)
	}
	rs, ok := body["runbooks"]
	if !ok {
		t.Fatal("runbooks key missing from body")
	}
	if rs == nil {
		t.Fatal("runbooks should be non-empty")
	}
}

func TestSearchDocs_Hits(t *testing.T) {
	tool := searchDocs{}
	got, err := tool.Run(context.Background(), map[string]any{"query": "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Summary, "matches") {
		t.Errorf("Summary should report match count; got %q", got.Summary)
	}
}

func TestSearchDocs_EmptyQuery(t *testing.T) {
	tool := searchDocs{}
	if _, err := tool.Run(context.Background(), map[string]any{"query": ""}); err == nil {
		t.Error("empty query should error")
	}
}

func TestRunJSON_RefusesMutationFlags(t *testing.T) {
	r := &CLIRunner{Path: "/x", Runner: func(_ context.Context, _ []string) ([]byte, []byte, int, error) {
		return nil, nil, 0, nil
	}}
	cases := [][]string{
		{"backup", "delete", "db1", "id-1", "--apply"},
		{"kms", "shred", "--yes"},
		{"restore", "db1", "id", "--force"},
		{"chain-restore", "--reset-chain-staging"},
	}
	for _, args := range cases {
		_, err := r.RunJSON(context.Background(), args...)
		if err == nil {
			t.Errorf("args %v: expected refusal", args)
			continue
		}
		if !strings.Contains(err.Error(), "refuse to invoke mutation flag") {
			t.Errorf("args %v: refusal message wrong: %v", args, err)
		}
	}
}

func TestRunJSON_AddsOutputJSON(t *testing.T) {
	captured := &stubRunner{stdout: map[string][]byte{
		"status": []byte(`{}`),
	}, exitCode: map[string]int{}}
	r := &CLIRunner{Path: "/x", Runner: captured.run}
	if _, err := r.RunJSON(context.Background(), "status"); err != nil {
		t.Fatal(err)
	}
	args := captured.calls[0]
	want := []string{"status", "-o", "json"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestRunJSON_PreservesExplicitOutput(t *testing.T) {
	captured := &stubRunner{stdout: map[string][]byte{
		"status": []byte(`{}`),
	}, exitCode: map[string]int{}}
	r := &CLIRunner{Path: "/x", Runner: captured.run}
	if _, err := r.RunJSON(context.Background(), "status", "-o", "yaml"); err != nil {
		t.Fatal(err)
	}
	for _, a := range captured.calls[0] {
		if a == "json" {
			t.Errorf("explicit -o yaml should be preserved; got %v", captured.calls[0])
		}
	}
}

func TestParseAsResult_NonJSONFallsBackGracefully(t *testing.T) {
	res, err := parseAsResult("test", []byte("not-json"))
	if err != nil {
		t.Fatal(err)
	}
	body, ok := res.Body.(map[string]any)
	if !ok {
		t.Fatalf("Body type = %T", res.Body)
	}
	if body["raw"] != "not-json" {
		t.Errorf("raw should preserve original body; got %v", body["raw"])
	}
	if _, ok := body["parse_error"]; !ok {
		t.Errorf("parse_error key should be set")
	}
}

func TestRunJSON_ReturnsErrNonZeroExit(t *testing.T) {
	r := &CLIRunner{Path: "/x", Runner: func(_ context.Context, _ []string) ([]byte, []byte, int, error) {
		return nil, []byte("doctor: issues"), 10, nil
	}}
	_, err := r.RunJSON(context.Background(), "doctor")
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !errors.Is(err, ErrNonZeroExit) {
		t.Errorf("err = %v, want wraps ErrNonZeroExit", err)
	}
}

// TestReadDoctor_DeploymentIsPositionalNotFlag — regression for the
// pilot-C1 bug: read_doctor was passing `-d <deployment>` which the
// underlying `pg_hardstorage doctor` rejects.  The deployment is a
// positional argument; verify the tool shells out accordingly.
func TestReadDoctor_DeploymentIsPositionalNotFlag(t *testing.T) {
	captured := &stubRunner{stdout: map[string][]byte{
		"doctor": []byte(`{"result":{}}`),
	}, exitCode: map[string]int{}}
	r := &CLIRunner{Path: "/x", Runner: captured.run}
	tool := &readDoctor{runner: r}
	if _, err := tool.Run(context.Background(), map[string]any{"deployment": "db7"}); err != nil {
		t.Fatal(err)
	}
	if len(captured.calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(captured.calls))
	}
	args := captured.calls[0]
	want := []string{"doctor", "db7", "-o", "json"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v (the bug was passing -d db7 instead of positional)", args, want)
	}
}

// TestReadStatus_ResolvesRepoFromConfig — regression for pilot-C1:
// `pg_hardstorage status` requires --repo and the tool didn't pass
// it.  The fix resolves the URL from the operator's config via
// repoResolver.  Verify the tool propagates the resolved URL.
func TestReadStatus_ResolvesRepoFromConfig(t *testing.T) {
	withStubRepoResolver(t, func(dep string) string {
		if dep == "billing" {
			return "s3://prod-billing/"
		}
		return "s3://default/"
	})
	captured := &stubRunner{stdout: map[string][]byte{
		"status": []byte(`{"result":{}}`),
	}, exitCode: map[string]int{}}
	r := &CLIRunner{Path: "/x", Runner: captured.run}
	tool := &readStatus{runner: r}

	if _, err := tool.Run(context.Background(), map[string]any{"deployment": "billing"}); err != nil {
		t.Fatal(err)
	}
	args := captured.calls[0]
	want := []string{"status", "billing", "--repo", "s3://prod-billing/", "-o", "json"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}

	// No deployment → falls back to the resolver's "" call,
	// which the stub maps to s3://default/.
	captured.calls = nil
	if _, err := tool.Run(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	args = captured.calls[0]
	want = []string{"status", "--repo", "s3://default/", "-o", "json"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}

	// Explicit repo arg wins over the resolver.
	captured.calls = nil
	if _, err := tool.Run(context.Background(), map[string]any{
		"deployment": "billing",
		"repo":       "s3://override/",
	}); err != nil {
		t.Fatal(err)
	}
	args = captured.calls[0]
	want = []string{"status", "billing", "--repo", "s3://override/", "-o", "json"}
	if !equalStrings(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// ----- helpers -----

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keysOf(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	return k
}

// Ensure the import is used in tests that decode bodies.
var _ = json.Marshal

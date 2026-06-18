package cli_test

import (
	stdjson "encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// provisionKeyring resolves the keyring dir for the current
// HOME/XDG and creates a kek.bin so the history-read commands
// can derive their DEK.  Smaller dependency surface than
// running the full `init` wizard.
func provisionKeyring(t *testing.T) {
	t.Helper()
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := keystore.LoadOrGenerateKEK(p.Keyring.Value); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Join // keep import usable
}

// TestLlmHistory_RequiresKEK: without a kek.bin the read-side
// commands surface a structured error the operator can act on.
func TestLlmHistory_RequiresKEK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "alice")

	_, stderr, exit := runCLI(t, "llm", "history", "list", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("expected refusal without KEK")
	}
	if !strings.Contains(stderr, "history.no_dek") {
		t.Errorf("expected history.no_dek; stderr=%s", stderr)
	}
}

// TestLlmHistory_EmptyListAfterInit: with a freshly-init'd
// keyring (no recorded sessions), `list` returns an empty
// `sessions` array.
func TestLlmHistory_EmptyListAfterInit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "alice")

	provisionKeyring(t)

	stdout, stderr, exit := runCLI(t, "llm", "history", "list", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit=%d stderr=%s", exit, stderr)
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body := res.Result.(map[string]any)
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions on a fresh keyring; got %d", len(sessions))
	}
	if body["principal"] != "alice" {
		t.Errorf("principal = %v, want alice", body["principal"])
	}
}

// TestLlmHistory_ShredRequiresYes: the destructive shred
// refuses without --yes.
func TestLlmHistory_ShredRequiresYes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "alice")

	provisionKeyring(t)
	_, stderr, exit := runCLI(t, "llm", "history", "shred", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("shred without --yes should refuse")
	}
	if !strings.Contains(stderr, "usage.confirmation_required") {
		t.Errorf("expected usage.confirmation_required; stderr=%s", stderr)
	}
}

// TestLlmHistory_ListWildcardPrincipal: `--principal *`
// lists across every principal on the host.
func TestLlmHistory_ListWildcardPrincipal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "alice")

	provisionKeyring(t)
	stdout, _, exit := runCLI(t, "llm", "history", "list", "--principal", "*", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("wildcard list exit=%d", exit)
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body := res.Result.(map[string]any)
	// Wildcard => empty principal scope
	if v, ok := body["principal"].(string); ok && v != "" {
		t.Errorf("wildcard list should report empty principal; got %q", v)
	}
}

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression: askLine used to busy-loop forever when stdin hit EOF with a
// required (no-default) prompt — ~80 MB of re-printed prompts in seconds
// under `init < /dev/null`. It must abort with a structured usage error.
func TestAskLine_EOFAbortsInsteadOfLooping(t *testing.T) {
	var out strings.Builder
	p := newPrompter(strings.NewReader(""), &out, false)
	_, err := p.askLine("PostgreSQL connection (libpq URI)", "", validateNonEmpty)
	if err == nil {
		t.Fatal("askLine on closed stdin returned nil; must abort")
	}
	oe, ok := output.AsOutputError(err)
	if !ok || oe.Code != "init.stdin_closed" {
		t.Errorf("error = %v, want structured init.stdin_closed", err)
	}
	// The prompt must have been printed a bounded number of times (once),
	// not thousands.
	if n := strings.Count(out.String(), "PostgreSQL connection"); n > 2 {
		t.Errorf("prompt printed %d times on EOF — busy-loop regression", n)
	}
}

// Regression: --quick defaulted to root-only /var/backups/pg_hardstorage,
// so every non-root evaluator failed with a permission error. Non-root
// must get a user-writable default.
func TestQuickDefaultRepoURL_NonRootIsUserWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; non-root branch not reachable")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := quickDefaultRepoURL()
	want := "file://" + filepath.Join(home, ".local", "share", "pg_hardstorage", "repo")
	if got != want {
		t.Errorf("quickDefaultRepoURL() = %q, want %q", got, want)
	}
}

// Regression: usage.* setup errors (e.g. usage.bad_pg_dsn from a pasted
// `--pg-connection ...` placeholder) retried forever with backoff. Any
// usage.* code is operator input and must be permanent (fail fast).
func TestIsPermanentStreamSetupError_UsageCodes(t *testing.T) {
	for _, code := range []string{"usage.bad_pg_dsn", "usage.bad_lsn", "usage.bad_flag"} {
		err := output.NewError(code, "x").Wrap(output.ErrUsage)
		if !isPermanentStreamSetupError(err) {
			t.Errorf("%s not treated as permanent — would retry a parse error forever", code)
		}
	}
	if isPermanentStreamSetupError(output.NewError("connect.replication", "x")) {
		t.Error("connect.replication must stay retryable (Patroni failovers surface as it)")
	}
}

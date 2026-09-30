package cli_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestLlmDoctor_FailureIsNotMisuse: a failing check (here: no
// provider can be resolved) is a runtime failure, not a usage error.
// docs/reference/exit-codes.md reserves 2 for bad invocations
// ("fix the invocation"); a provider outage or missing key must not
// tell cron/CI the command line was wrong.
func TestLlmDoctor_FailureIsNotMisuse(t *testing.T) {
	llmXDGHome(t)
	t.Setenv("PG_HARDSTORAGE_LLM_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_PROVIDER", "")

	stdout, stderr, exit := runCLI(t, "llm", "doctor", "-o", "json")
	if exit != int(output.ExitError) {
		t.Fatalf("failing llm doctor exit=%d, want %d (ExitError)\nstdout=%s\nstderr=%s",
			exit, output.ExitError, stdout, stderr)
	}
}

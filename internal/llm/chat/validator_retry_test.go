package chat

import (
	"context"
	"strings"
	"testing"
)

// TestValidateAndMaybeRetry_NoWarningsNoRetry asserts the retry
// path is a no-op when the first reply is already clean.
func TestValidateAndMaybeRetry_NoWarningsNoRetry(t *testing.T) {
	s := &Session{
		CommandValidator:    func(_ string) error { return nil },
		MaxValidatorRetries: 1,
	}
	reply := &Reply{Text: "Run `pg_hardstorage status` weekly."}
	out := s.validateAndMaybeRetry(context.Background(), reply)
	if got := len(out.CommandWarnings); got != 0 {
		t.Errorf("expected zero warnings on clean reply, got %d", got)
	}
}

// TestValidateAndMaybeRetry_WarningsButNoRetry asserts that with
// MaxValidatorRetries=0 we surface warnings without re-prompting.
func TestValidateAndMaybeRetry_WarningsButNoRetry(t *testing.T) {
	s := &Session{
		CommandValidator: func(cmd string) error {
			if strings.Contains(cmd, "--bogus") {
				return &fakeErr{msg: "unknown flag: --bogus"}
			}
			return nil
		},
		MaxValidatorRetries: 0,
	}
	reply := &Reply{Text: "Run:\n```\npg_hardstorage status --bogus\n```"}
	out := s.validateAndMaybeRetry(context.Background(), reply)
	if got := len(out.CommandWarnings); got != 1 {
		t.Fatalf("expected 1 warning, got %d", got)
	}
	if !strings.Contains(out.CommandWarnings[0].Issue, "--bogus") {
		t.Errorf("warning didn't mention the bad flag: %+v", out.CommandWarnings[0])
	}
}

// TestExtractAgentCommands_FencedAndContinuation verifies the
// extractor catches multi-line backslash-continuation forms
// inside fenced code blocks, which is how the model usually
// formats its recommendations.
func TestExtractAgentCommands_FencedAndContinuation(t *testing.T) {
	text := "Here's the workflow:\n\n```bash\npg_hardstorage restore db1 latest \\\n  --target /tmp/r \\\n  --to 'yesterday'\n```\n\nAlso run `pg_hardstorage status`."
	cmds := extractAgentCommands(text)
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d: %#v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "restore db1 latest") ||
		!strings.Contains(cmds[0], "--target /tmp/r") ||
		!strings.Contains(cmds[0], "--to 'yesterday'") {
		t.Errorf("continuation join failed: %q", cmds[0])
	}
	if cmds[1] != "pg_hardstorage status" {
		t.Errorf("inline-backtick form not extracted cleanly: %q", cmds[1])
	}
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

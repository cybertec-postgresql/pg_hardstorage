package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runCmd executes args against a fresh root and returns stdout, stderr,
// and the exit code.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	exit = cli.Run(root)
	return out.String(), errb.String(), exit
}

func TestVersion_TextMode(t *testing.T) {
	out, _, exit := runCmd(t, "version", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, want %d", exit, output.ExitOK)
	}
	if !strings.HasPrefix(out, "pg_hardstorage ") {
		t.Errorf("text mode should produce the one-liner; got %q", out)
	}
}

func TestVersion_JSONMode(t *testing.T) {
	out, _, exit := runCmd(t, "version", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var got output.Result
	if err := stdjson.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if got.Schema != output.Schema {
		t.Errorf("schema = %q", got.Schema)
	}
	if !strings.HasSuffix(got.Command, "version") {
		t.Errorf("command should end with 'version'; got %q", got.Command)
	}
	if got.IsError() {
		t.Error("version should not be an error result")
	}
}

func TestVersion_NDJSONMode(t *testing.T) {
	out, _, exit := runCmd(t, "version", "-o", "ndjson")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("ndjson should be exactly one line; got %q", out)
	}
}

// Stub-error tests retired: every command in the v0.1 surface is
// now real. The structured-error contract for the stub() helper is
// still exercised by the framework — every CLI command that returns
// a typed *output.Error goes through the same encoder — but the
// dedicated pin against a stubbed verb has no remaining target.
//
// If a future feature lands as a structured stub before its real
// implementation is ready, re-introduce a TestStub_* against that
// verb.
//
// silence unused-import on the rare build paths where these were
// the only references:
var _ = stdjson.Marshal
var _ = strings.Contains

func TestUnknownOutputFormat_Misuse(t *testing.T) {
	_, _, exit := runCmd(t, "version", "-o", "xml")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want %d", exit, output.ExitMisuse)
	}
}

func TestEnvVarOverride(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "ndjson")
	out, _, exit := runCmd(t, "version")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("env should pick ndjson; got %q", out)
	}
}

// TestFlagError_ExitsMisuse locks the FlagErrorFunc contract: a
// cobra flag-parse failure (unknown flag, bad flag value) must exit
// ExitMisuse (2), the same as a missing-positional-arg usage error —
// not the generic ExitError (1). Without the root's FlagErrorFunc,
// cobra returns a bare error that ExitCodeFor can't classify and it
// leaks out as exit 1. exitcode.go's contract documents unknown-flag
// as an ErrUsage case.
func TestFlagError_ExitsMisuse(t *testing.T) {
	cases := [][]string{
		{"version", "--no-such-flag"},
		{"backup", "--no-such-flag"},
		{"llm", "ask", "--no-such-flag", "hi"},
		{"restore", "--bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, _, exit := runCmd(t, args...)
			if exit != int(output.ExitMisuse) {
				t.Errorf("%v: exit = %d, want %d (ExitMisuse)", args, exit, output.ExitMisuse)
			}
		})
	}
}

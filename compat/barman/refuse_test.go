package barman

import (
	"bytes"
	"strings"
	"testing"
)

// TestRefusalSurface verifies that every refusal-only verb produces
// the canonical "not implemented in v1.1" line and a non-zero exit
// code.  Operators wiring their cron jobs to this binary need
// predictable failure modes — a clean refusal beats an ambiguous
// "command not found".
func TestRefusalSurface(t *testing.T) {
	cases := []struct {
		args         []string
		wantSnippet  string
		wantExitCode int
	}{
		{[]string{"cron"}, "use systemd timers", 2},
		{[]string{"archive-wal"}, "no equivalent", 2},
		{[]string{"switch-wal"}, "not implemented", 2},
		{[]string{"diagnose"}, "doctor", 2},
		{[]string{"verify-backup"}, "verify", 2},
		{[]string{"keep"}, "hold add", 2},
		{[]string{"receive-wal"}, "wal stream", 2},
		{[]string{"replication-status"}, "doctor", 2},
		{[]string{"show-server"}, "deployment show", 2},
	}

	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewRoot(&stdout, &stderr)
			root.SetArgs(tc.args)
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			_, err := root.ExecuteC()
			if err == nil {
				t.Fatalf("want refusal error, got nil; stderr=%q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantSnippet) {
				t.Errorf("stderr missing %q\n--- stderr ---\n%s", tc.wantSnippet, stderr.String())
			}
			if !strings.Contains(stderr.String(), "pg-hardstorage-barman:") {
				t.Errorf("stderr missing canonical prefix; got:\n%s", stderr.String())
			}
			if got := ExitCode(err); got != tc.wantExitCode {
				t.Errorf("exit code: got %d want %d", got, tc.wantExitCode)
			}
		})
	}
}

// TestExitCodeNil ensures the helper handles success cleanly.
func TestExitCodeNil(t *testing.T) {
	if ExitCode(nil) != 0 {
		t.Errorf("ExitCode(nil) must be 0")
	}
}

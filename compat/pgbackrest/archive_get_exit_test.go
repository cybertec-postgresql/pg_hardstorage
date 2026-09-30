package pgbackrest

import (
	"testing"
)

// archive-get is PostgreSQL's restore_command: exit 1 means "end of
// archive, promote". Only the native notfound (6) may become 1; every
// other failure must exit >125 so recovery aborts instead of promoting
// with unreplayed WAL still in the repository.
func TestArchiveGet_ExitCodeContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nativeRC int
		want     int
	}{
		{"delivered", 0, 0},
		{"notfound", 6, 1},
		{"generic failure (S3 503)", 1, exitAbortRecovery},
		{"storage fault", 4, exitAbortRecovery},
		{"usage", 2, exitAbortRecovery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureDispatch(t)
			dispatchNative = func([]string) int { return tc.nativeRC }
			root := NewRoot()
			root.SetArgs([]string{"--stanza=db1", "--repo1-path=/r", "archive-get",
				"000000010000000000000001", "/tmp/seg"})
			cmd, err := root.ExecuteC()
			got := 0
			if err != nil {
				got = exitCodeFor(cmd, err)
			}
			if got != tc.want {
				t.Fatalf("native rc %d: shim exit %d, want %d (err %v)", tc.nativeRC, got, tc.want, err)
			}
		})
	}
}

// Failures that never reached the repository — missing stanza, bad
// argc, unknown or refused flags — cannot mean "segment absent".
func TestArchiveGet_ConfigAndArgvErrorsAbortRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"missing stanza", []string{"archive-get", "000000010000000000000001", "/tmp/seg"}},
		{"wrong argc", []string{"--stanza=db1", "--repo1-path=/r", "archive-get", "000000010000000000000001"}},
		{"unknown flag", []string{"--stanza=db1", "--repo1-path=/r", "archive-get", "--bogus", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureDispatch(t)
			t.Cleanup(swapStanzaLookup(func(string) stanzaSettings { return stanzaSettings{} }))
			root := NewRoot()
			root.SetArgs(tc.argv)
			cmd, err := root.ExecuteC()
			if err == nil {
				t.Fatal("expected error")
			}
			if got := exitCodeFor(cmd, err); got != exitAbortRecovery {
				t.Fatalf("exit %d, want %d (err %v)", got, exitAbortRecovery, err)
			}
		})
	}
}

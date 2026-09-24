package walg

import (
	"bytes"
	"errors"
	"testing"
)

// wal-fetch is PostgreSQL's restore_command: exit 1 means "end of
// archive, promote". Only the native notfound (6) may become 1; every
// other failure must exit >125 so recovery aborts instead of promoting
// with unreplayed WAL in the repository.
func TestWalFetch_ExitCodeContract(t *testing.T) {
	env := map[string]string{"WALG_S3_PREFIX": "s3://acme/wal-g", "PGHOST": "db.example.com"}
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
			prev := dispatchNative
			t.Cleanup(func() { dispatchNative = prev })
			prevEnv := envLookup
			envLookup = func(k string) string { return env[k] }
			t.Cleanup(func() { envLookup = prevEnv })
			dispatchNative = func([]string) int { return tc.nativeRC }
			var stderr bytes.Buffer
			root := NewRoot(&bytes.Buffer{}, &stderr)
			root.SetArgs([]string{"wal-fetch", "000000010000000000000003", "/tmp/x"})
			cmd, err := root.ExecuteC()
			got := 0
			if err != nil {
				got = exitCodeFor(cmd, err)
			}
			if got != tc.want {
				t.Fatalf("native rc %d: shim exit %d, want %d (stderr %q)", tc.nativeRC, got, tc.want, stderr.String())
			}
		})
	}
}

// A broken WALG_* configuration never reached the repository; it is a
// configuration fault, not "segment absent".
func TestWalFetch_BadEnvAbortsRecovery(t *testing.T) {
	_, exit, _ := runWithStubbedDispatch(t, map[string]string{
		"WALG_S3_PREFIX": "s3://a/b", "WALG_FILE_PREFIX": "/srv/wal",
	},
		[]string{"wal-fetch", "000000010000000000000003", "/tmp/x"})
	if exit != exitAbortRecovery {
		t.Fatalf("missing env: exit %d, want %d", exit, exitAbortRecovery)
	}
}

// Cobra-level failures (wrong argc, unknown flag) happen before RunE.
func TestWalFetch_ArgvErrorAbortsRecovery(t *testing.T) {
	for _, argv := range [][]string{
		{"wal-fetch", "000000010000000000000003"},
		{"wal-fetch", "--bogus", "a", "b"},
	} {
		root := NewRoot(&bytes.Buffer{}, &bytes.Buffer{})
		root.SetArgs(argv)
		cmd, err := root.ExecuteC()
		if err == nil {
			t.Fatalf("%v: expected error", argv)
		}
		if got := exitCodeFor(cmd, err); got != exitAbortRecovery {
			t.Errorf("%v: exit %d, want %d", argv, got, exitAbortRecovery)
		}
	}
	// Other verbs keep the ordinary mapping.
	if got := exitCodeFor(nil, errors.New("x")); got != 1 {
		t.Errorf("non-wal-fetch generic error: exit %d, want 1", got)
	}
}

package walg

import (
	"bytes"
	"strings"
	"testing"
)

// Each verb test runs the cobra root with a synthetic argv, captures
// the args we would have dispatched to the native CLI, and asserts
// that mapping looks right.

func runWithStubbedDispatch(t *testing.T, env map[string]string, argv []string) (capturedArgs []string, exitCode int, stderr string) {
	t.Helper()

	// Override env lookup deterministically.
	prev := envLookup
	envLookup = func(k string) string {
		return env[k]
	}
	t.Cleanup(func() { envLookup = prev })

	// Stub BOTH dispatch entry points so no native CLI runs.
	// The auto-init wrapper (dispatchWithAutoInit) goes through
	// dispatchNativeCapture; verbs without auto-init (backup-list,
	// backup-fetch, wal-fetch) still use the simple int-returning
	// dispatchNative.  Stubbing both keeps every verb's test
	// indifferent to which path it takes.
	prevDispatch := dispatchNative
	dispatchNative = func(args []string) int {
		capturedArgs = append([]string{}, args...)
		return 0
	}
	t.Cleanup(func() { dispatchNative = prevDispatch })

	prevCapture := dispatchNativeCapture
	dispatchNativeCapture = func(args []string) dispatchResult {
		capturedArgs = append([]string{}, args...)
		return dispatchResult{ExitCode: 0}
	}
	t.Cleanup(func() { dispatchNativeCapture = prevCapture })

	var stderrBuf bytes.Buffer
	root := NewRoot(&bytes.Buffer{}, &stderrBuf)
	// Also redirect the package-level stderr so emitWarnings hits
	// our buffer.
	prevStderr := stderrWriter
	stderrWriter = &stderrBuf
	t.Cleanup(func() { stderrWriter = prevStderr })

	root.SetArgs(argv)
	if err := root.Execute(); err != nil {
		exitCode = ExitCode(err)
		// Mirror Execute()'s behaviour: shimErrors already
		// printed the canonical refusal to stderrWriter; other
		// errors land here so callers see them too.
		if _, ok := err.(*shimError); !ok {
			stderrBuf.WriteString(err.Error())
			stderrBuf.WriteByte('\n')
		}
	}
	return capturedArgs, exitCode, stderrBuf.String()
}

func TestBackupPush_Default_DispatchesIncremental(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
			"PGUSER":         "pgbackup",
		},
		[]string{"backup-push", "/var/lib/postgresql/15/main"},
	)
	if exit != 0 {
		t.Fatalf("exit code %d (expected 0 with stub dispatch)", exit)
	}
	want := []string{
		"backup", "db.example.com",
		"--pg-connection", "postgres://pgbackup@db.example.com/postgres",
		"--repo", "s3://acme/wal-g",
		"--incremental-from", "latest",
	}
	if !slicesEqual(got, want) {
		t.Errorf("dispatch:\n got %v\nwant %v", got, want)
	}
}

func TestBackupPush_Full_OmitsIncremental(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"backup-push", "--full", "/var/lib/postgresql/15/main"},
	)
	if exit != 0 {
		t.Fatalf("exit code %d", exit)
	}
	for _, a := range got {
		if a == "--incremental-from" {
			t.Errorf("--full should NOT add --incremental-from; got %v", got)
		}
	}
}

func TestBackupPush_Permanent_Refused(t *testing.T) {
	_, exit, stderr := runWithStubbedDispatch(t,
		map[string]string{"WALG_S3_PREFIX": "s3://acme/wal-g", "PGHOST": "db"},
		[]string{"backup-push", "--permanent", "/var/lib/postgresql/15/main"},
	)
	if exit != notImplementedExitCode {
		t.Errorf("expected exit %d, got %d", notImplementedExitCode, exit)
	}
	if !strings.Contains(stderr, "hold add") {
		t.Errorf("stderr should suggest `hold add`; got %q", stderr)
	}
}

func TestBackupFetch_Latest(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"backup-fetch", "/tmp/restore", "LATEST"},
	)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	want := []string{
		"restore", "db.example.com", "latest",
		"--target", "/tmp/restore",
		"--pg-connection", "postgres://postgres@db.example.com/postgres",
		"--repo", "s3://acme/wal-g",
	}
	if !slicesEqual(got, want) {
		t.Errorf("dispatch:\n got %v\nwant %v", got, want)
	}
}

func TestBackupFetch_NamedBackup(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"backup-fetch", "/tmp/restore", "base_000000010000000000000010"},
	)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if !sliceContainsAdjacent(got, "--to-backup", "base_000000010000000000000010") {
		t.Errorf("expected --to-backup adjacency; got %v", got)
	}
}

func TestBackupList_DefaultPretty(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"backup-list"},
	)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if got[0] != "list" {
		t.Errorf("expected verb=list; got %v", got)
	}
}

func TestBackupList_JSON(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"backup-list", "--json"},
	)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if !sliceContainsAdjacent(got, "-o", "json") {
		t.Errorf("expected -o json; got %v", got)
	}
}

func TestWalPush(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"wal-push", "/var/lib/postgresql/15/pg_wal/000000010000000000000003"},
	)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	want := []string{
		"wal", "push", "db.example.com",
		"/var/lib/postgresql/15/pg_wal/000000010000000000000003",
		"--pg-connection", "postgres://postgres@db.example.com/postgres",
		"--repo", "s3://acme/wal-g",
	}
	if !slicesEqual(got, want) {
		t.Errorf("dispatch:\n got %v\nwant %v", got, want)
	}
}

func TestWalFetch(t *testing.T) {
	got, exit, _ := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX": "s3://acme/wal-g",
			"PGHOST":         "db.example.com",
		},
		[]string{"wal-fetch", "000000010000000000000003", "/var/lib/postgresql/15/pg_wal/000000010000000000000003"},
	)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if got[0] != "wal" || got[1] != "fetch" {
		t.Errorf("expected `wal fetch`; got %v", got)
	}
}

func TestRefusedVerbs(t *testing.T) {
	for _, verb := range []string{"delete", "backup-mark", "catchup-push", "wal-receive", "st", "copy"} {
		t.Run(verb, func(t *testing.T) {
			_, exit, stderr := runWithStubbedDispatch(t,
				map[string]string{},
				[]string{verb},
			)
			if exit != notImplementedExitCode {
				t.Errorf("exit: got %d want %d", exit, notImplementedExitCode)
			}
			if !strings.Contains(stderr, "not implemented in v1.1") {
				t.Errorf("stderr should carry refusal message; got %q", stderr)
			}
		})
	}
}

func TestUnknownVerb(t *testing.T) {
	_, exit, stderr := runWithStubbedDispatch(t,
		map[string]string{},
		[]string{"some-imaginary-verb"},
	)
	if exit != notImplementedExitCode {
		t.Errorf("exit: got %d want %d", exit, notImplementedExitCode)
	}
	if !strings.Contains(stderr, "not implemented in v1.1") {
		t.Errorf("expected refusal; got %q", stderr)
	}
}

func TestLibsodiumKeyRefusedAtDispatch(t *testing.T) {
	_, exit, stderr := runWithStubbedDispatch(t,
		map[string]string{
			"WALG_S3_PREFIX":     "s3://acme/wal-g",
			"PGHOST":             "db.example.com",
			"WALG_LIBSODIUM_KEY": "abcdef",
		},
		[]string{"backup-push", "/var/lib/postgresql/15/main"},
	)
	// LIBSODIUM refusal is a generic error (not shimError), so
	// ExitCode falls back to 1.
	if exit != 1 {
		t.Errorf("exit: got %d want 1", exit)
	}
	if !strings.Contains(stderr, "WALG_LIBSODIUM_KEY") {
		t.Errorf("stderr should mention WALG_LIBSODIUM_KEY; got %q", stderr)
	}
}

func slicesEqual(a, b []string) bool {
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

func sliceContainsAdjacent(haystack []string, a, b string) bool {
	for i := 0; i+1 < len(haystack); i++ {
		if haystack[i] == a && haystack[i+1] == b {
			return true
		}
	}
	return false
}

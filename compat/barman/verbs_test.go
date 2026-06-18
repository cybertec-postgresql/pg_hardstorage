package barman

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// runShim invokes the shim root with the given argv, captures the
// synthetic argv handed to the native CLI, and returns it alongside
// any warnings and the verb's error.  Stubs the deployment-config
// lookup so verb unit tests don't need a real pg_hardstorage.yaml
// on disk; the synthetic argv has the auto-injected --repo /
// --pg-connection trimmed off so per-verb assertions stay focused
// on the translation logic.
func runShim(t *testing.T, argv ...string) (synthetic []string, stdout, stderr string, err error) {
	t.Helper()
	var captured []string
	restore := swapDispatcher(func(_, _ io.Writer, args []string) error {
		captured = append([]string(nil), args...)
		return nil
	})
	defer restore()
	restoreDep := swapDeploymentLookup(func(server string) (deploymentSettings, error) {
		return deploymentSettings{
			Repo:         "file:///tmp/test-repo",
			PGConnection: "postgres://postgres@h/postgres",
		}, nil
	})
	defer restoreDep()

	var sout, serr bytes.Buffer
	root := NewRoot(&sout, &serr)
	root.SetArgs(argv)
	root.SetOut(&sout)
	root.SetErr(&serr)
	_, err = root.ExecuteC()
	return stripInjectedFlags(captured), sout.String(), serr.String(), err
}

// stripInjectedFlags removes the --repo and --pg-connection
// pairs the deployment-lookup helper appends, so verb tests
// continue to assert on the verb-specific translation alone.
// A separate test (TestInjectDeploymentFlags) covers the
// injection itself end-to-end.
func stripInjectedFlags(in []string) []string {
	out := make([]string, 0, len(in))
	for i := 0; i < len(in); i++ {
		if (in[i] == "--repo" || in[i] == "--pg-connection") && i+1 < len(in) {
			i++
			continue
		}
		out = append(out, in[i])
	}
	return out
}

func TestBackupVerb(t *testing.T) {
	got, _, _, err := runShim(t, "backup", "db1")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if want := []string{"backup", "db1"}; !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestBackupVerbImmediateCKPT(t *testing.T) {
	got, _, _, err := runShim(t, "backup", "db1", "--immediate-checkpoint")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if want := []string{"backup", "db1", "--fast"}; !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestBackupVerbDroppedFlagsWarn(t *testing.T) {
	_, _, stderr, err := runShim(t, "backup", "db1", "--jobs=4", "--retry-times=3")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	for _, frag := range []string{"--jobs", "--retry-times"} {
		if !strings.Contains(stderr, frag) {
			t.Errorf("stderr missing %q in %q", frag, stderr)
		}
	}
}

func TestRecoverVerbBasic(t *testing.T) {
	got, _, _, err := runShim(t, "recover", "db1", "20260427T0942-abc", "/srv/pg17")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	want := []string{"restore", "db1", "20260427T0942-abc", "--target", "/srv/pg17"}
	if !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestRecoverVerbPITR(t *testing.T) {
	got, _, _, err := runShim(t,
		"recover", "db1", "latest", "/srv/pg17",
		"--target-time=2026-04-27 09:42 UTC",
		"--target-action=promote",
	)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	want := []string{
		"restore", "db1", "latest", "--target", "/srv/pg17",
		"--to", "2026-04-27 09:42 UTC",
		"--to-action", "promote",
	}
	if !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestRecoverVerbXIDRefuses(t *testing.T) {
	_, _, _, err := runShim(t,
		"recover", "db1", "latest", "/srv/pg17",
		"--target-xid=12345",
	)
	if err == nil {
		t.Fatalf("want refusal error for --target-xid, got nil")
	}
	if !strings.Contains(err.Error(), "--target-xid") {
		t.Errorf("error message: %v", err)
	}
}

func TestListBackupVerb(t *testing.T) {
	got, _, _, err := runShim(t, "list-backup", "db1")
	if err != nil {
		t.Fatalf("list-backup: %v", err)
	}
	if want := []string{"list", "db1"}; !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestListBackupVerbMinimal(t *testing.T) {
	got, _, _, err := runShim(t, "list-backup", "db1", "--minimal")
	if err != nil {
		t.Fatalf("list-backup --minimal: %v", err)
	}
	if len(got) < 4 || got[0] != "list" || got[2] != "--output" || got[3] != "template" {
		t.Errorf("argv: %v", got)
	}
}

func TestShowBackupVerb(t *testing.T) {
	got, _, _, err := runShim(t, "show-backup", "db1", "20260427T0942-abc")
	if err != nil {
		t.Fatalf("show-backup: %v", err)
	}
	want := []string{"show", "db1", "20260427T0942-abc"}
	if !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestCheckVerb(t *testing.T) {
	got, _, _, err := runShim(t, "check", "db1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if want := []string{"doctor", "db1"}; !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestCheckVerbNagios(t *testing.T) {
	got, _, _, err := runShim(t, "check", "db1", "--nagios")
	if err != nil {
		t.Fatalf("check --nagios: %v", err)
	}
	if len(got) < 3 || got[0] != "doctor" || got[2] != "--output" {
		t.Errorf("argv: %v", got)
	}
}

func TestDeleteVerb(t *testing.T) {
	got, _, _, err := runShim(t, "delete", "db1", "20260427T0942-abc")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []string{"backup", "delete", "db1", "20260427T0942-abc"}
	if !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestWALArchiveVerb covers the separate barman-wal-archive root.
func TestWALArchiveVerb(t *testing.T) {
	var captured []string
	restore := swapDispatcher(func(_, _ io.Writer, args []string) error {
		captured = append([]string(nil), args...)
		return nil
	})
	defer restore()
	restoreDep := swapDeploymentLookup(func(server string) (deploymentSettings, error) {
		return deploymentSettings{
			Repo:         "file:///tmp/test-repo",
			PGConnection: "postgres://postgres@h/postgres",
		}, nil
	})
	defer restoreDep()

	var sout, serr bytes.Buffer
	root := NewWALArchiveRoot(&sout, &serr)
	root.SetArgs([]string{"db1", "pg_wal/000000010000000000000005"})
	root.SetOut(&sout)
	root.SetErr(&serr)
	if _, err := root.ExecuteC(); err != nil {
		t.Fatalf("wal-archive: %v", err)
	}
	got := stripInjectedFlags(captured)
	want := []string{"wal", "push", "db1", "pg_wal/000000010000000000000005"}
	if !equalSlices(got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
	// Sanity: --repo MUST be present in the raw captured argv.
	// --pg-connection is intentionally NOT injected for
	// wal-archive (issue #8 derives sysid from the segment
	// header, and a deployment without a configured DSN should
	// still be able to archive).
	if !containsPair(captured, "--repo", "file:///tmp/test-repo") {
		t.Errorf("missing --repo file:///tmp/test-repo in captured argv: %v", captured)
	}
	for i := 0; i < len(captured); i++ {
		if captured[i] == "--pg-connection" {
			t.Errorf("wal-archive should NOT inject --pg-connection (sysid derived from segment header); captured: %v", captured)
		}
	}
}

// TestInjectDeploymentFlags exercises the helper directly:
// confirms repo is required, pg_connection is forwarded when
// present and silently omitted when not, and lookup errors
// propagate.
func TestInjectDeploymentFlags(t *testing.T) {
	t.Run("happy_path", func(t *testing.T) {
		restore := swapDeploymentLookup(func(server string) (deploymentSettings, error) {
			return deploymentSettings{
				Repo:         "s3://bucket/db1",
				PGConnection: "postgres://x@host/db1",
			}, nil
		})
		defer restore()
		got, err := injectDeploymentFlags([]string{"backup", "db1"}, "db1", true)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"backup", "db1", "--repo", "s3://bucket/db1", "--pg-connection", "postgres://x@host/db1"}
		if !equalSlices(got, want) {
			t.Errorf("got %v\nwant %v", got, want)
		}
	})
	t.Run("no_pg_when_wantsPG_false", func(t *testing.T) {
		restore := swapDeploymentLookup(func(server string) (deploymentSettings, error) {
			return deploymentSettings{Repo: "s3://bucket/db1", PGConnection: "should-be-omitted"}, nil
		})
		defer restore()
		got, err := injectDeploymentFlags([]string{"list", "db1"}, "db1", false)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"list", "db1", "--repo", "s3://bucket/db1"}
		if !equalSlices(got, want) {
			t.Errorf("got %v\nwant %v", got, want)
		}
	})
	t.Run("missing_pg_when_wantsPG_true_errors", func(t *testing.T) {
		restore := swapDeploymentLookup(func(server string) (deploymentSettings, error) {
			return deploymentSettings{Repo: "s3://bucket/db1"}, nil
		})
		defer restore()
		_, err := injectDeploymentFlags([]string{"backup", "db1"}, "db1", true)
		if err == nil || !strings.Contains(err.Error(), "pg_connection") {
			t.Errorf("err = %v; want pg_connection-mention error", err)
		}
	})
	t.Run("lookup_error_propagates", func(t *testing.T) {
		restore := swapDeploymentLookup(func(server string) (deploymentSettings, error) {
			return deploymentSettings{}, errExpected
		})
		defer restore()
		_, err := injectDeploymentFlags([]string{"backup", "x"}, "x", true)
		if err == nil || err.Error() != errExpected.Error() {
			t.Errorf("err = %v; want %v", err, errExpected)
		}
	})
}

var errExpected = &shimError{exitCode: 99, message: "synthetic"}

// containsPair reports whether haystack contains the slice
// [a, b] in order at any position.  Used by TestWALArchiveVerb
// to verify the injection without relying on positional
// indices (the deployment helper appends the pair at the END
// today, but a future "merge before --output" refactor must
// not break this test).
func containsPair(haystack []string, a, b string) bool {
	for i := 0; i+1 < len(haystack); i++ {
		if haystack[i] == a && haystack[i+1] == b {
			return true
		}
	}
	return false
}

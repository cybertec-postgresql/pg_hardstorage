package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParsePGVersionContent: the data-dir PG_VERSION file is a
// one-line ASCII integer. We tolerate trailing whitespace + the
// historical "9.6" minor-included shape. Pure unit test — no
// pg_ctl needed.
func TestParsePGVersionContent(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"17\n", 17, true},
		{"17", 17, true},
		{"  16\n", 16, true},
		{"15\r\n", 15, true},
		{"9.6\n", 9, true}, // historical minor-included shape; major is 9
		{"99", 99, true},
		{"", 0, false},
		{"\n", 0, false},
		{"abc", 0, false},
		{"  abc\n", 0, false},
		{"0", 0, false},
	}
	for _, c := range cases {
		got, err := parsePGVersionContent(c.in)
		if c.ok && err != nil {
			t.Errorf("parsePGVersionContent(%q): unexpected err %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("parsePGVersionContent(%q): expected err, got %d", c.in, got)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parsePGVersionContent(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParsePGBinaryVersionOutput: pg_dump --version follows the
// same banner shape as pg_ctl --version, so the shared parser
// handles both. Pin a few real-world shapes.
func TestParsePGBinaryVersionOutput(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"pg_dump (PostgreSQL) 17.2\n", 17, true},
		{"pg_dump (PostgreSQL) 16.4", 16, true},
		// Vendor banner — the heuristic takes the LAST
		// dot-bearing token, so this works even though the first
		// token has a dot too.
		{"pg_dump (PostgreSQL) 15.7\n", 15, true},
	}
	for _, c := range cases {
		got, err := parsePGCtlVersionOutput(c.in)
		if c.ok && err != nil {
			t.Errorf("parsePGCtlVersionOutput(%q): unexpected err %v", c.in, err)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parsePGCtlVersionOutput(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestVersionMismatchError_BinaryNamePropagatesToMessage: the
// error string must name the offending binary so the operator
// knows which to replace. Both pg_ctl and pg_dump cases pin'd
// here.
func TestVersionMismatchError_BinaryNamePropagatesToMessage(t *testing.T) {
	cases := []struct {
		name        string
		err         VersionMismatchError
		wantBinary  string
		wantOptHint string
	}{
		{
			name:        "pg_ctl",
			err:         VersionMismatchError{DataDir: "/d", DataDirMajor: 17, BinaryMajor: 16, PGCtlPath: "/usr/bin/pg_ctl", BinaryName: "pg_ctl"},
			wantBinary:  "pg_ctl",
			wantOptHint: "Options.PGCtlPath",
		},
		{
			name:        "pg_dump",
			err:         VersionMismatchError{DataDir: "/d", DataDirMajor: 17, BinaryMajor: 15, PGCtlPath: "/usr/bin/pg_dump", BinaryName: "pg_dump"},
			wantBinary:  "pg_dump",
			wantOptHint: "Options.PGDumpPath",
		},
		{
			name:        "legacy-empty-name-defaults-to-pg_ctl",
			err:         VersionMismatchError{DataDir: "/d", DataDirMajor: 17, BinaryMajor: 16, PGCtlPath: "/usr/bin/pg_ctl"},
			wantBinary:  "pg_ctl",
			wantOptHint: "Options.PGCtlPath",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := c.err.Error()
			if !strings.Contains(msg, c.wantBinary) {
				t.Errorf("error should name binary %q; got %q", c.wantBinary, msg)
			}
			if !strings.Contains(msg, c.wantOptHint) {
				t.Errorf("error should hint at %q override; got %q", c.wantOptHint, msg)
			}
		})
	}
}

// TestParsePGCtlVersionOutput: pg_ctl --version emits a one-line
// banner. Tolerate vendor-injected strings as long as a
// version-shaped token is present.
func TestParsePGCtlVersionOutput(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"pg_ctl (PostgreSQL) 17.2\n", 17, true},
		{"pg_ctl (PostgreSQL) 16.4", 16, true},
		{"pg_ctl (EnterpriseDB PostgreSQL) 17.2 (custom)\n", 17, true},
		{"pg_ctl (PostgreSQL) 9.6.24\n", 9, true},
		{"pg_ctl (PostgreSQL) 18beta1\n", 0, false}, // no dot in token
		{"", 0, false},
		{"some unrelated text\n", 0, false},
	}
	for _, c := range cases {
		got, err := parsePGCtlVersionOutput(c.in)
		if c.ok && err != nil {
			t.Errorf("parsePGCtlVersionOutput(%q): unexpected err %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("parsePGCtlVersionOutput(%q): expected err, got %d", c.in, got)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parsePGCtlVersionOutput(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestReadDataDirPGVersion_Absent: the most common operator
// confusion — pointing at a directory that isn't a PG data dir.
// The error names PG_VERSION so the operator knows what's missing.
func TestReadDataDirPGVersion_Absent(t *testing.T) {
	dir := t.TempDir() // empty
	_, err := readDataDirPGVersion(dir)
	if err == nil {
		t.Fatal("expected error for absent PG_VERSION")
	}
	if !strings.Contains(err.Error(), "PG_VERSION") {
		t.Errorf("error should mention PG_VERSION: %v", err)
	}
}

// TestReadDataDirPGVersion_Parses: a proper PG_VERSION file
// round-trips as the major version.
func TestReadDataDirPGVersion_Parses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readDataDirPGVersion(dir)
	if err != nil {
		t.Fatalf("readDataDirPGVersion: %v", err)
	}
	if got != 17 {
		t.Errorf("got %d, want 17", got)
	}
}

// TestStart_RefusesPGVersionMismatch: with a planted PG_VERSION
// of 99 (a future major) and pg_ctl on PATH (whatever its
// version), Start refuses with VersionMismatchError BEFORE
// touching auto.conf or socket dirs. Skips when pg_ctl isn't on
// PATH (CI without PG installed).
func TestStart_RefusesPGVersionMismatch(t *testing.T) {
	if _, err := exec.LookPath("pg_ctl"); err != nil {
		t.Skip("pg_ctl not on PATH; skipping version-mismatch integration")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("99\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Start(context.Background(), Options{DataDir: dir})
	if err == nil {
		t.Fatal("expected version mismatch error")
	}
	if !errors.Is(err, ErrPGVersionMismatch) {
		t.Errorf("expected errors.Is(ErrPGVersionMismatch); got %v", err)
	}
	var ve *VersionMismatchError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *VersionMismatchError; got %T", err)
	}
	if ve.DataDirMajor != 99 {
		t.Errorf("DataDirMajor = %d, want 99", ve.DataDirMajor)
	}
	if ve.BinaryMajor == 0 {
		t.Errorf("BinaryMajor should be populated; got 0")
	}
	// Critical: auto.conf must NOT be touched on the pre-flight
	// failure path. The pre-flight runs BEFORE any state mutation.
	if _, statErr := os.Stat(filepath.Join(dir, "postgresql.auto.conf")); statErr == nil {
		t.Errorf("pre-flight failure should not write auto.conf")
	}
}

// TestStart_RefusesAbsentPGVersion: pre-flight surfaces a clear
// error when PG_VERSION is missing. Same skip posture as the
// mismatch test.
func TestStart_RefusesAbsentPGVersion(t *testing.T) {
	if _, err := exec.LookPath("pg_ctl"); err != nil {
		t.Skip("pg_ctl not on PATH; skipping")
	}
	dir := t.TempDir()
	_, err := Start(context.Background(), Options{DataDir: dir})
	if err == nil {
		t.Fatal("expected error for absent PG_VERSION")
	}
	if !strings.Contains(err.Error(), "PG_VERSION") {
		t.Errorf("error should mention PG_VERSION: %v", err)
	}
}

// TestStart_SkipVersionCheckBypassesGate: SkipVersionCheck=true
// allows Start to proceed past the pre-flight even when
// PG_VERSION is absent or mismatched. The downstream pg_ctl
// invocation will fail on its own merits — this just confirms
// the gate isn't the obstacle.
func TestStart_SkipVersionCheckBypassesGate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No PG_VERSION; would normally fail pre-flight. With skip,
	// we proceed past pre-flight and fail later (pg_ctl doesn't
	// like the empty dir). What matters: the error must NOT be
	// the version-mismatch / PG_VERSION-absent variety.
	_, err := Start(context.Background(), Options{
		DataDir:          dir,
		PGCtlPath:        "/bin/false",
		PGDumpPath:       "/bin/false",
		SkipVersionCheck: true,
	})
	if err == nil {
		t.Fatal("expected /bin/false to fail")
	}
	if errors.Is(err, ErrPGVersionMismatch) {
		t.Errorf("SkipVersionCheck should bypass the version gate; got %v", err)
	}
	if strings.Contains(err.Error(), "PG_VERSION absent") {
		t.Errorf("SkipVersionCheck should bypass the PG_VERSION-absent check; got %v", err)
	}
}

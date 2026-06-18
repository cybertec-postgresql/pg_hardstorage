package restore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// makeFakeTool writes a one-line shell script named "pg_verifybackup"
// to dir, with the given exit code, then returns dir. Tests prepend
// dir to PATH so exec.LookPath finds it before any real binary.
func makeFakeTool(t *testing.T, exitCode int, stdout, stderr string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-tool harness uses /bin/sh; not portable to Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pg_verifybackup")
	body := "#!/bin/sh\n"
	if stdout != "" {
		body += `printf '%s' "` + stdout + `"` + "\n"
	}
	if stderr != "" {
		body += `printf '%s' "` + stderr + `" >&2` + "\n"
	}
	body += "exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// withPATHPrefix prepends dir to PATH for the duration of the test.
func withPATHPrefix(t *testing.T, dir string) {
	t.Helper()
	orig := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+orig)
}

// withEmptyPATH wipes PATH so exec.LookPath fails for any binary.
// Used to exercise the "missing tool" code paths.
func withEmptyPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir()) // dir exists but contains nothing
}

func TestParseVerifyMode(t *testing.T) {
	cases := []struct {
		in    string
		want  restore.VerifyMode
		isErr bool
	}{
		{"", restore.VerifyAuto, false},
		{"auto", restore.VerifyAuto, false},
		{"AUTO", restore.VerifyAuto, false},
		{" auto ", restore.VerifyAuto, false},
		{"skip", restore.VerifySkip, false},
		{"off", restore.VerifySkip, false},
		{"none", restore.VerifySkip, false},
		{"require", restore.VerifyRequire, false},
		{"required", restore.VerifyRequire, false},
		{"yes", restore.VerifyRequire, false},
		{"bogus", "", true},
	}
	for _, c := range cases {
		got, err := restore.ParseVerifyMode(c.in)
		if (err != nil) != c.isErr {
			t.Errorf("ParseVerifyMode(%q) err=%v wantErr=%v", c.in, err, c.isErr)
			continue
		}
		if !c.isErr && got != c.want {
			t.Errorf("ParseVerifyMode(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestVerify_Skip(t *testing.T) {
	res, err := restore.Verify(context.Background(), "/nope", restore.VerifySkip)
	if err != nil {
		t.Fatalf("skip should never error: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
}

func TestVerify_Auto_Passes(t *testing.T) {
	dir := makeFakeTool(t, 0, "backup_manifest verified\n", "")
	withPATHPrefix(t, dir)

	res, err := restore.Verify(context.Background(), t.TempDir(), restore.VerifyAuto)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != "passed" {
		t.Errorf("Status = %q, want passed", res.Status)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "verified") {
		t.Errorf("Stdout not captured: %q", res.Stdout)
	}
	if !strings.HasSuffix(res.ToolPath, "/pg_verifybackup") {
		t.Errorf("ToolPath = %q", res.ToolPath)
	}
}

func TestVerify_Auto_Fails_DoesNotError(t *testing.T) {
	dir := makeFakeTool(t, 1, "", "manifest mismatch at file foo\n")
	withPATHPrefix(t, dir)

	res, err := restore.Verify(context.Background(), t.TempDir(), restore.VerifyAuto)
	if err != nil {
		t.Errorf("auto mode must NOT return an error on tool failure: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("Status = %q, want failed", res.Status)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
}

func TestVerify_Require_Fails_ReturnsError(t *testing.T) {
	dir := makeFakeTool(t, 1, "", "manifest mismatch\n")
	withPATHPrefix(t, dir)

	_, err := restore.Verify(context.Background(), t.TempDir(), restore.VerifyRequire)
	if err == nil {
		t.Fatal("require mode must return error on tool failure")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected structured error; got %v", err)
	}
	if oe.Code != "verify.checksum_mismatch" {
		t.Errorf("code = %q", oe.Code)
	}
}

func TestVerify_Auto_MissingTool(t *testing.T) {
	withEmptyPATH(t)
	res, err := restore.Verify(context.Background(), t.TempDir(), restore.VerifyAuto)
	if err != nil {
		t.Errorf("auto mode must NOT error when tool is absent: %v", err)
	}
	if res.Status != "missing_tool" {
		t.Errorf("Status = %q, want missing_tool", res.Status)
	}
}

func TestVerify_Require_MissingTool(t *testing.T) {
	withEmptyPATH(t)
	_, err := restore.Verify(context.Background(), t.TempDir(), restore.VerifyRequire)
	if err == nil {
		t.Fatal("require mode must error when tool is absent")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected structured error; got %v", err)
	}
	if oe.Code != "verify.missing_tool" {
		t.Errorf("code = %q, want verify.missing_tool", oe.Code)
	}
}

func TestVerify_BadMode(t *testing.T) {
	_, err := restore.Verify(context.Background(), t.TempDir(), restore.VerifyMode("nonsense"))
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

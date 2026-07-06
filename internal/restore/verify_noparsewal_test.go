package restore

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Verify must invoke pg_verifybackup with -n (--no-parse-wal). A
// pg_hardstorage restore lays down the base backup only; the WAL needed
// to reach consistency is fetched at recovery time via restore_command,
// so the restored data dir has no pg_wal segments yet. Without -n,
// pg_verifybackup fails every normal restore with "could not find any
// WAL file", defeating the --verify gate.
func TestVerify_PassesNoParseWAL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-script seam is POSIX")
	}
	dir := t.TempDir()
	argfile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "pg_verifybackup")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argfile + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pg_verifybackup: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := Verify(context.Background(), t.TempDir(), VerifyRequire)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != "passed" {
		t.Errorf("status = %q, want passed", res.Status)
	}
	got, err := os.ReadFile(argfile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	args := strings.Fields(string(got))
	found := false
	for _, a := range args {
		if a == "-n" || a == "--no-parse-wal" {
			found = true
		}
	}
	if !found {
		t.Errorf("pg_verifybackup args = %v, want to include -n / --no-parse-wal", args)
	}
}

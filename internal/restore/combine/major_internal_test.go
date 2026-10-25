package combine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPGCombineBackupMajor(t *testing.T) {
	script := func(body string) string {
		p := filepath.Join(t.TempDir(), "pg_combinebackup")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, out string
		want      int
		wantErr   bool
	}{
		{"plain", "pg_combinebackup (PostgreSQL) 17.5", 17, false},
		{"distro suffix", "pg_combinebackup (PostgreSQL) 18.0 (Debian 18.0-1.pgdg12+1)", 18, false},
		{"no dot", "pg_combinebackup (PostgreSQL) 19", 19, false},
		{"not postgres", "something else entirely", 0, true},
		{"non numeric", "pg_combinebackup (PostgreSQL) beta", 0, true},
	} {
		got, err := readPGCombineBackupMajor(script("echo '" + tc.out + "'"))
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: got (%d, %v), want (%d, err=%v)", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
	if _, err := readPGCombineBackupMajor(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing binary must be an error")
	}
	if _, err := readPGCombineBackupMajor(script("exit 3")); err == nil {
		t.Error("a failing binary must be an error")
	}
}

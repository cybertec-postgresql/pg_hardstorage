package sandbox

import (
	"strings"
	"testing"
)

// Regression for issue #26: the sandbox ran pg_verifybackup WITHOUT
// -n/--no-parse-wal, so `recovery drill` (and `verify --full`) failed
// every WAL-streaming backup with "could not find any WAL file" — the
// base backup legitimately has an empty pg_wal/ (WAL lives in the repo
// and is fetched at recovery via restore_command). Same defect class as
// the v1.0.8 restore --verify fix.
func TestVerifyBackupArgs_NoParseWAL(t *testing.T) {
	args := verifyBackupArgs("/usr/lib/postgresql/18/bin/pg_verifybackup")
	if len(args) != 3 || args[1] != "-n" || args[2] != sandboxDataDir {
		t.Fatalf("verifyBackupArgs = %v, want [bin -n %s]", args, sandboxDataDir)
	}
	if !strings.HasSuffix(args[0], "pg_verifybackup") {
		t.Errorf("args[0] = %q, want the pg_verifybackup binary", args[0])
	}
}

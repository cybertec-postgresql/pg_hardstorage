package barmancloud

import "testing"

// CNPG passes relative %p paths (pg_wal/...), but a hand-written
// archive_command / restore_command may pass an absolute one. Blindly
// prefixing PGDATA turned /var/lib/pg/pg_wal/X into
// /pgdata//var/lib/pg/pg_wal/X — a path that does not exist.
func TestAbsoluteWALPathsNotPrefixed(t *testing.T) {
	env := map[string]string{"PGDATA": "/pgdata"}

	got, rc := runWithStubbedDispatch(t, env, nil, ExecuteWalArchive,
		[]string{"s3://b/p", "srv", "/var/lib/pg/pg_wal/000000010000000000000001"})
	if rc != 0 || len(got) < 4 || got[3] != "/var/lib/pg/pg_wal/000000010000000000000001" {
		t.Fatalf("wal-archive absolute path: rc=%d argv=%v", rc, got)
	}

	got, rc = runWithStubbedDispatch(t, env, nil, ExecuteWalRestore,
		[]string{"s3://b/p", "srv", "000000010000000000000001", "/var/lib/pg/pg_wal/RECOVERYXLOG"})
	if rc != 0 || len(got) < 5 || got[4] != "/var/lib/pg/pg_wal/RECOVERYXLOG" {
		t.Fatalf("wal-restore absolute path: rc=%d argv=%v", rc, got)
	}

	// Relative paths still resolve against PGDATA.
	got, _ = runWithStubbedDispatch(t, env, nil, ExecuteWalRestore,
		[]string{"s3://b/p", "srv", "000000010000000000000001", "pg_wal/RECOVERYXLOG"})
	if got[4] != "/pgdata/pg_wal/RECOVERYXLOG" {
		t.Fatalf("relative path: argv=%v", got)
	}
}

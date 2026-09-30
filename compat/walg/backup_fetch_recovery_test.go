package walg

import "testing"

// wal-g backup-fetch writes no recovery configuration: the operator
// adds recovery.signal / standby.signal and their own recovery_target_*
// and PostgreSQL replays every archived segment by default. A plain
// native restore instead arms standby.signal + recovery_target=
// 'immediate' + promote, which drops all WAL archived after the backup,
// promotes a would-be standby, and makes an operator-supplied
// recovery_target_time FATAL ("multiple recovery targets"). --to-latest
// arms restore_command with NO target and recovery.signal, which the
// operator's own signal file / targets compose with.
func TestBackupFetch_ReplaysAllWAL(t *testing.T) {
	for _, name := range []string{"LATEST", "base_000000010000000000000002"} {
		got, exit, _ := runWithStubbedDispatch(t,
			map[string]string{"WALG_S3_PREFIX": "s3://acme/wal-g", "PGHOST": "db.example.com"},
			[]string{"backup-fetch", "/tmp/restore", name},
		)
		if exit != 0 {
			t.Fatalf("exit %d", exit)
		}
		n := 0
		for _, a := range got {
			if a == "--to-latest" {
				n++
			}
			if a == "--to" || a == "--to-lsn" || a == "--to-name" || a == "--to-action" {
				t.Errorf("%s: backup-fetch must not arm a target/action of its own; got %v", name, got)
			}
		}
		if n != 1 {
			t.Errorf("%s: want exactly one --to-latest; got %v", name, got)
		}
	}
}

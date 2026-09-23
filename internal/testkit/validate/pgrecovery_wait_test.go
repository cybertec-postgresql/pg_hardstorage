package validate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TakeBackup retries while PostgreSQL is still recovering from a fault.
// It used to give up after three attempts — about three seconds of
// patience, calibrated on the light oltp_smoke profile. Under
// enterprise_heavy (10 GB seeded, 16 writers) PG was still in crash
// recovery 29 s after a fault, and every such backup was scored
// backup_failed: the soak blamed the product for refusing to back up a
// database that was not accepting connections yet.
//
// A fake docker answers "PG unreachable" five times, then succeeds.
func TestTakeBackupWaitsOutPGRecovery(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "n")
	fake := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$*" in
  *inspect*) echo true; exit 0 ;;
  *" backup "*)
    n=$(cat ` + count + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + count + `
    if [ $n -le 5 ]; then
      echo '{"error":{"code":"pg.unreachable","message":"cannot connect to PostgreSQL: connection refused"}}'
      exit 8
    fi
    echo '{"result":{"backup_id":"db1.full.20260923T000000Z.abcd"}}'; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldB, oldM := pgRecoveryBudget, pgRecoveryMaxBackoff
	pgRecoveryBudget, pgRecoveryMaxBackoff = 30*time.Second, 5*time.Millisecond
	defer func() { pgRecoveryBudget, pgRecoveryMaxBackoff = oldB, oldM }()

	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fake,
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	id, err := d.TakeBackup(context.Background())
	if err != nil {
		t.Fatalf("a backup that succeeds once PG finishes recovering was scored as failed: %v", err)
	}
	if id != "db1.full.20260923T000000Z.abcd" {
		t.Errorf("backup id = %q", id)
	}
}

// The wait must stay bounded: a database that never comes back is a
// failure, not a hang.
func TestTakeBackupGivesUpAfterTheBudget(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$*" in
  *inspect*) echo true; exit 0 ;;
  *" backup "*) echo '{"error":{"code":"pg.unreachable","message":"cannot connect to PostgreSQL"}}'; exit 8 ;;
esac
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldB, oldM := pgRecoveryBudget, pgRecoveryMaxBackoff
	pgRecoveryBudget, pgRecoveryMaxBackoff = 1500*time.Millisecond, 5*time.Millisecond
	defer func() { pgRecoveryBudget, pgRecoveryMaxBackoff = oldB, oldM }()

	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fake,
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	start := time.Now()
	if _, err := d.TakeBackup(context.Background()); err == nil {
		t.Fatal("PG never recovered; TakeBackup must report failure")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("took %s against a 1.5 s budget — the wait is not bounded", el)
	}
}

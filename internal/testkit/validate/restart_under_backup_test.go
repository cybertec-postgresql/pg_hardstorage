package validate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeDocker builds a docker stand-in whose StartedAt either changes
// between inspect calls (a container restart) or stays fixed, and
// whose `backup` is SIGKILLed (exit 137) — what a docker exec child
// reports when its container restarts underneath it.
func fakeDocker(t *testing.T, restarts bool) string {
	t.Helper()
	dir := t.TempDir()
	n := filepath.Join(dir, "n")
	second := "2026-09-23T17:09:30Z"
	if restarts {
		second = "2026-09-23T17:09:53Z"
	}
	script := `#!/bin/sh
case "$*" in
  *State.StartedAt*)
    c=$(cat ` + n + ` 2>/dev/null || echo 0); c=$((c+1)); echo $c > ` + n + `
    if [ $c -le 1 ]; then echo 2026-09-23T17:09:30Z; else echo ` + second + `; fi; exit 0 ;;
  *inspect*) echo true; exit 0 ;;
  *" backup "*) exit 137 ;;
esac
exit 0
`
	p := filepath.Join(dir, "docker")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The enterprise_heavy soak lost four backups to exit 137, each within
// one second of Docker's restart policy restarting the cell. The
// product did nothing wrong: docker exec children die with their
// container. Counting that as backup_failed blamed pg_hardstorage for
// the testbed.
func TestTakeBackupContainerRestartIsCellNotReady(t *testing.T) {
	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fakeDocker(t, true),
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	_, err := d.TakeBackup(context.Background())
	if !errors.Is(err, ErrCellNotReady) {
		t.Fatalf("a backup killed by its container restarting must be ErrCellNotReady, got: %v", err)
	}
}

// And the classification must not swallow real failures: with no
// restart, a killed backup is still a failed backup.
func TestTakeBackupKilledWithoutRestartStillFails(t *testing.T) {
	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fakeDocker(t, false),
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	_, err := d.TakeBackup(context.Background())
	if err == nil || errors.Is(err, ErrCellNotReady) {
		t.Fatalf("no restart happened; the backup failure must stay a failure, got: %v", err)
	}
}

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

// fakeDockerStops is a container that a fault stops under the backup
// and that has not restarted yet: StartedAt is unchanged, State.Running
// is true for the pre-check and then false (or unreadable), and the
// exec fails the way Docker fails to enter a dead container.
func fakeDockerStops(t *testing.T, afterward string) string {
	t.Helper()
	dir := t.TempDir()
	n := filepath.Join(dir, "n")
	script := `#!/bin/sh
case "$*" in
  *State.StartedAt*) echo 2026-09-24T05:26:08Z; exit 0 ;;
  *State.Running*)
    c=$(cat ` + n + ` 2>/dev/null || echo 0); c=$((c+1)); echo $c > ` + n + `
    if [ $c -le 1 ]; then echo true; exit 0; fi
    ` + afterward + ` ;;
  *" backup "*) echo "OCI runtime exec failed: exec failed: unable to start container process: error executing setns process: exit status 1" >&2; exit 128 ;;
esac
exit 0
`
	p := filepath.Join(dir, "docker")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The v1.5.0 release soak's first backup_failed: a SIGKILL fault killed
// the container, and the backup's docker exec could not enter it.
func TestTakeBackupContainerStoppedIsCellNotReady(t *testing.T) {
	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fakeDockerStops(t, "echo false; exit 0"),
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	_, err := d.TakeBackup(context.Background())
	if !errors.Is(err, ErrCellNotReady) {
		t.Fatalf("a backup whose container stopped under it must be ErrCellNotReady, got: %v", err)
	}
}

// If Docker cannot say whether the container is running, the failure
// stays a failure — not knowing is not evidence against the testbed.
func TestTakeBackupUnknownContainerStateStillFails(t *testing.T) {
	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fakeDockerStops(t, "exit 1"),
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	_, err := d.TakeBackup(context.Background())
	if err == nil || errors.Is(err, ErrCellNotReady) {
		t.Fatalf("container state unknown; the backup failure must stay a failure, got: %v", err)
	}
}

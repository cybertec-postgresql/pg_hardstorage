package validate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFakeDocker drops an executable docker stand-in built from script.
func writeFakeDocker(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A container restart during the pg-unreachable retry loop must not be
// charged to a LATER attempt. StartedAt was sampled once, before the
// first attempt: the cell restarted while the loop waited for PG, the
// next attempt reached PostgreSQL and failed on its own merits, and the
// stale StartedAt comparison turned that genuine product failure into
// backup_skipped_cell_down.
func TestTakeBackupRestartDuringRetriesDoesNotMaskLaterFailure(t *testing.T) {
	dir := t.TempDir()
	boots := filepath.Join(dir, "boots")
	calls := filepath.Join(dir, "calls")
	fake := writeFakeDocker(t, `case "$*" in
  *State.StartedAt*)
    c=$(cat `+boots+` 2>/dev/null || echo 0); c=$((c+1)); echo $c > `+boots+`
    if [ $c -le 1 ]; then echo 2026-09-24T01:00:00Z; else echo 2026-09-24T01:00:42Z; fi; exit 0 ;;
  *inspect*) echo true; exit 0 ;;
  *" backup "*)
    n=$(cat `+calls+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+calls+`
    if [ $n -le 1 ]; then
      echo '{"error":{"code":"pg.unreachable","message":"cannot connect to PostgreSQL: connection refused"}}'; exit 8
    fi
    echo '{"error":{"code":"internal","message":"manifest commit failed"}}'; exit 1 ;;
esac
exit 0
`)
	oldB, oldM := pgRecoveryBudget, pgRecoveryMaxBackoff
	pgRecoveryBudget, pgRecoveryMaxBackoff = 30*time.Second, 5*time.Millisecond
	defer func() { pgRecoveryBudget, pgRecoveryMaxBackoff = oldB, oldM }()

	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fake,
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
	_, err := d.TakeBackup(context.Background())
	if err == nil || errors.Is(err, ErrCellNotReady) {
		t.Fatalf("the restart happened before the failing attempt; its failure must stay a failure, got: %v", err)
	}
}

// fakeDockerRestore is a container whose `restore` exec fails with
// execOut on stderr. runningAfter is the shell run for every
// State.Running inspect after the first (which says true, so the
// pre-check lets the verify through); bootAfter is what StartedAt says
// after the first read.
func fakeDockerRestore(t *testing.T, runningAfter, bootAfter, execOut string) string {
	t.Helper()
	dir := t.TempDir()
	n := filepath.Join(dir, "n")
	b := filepath.Join(dir, "b")
	return writeFakeDocker(t, `case "$*" in
  *State.StartedAt*)
    c=$(cat `+b+` 2>/dev/null || echo 0); c=$((c+1)); echo $c > `+b+`
    if [ $c -le 1 ]; then echo 2026-09-24T01:00:00Z; else echo `+bootAfter+`; fi; exit 0 ;;
  *State.Running*)
    c=$(cat `+n+` 2>/dev/null || echo 0); c=$((c+1)); echo $c > `+n+`
    if [ $c -le 1 ]; then echo true; exit 0; fi
    `+runningAfter+` ;;
  *" restore "*) echo '`+execOut+`' >&2; exit 1 ;;
esac
exit 0
`)
}

func verifyRuntime(docker string) *DockerCellRuntime {
	return &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: docker,
		AgentBinary: "/usr/bin/pg_hardstorage", Deployment: "db1", RepoURL: "file:///r"}
}

// An inspect that fails after a failed restore proves nothing about the
// container. Treating "could not ask" as "stopped" hid real restore
// failures as verify_skipped_cell_down.
func TestVerifyRestoreUnknownContainerStateStillFails(t *testing.T) {
	d := verifyRuntime(fakeDockerRestore(t, "exit 1", "2026-09-24T01:00:00Z",
		`{"error":{"code":"restore.postverify_failed","message":"cluster did not start"}}`))
	err := d.VerifyRestore(context.Background(), "db1.full.1")
	if err == nil || errors.Is(err, ErrCellNotReady) {
		t.Fatalf("container state unknown; the verify failure must stay a failure, got: %v", err)
	}
}

// dockerd refusing the exec is container state, and it is in the exec's
// OUTPUT — the Go error is only "exit status 1", so matching the error
// text never fired.
func TestVerifyRestoreDaemonRefusalIsCellNotReady(t *testing.T) {
	d := verifyRuntime(fakeDockerRestore(t, "echo true; exit 0", "2026-09-24T01:00:00Z",
		"Error response from daemon: Container 9d6fba02ee7c is restarting, wait until the container is running"))
	err := d.VerifyRestore(context.Background(), "db1.full.1")
	if !errors.Is(err, ErrCellNotReady) {
		t.Fatalf("dockerd refused the exec; must be ErrCellNotReady, got: %v", err)
	}
}

// A container that restarted under the restore killed it: the testbed's
// doing, detected exactly as TakeBackup detects it.
func TestVerifyRestoreContainerRestartIsCellNotReady(t *testing.T) {
	d := verifyRuntime(fakeDockerRestore(t, "echo true; exit 0", "2026-09-24T01:00:42Z", "Killed"))
	err := d.VerifyRestore(context.Background(), "db1.full.1")
	if !errors.Is(err, ErrCellNotReady) {
		t.Fatalf("the container restarted under the restore; must be ErrCellNotReady, got: %v", err)
	}
}

// Docker positively saying the container stopped is still a skip.
func TestVerifyRestoreContainerStoppedIsCellNotReady(t *testing.T) {
	d := verifyRuntime(fakeDockerRestore(t, "echo false; exit 0", "2026-09-24T01:00:00Z", "Killed"))
	err := d.VerifyRestore(context.Background(), "db1.full.1")
	if !errors.Is(err, ErrCellNotReady) {
		t.Fatalf("the container stopped under the restore; must be ErrCellNotReady, got: %v", err)
	}
}

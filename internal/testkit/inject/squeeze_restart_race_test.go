package inject

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func fakeDockerForSqueeze(t *testing.T, restarts bool, updateErr string) *DockerTarget {
	t.Helper()
	dir := t.TempDir()
	n := filepath.Join(dir, "n")
	second := "2026-09-24T01:00:00Z"
	if restarts {
		second = "2026-09-24T01:00:07Z"
	}
	script := `#!/bin/sh
case "$*" in
  *State.StartedAt*)
    c=$(cat ` + n + ` 2>/dev/null || echo 0); c=$((c+1)); echo $c > ` + n + `
    if [ $c -le 1 ]; then echo 2026-09-24T01:00:00Z; else echo ` + second + `; fi ;;
  *State.Running*) echo true ;;
  *update*) echo "Error response from daemon: Cannot update container abc: runc did not terminate successfully: exit status 1: ` + updateErr + `"; exit 1 ;;
esac
exit 0
`
	p := filepath.Join(dir, "docker")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &DockerTarget{Container: "cell-c", DockerBin: p}
}

// The enterprise_heavy soak recorded 27 cgroup_squeeze fault_apply_failed.
// With the errors no longer truncated they read "…/cgroup.controllers:
// no such file or directory" and "…/memory.swap.max: no such device":
// the container's cgroup vanished mid-update because the container was
// restarting. That is the injector's cell-down skip, not a failure.
func TestSqueezeRacingARestartIsTargetNotRunning(t *testing.T) {
	d := fakeDockerForSqueeze(t, true,
		"openat2 /sys/fs/cgroup/system.slice/docker-abc.scope/cgroup.controllers: no such file or directory")
	err := d.SetMemoryLimit(context.Background(), 33554432)
	if !errors.Is(err, ErrTargetNotRunning) {
		t.Fatalf("a squeeze that raced a container restart must be ErrTargetNotRunning, got %v", err)
	}
}

// Without a restart the same kind of refusal stays loud: nothing here
// shows the host can squeeze at all.
func TestSqueezeRefusalWithoutRestartStaysLoud(t *testing.T) {
	d := fakeDockerForSqueeze(t, false,
		`failed to write "33554432": write /sys/fs/cgroup/docker/abc/memory.max: permission denied`)
	err := d.SetMemoryLimit(context.Background(), 33554432)
	if err == nil || errors.Is(err, ErrTargetNotRunning) || errors.Is(err, ErrLimitUnreachable) {
		t.Fatalf("a permission refusal with no restart must be a plain failure, got %v", err)
	}
}

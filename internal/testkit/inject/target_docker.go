// target_docker.go — DockerTarget: production Target backed by `docker exec/kill/cp`.
package inject

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// ErrTargetNotRunning reports that a docker operation could not be
// performed because the container is not running — typically it
// crashed or was OOM-killed before the operation was attempted.
// Callers that only care about the container's resulting state
// (e.g. the signal fault, whose intent is to take the container
// down) can treat this as already-satisfied rather than a failure.
var ErrTargetNotRunning = errors.New("target container is not running")

// DockerTarget fronts a docker container.  Constructed by the
// soak driver from the fleet → container mapping; passes
// through `docker exec`, `docker kill`, `docker cp`.
//
// We do not import the Docker SDK here — `docker` on PATH is
// the universal interface and avoids a Go-module dep that
// drags in cgo-flavoured paths.  Buf returns are bounded by
// the tested container's output volume; soak runs cap each
// fault execution at a few seconds via context.
type DockerTarget struct {
	// Container is the docker container name/id.
	Container string
	// RoleStr is what TargetSet.Pick filters on.
	RoleStr string
	// DockerBin is the docker / podman binary to invoke.
	// Empty defaults to "docker".
	DockerBin string
}

// Name returns the container name.
func (d *DockerTarget) Name() string { return d.Container }

// Role returns the role string.
func (d *DockerTarget) Role() string { return d.RoleStr }

// docker resolves the binary, defaulting to "docker".
func (d *DockerTarget) docker() string {
	if d.DockerBin != "" {
		return d.DockerBin
	}
	return "docker"
}

// Exec runs `docker exec <container> <argv...>` and returns
// combined stdout+stderr.
func (d *DockerTarget) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	full := append([]string{"exec", d.Container}, argv...)
	cmd := exec.CommandContext(ctx, d.docker(), full...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		// A container that has crashed / been OOM-killed / been
		// taken down by an earlier fault makes `docker exec` fail
		// with "... is not running".  Surface the typed sentinel
		// so every Exec-based fault (disk_full, sql, toxiproxy,
		// network_block, ...) reports a pre-existing cell crash
		// cleanly instead of as a cryptic raw-docker failure.
		if strings.Contains(buf.String(), "is not running") {
			return buf.Bytes(), fmt.Errorf("%s: %w", d.Container, ErrTargetNotRunning)
		}
		return buf.Bytes(), fmt.Errorf("docker exec %s %v: %w (output: %s)",
			d.Container, argv, err, truncate(buf.Bytes(), 256))
	}
	return buf.Bytes(), nil
}

// Signal sends a Unix signal via `docker kill -s SIG`.
//
// docker kill maps numeric signals via -s (which also accepts
// names like "TERM"); we always pass the number for
// reproducibility — a numeric signal is unambiguous across
// docker / podman / containerd-shim flavours.
func (d *DockerTarget) Signal(ctx context.Context, sig int) error {
	cmd := exec.CommandContext(ctx, d.docker(),
		"kill", "-s", strconv.Itoa(sig), d.Container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// `docker kill` on a stopped/exited container fails with
		// "... is not running".  Surface that as a typed sentinel
		// so callers can tell "the container is already down"
		// apart from a genuine signal-delivery failure.
		if strings.Contains(string(out), "is not running") {
			return fmt.Errorf("%s: %w", d.Container, ErrTargetNotRunning)
		}
		return fmt.Errorf("docker kill -s %d %s: %w (output: %s)",
			sig, d.Container, err, truncate(out, 256))
	}
	return nil
}

// Start runs `docker start <container>`.  Safe to call on a
// running container — docker treats it as a no-op success.
//
// Why this exists: Docker treats `docker kill` (any signal) as a
// user-initiated stop, so neither `--restart=unless-stopped` nor
// `--restart=always` will auto-restart after our `Signal` call.
// The fault driver (or scenario runner) calls Start to bring the
// container back so subsequent steps see a live PG.
func (d *DockerTarget) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, d.docker(), "start", d.Container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker start %s: %w (output: %s)",
			d.Container, err, truncate(out, 256))
	}
	return nil
}

// CopyOut runs `docker cp <container>:<path> -` and returns
// the tar-stream payload.  For typical chunk-sized files this
// keeps the implementation simple; the soak driver caps the
// expected file size.
func (d *DockerTarget) CopyOut(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.docker(),
		"cp", d.Container+":"+path, "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	body, rerr := io.ReadAll(stdout)
	if werr := cmd.Wait(); werr != nil {
		return body, fmt.Errorf("docker cp %s:%s: %w", d.Container, path, werr)
	}
	return body, rerr
}

// SetMemoryLimit applies a cgroup memory limit via `docker
// update --memory=N --memory-swap=N`.  Bytes <= 0 means "no
// limit" and is encoded as a large sentinel (1 PiB) rather
// than the documented `-1` — see dockerMemoryLimitArg for why.
//
// We can't write /sys/fs/cgroup/memory.max from inside the
// container (Docker mounts cgroupfs read-only by default — the
// soak hit `Read-only file system` on every cell using the
// in-container approach).  `docker update` is the supported
// out-of-band path and works on both cgroup-v1 and v2 hosts.
//
// `--memory-swap` MUST be passed alongside `--memory` when
// lowering the limit, otherwise Docker refuses with:
//
//	Memory limit should be smaller than already set memoryswap
//	limit, update the memoryswap at the same time
//
// (every cgroup_squeeze application in the parallel-soak hit
// this).  Setting memory-swap to the same value as memory
// disables swap inside the cell, which is what the fault wants
// — RSS pressure, not "RSS pressure plus generous swap".  When
// raising the limit (recovery path, bytes <= 0) we set both to
// the sentinel so the container effectively goes back to
// unbounded.
func (d *DockerTarget) SetMemoryLimit(ctx context.Context, bytes int64) error {
	arg := dockerMemoryLimitArg(bytes)
	cmd := exec.CommandContext(ctx, d.docker(),
		"update", "--memory="+arg, "--memory-swap="+arg, d.Container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Same posture as Exec: a down container is a
		// pre-existing cell crash, not a cgroup_squeeze failure.
		if strings.Contains(string(out), "is not running") {
			return fmt.Errorf("%s: %w", d.Container, ErrTargetNotRunning)
		}
		return fmt.Errorf("docker update --memory=%s --memory-swap=%s %s: %w (output: %s)",
			arg, arg, d.Container, err, truncate(out, 256))
	}
	return nil
}

// dockerMemoryLimitArg encodes a byte count for `docker update
// --memory=` / `--memory-swap=`.  bytes <= 0 ("unlimited") is
// encoded as a 1 PiB sentinel for two reasons:
//
//  1. `--memory=-1` was the historical sentinel but modern
//     Docker CLI (verified on 29.x) rejects it with
//     `invalid argument "-1" ... invalid size: '-1'`.
//  2. `--memory=0` is silently a no-op on the `docker update`
//     path — the daemon treats 0 as "leave unchanged" rather
//     than "unlimited", so a recovery that uses 0 leaves the
//     previous squeezed limit in place.
//
// 1 PiB exceeds any realistic host's memory by orders of
// magnitude, so memory.max ending at 2^50 instead of the
// kernel's literal "max" is operationally identical: nothing
// will ever come close to bumping it.
//
// Why this matters: cgroup_squeeze.recovery calls SetMemoryLimit(-1)
// to lift the squeeze.  With the old `-1` arg the call silently
// failed, the container stayed clamped at 32 MiB, and the next
// `pg_hardstorage backup` inside the cell was OOM-killed (exit
// 137 within ~100 ms of starting, empty output).  Three soak
// failures across one parallel-4 / 3 min / 10 cells run traced
// back to this single missed recovery path.
func dockerMemoryLimitArg(bytes int64) string {
	const unlimitedSentinel int64 = 1 << 50 // 1 PiB
	if bytes <= 0 {
		bytes = unlimitedSentinel
	}
	return strconv.FormatInt(bytes, 10)
}

// CopyIn writes body to <container>:<path> via `docker cp -`.
// Body is wrapped in a tar header (handled by docker cp's
// "-" flag) so callers can use it as a flat file write.
func (d *DockerTarget) CopyIn(ctx context.Context, path string, body []byte) error {
	cmd := exec.CommandContext(ctx, d.docker(),
		"cp", "-", d.Container+":"+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := stdin.Write(body); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return err
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("docker cp - %s:%s: %w", d.Container, path, err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

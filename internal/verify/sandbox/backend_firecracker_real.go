//go:build firecracker
// +build firecracker

// Real Firecracker backend, compiled with `-tags firecracker`.
// Boots a microVM via the firecracker-go-sdk, attaches the
// operator-supplied rootfs read-only, attaches the PGDATA
// image as a second drive read-only, runs pg_verifybackup
// inside, captures the serial console, parses the result.
//
// # Rootfs contract
//
// The operator-supplied rootfs (typically built once and
// reused) must contain an init script that:
//
//  1. Mounts /dev/vdb (read-only) at /mnt/pgdata
//  2. Runs `pg_verifybackup -n /mnt/pgdata` (-n: WAL lives in the
//     repo, not the base backup — see verifyBackupArgs)
//  3. Prints exactly one line on /dev/console with the
//     magic prefix:
//
//        __PG_HARDSTORAGE_VERIFY__:OK
//        __PG_HARDSTORAGE_VERIFY__:FAIL <stderr>
//        __PG_HARDSTORAGE_VERIFY__:SKIPPED <reason>
//
//  4. Halts (`reboot -f` is fine — the kernel cmdline has
//     `panic=1 reboot=k` so any exit halts the microVM).
//
// A reference rootfs build script ships under
// `scripts/firecracker-rootfs.sh`; operators with custom
// security baselines can roll their own and just keep the
// magic prefix contract.
//
// # PGDATA image
//
// Firecracker doesn't have a directory-bind primitive; the
// operator must provide DataDir as a path to a block image
// (typically `mkfs.ext4 -d <pgdata> pgdata.img <size>`).
// Auto-image-creation is a v1.5 polish item; for the
// expectation is that the agent (or a `pg_hardstorage verify
// prepare-image` helper) produces the image before calling
// Verify.  The Firecracker backend refuses with a clear
// remediation if DataDir is a directory.

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

func init() {
	register(firecrackerRealBackend{})
}

// firecrackerBuilt is overridden in this build flavour.
func firecrackerBuilt() bool { return true }

// FirecrackerBuilt is the public predicate (overrides the
// stub-flavour symbol).
func FirecrackerBuilt() bool { return firecrackerBuilt() }

// ErrFirecrackerNotBuilt is the real-build counterpart of the
// stub-build sentinel.  It exists only to satisfy the test
// reference at internal/verify/sandbox/sandbox_test.go:93,
// which itself short-circuits via FirecrackerBuilt() before
// ever calling this function under the firecracker tag — so
// the returned value is irrelevant in practice.  Defined as
// nil so the test's `errors.Is(err, sandbox.ErrFirecrackerNotBuilt())`
// is a no-op match (the test never reaches it).  Without
// this stub the test file fails to vet under -tags firecracker
// with "undefined: sandbox.ErrFirecrackerNotBuilt".
func ErrFirecrackerNotBuilt() error { return nil }

// vmStartTimeout caps how long we wait for Firecracker's API
// socket to come up.  Boot itself is sub-second; the timeout
// covers the case where firecracker fails to fork.
var vmStartTimeout = 30 * time.Second

// vmRunTimeout caps the total verify wallclock.  Anything
// over this is interpreted as the rootfs hanging — kill the
// microVM and surface a structured error.  Operators with
// large datasets can crank this up via opts (the Options
// shape doesn't yet expose it; ships with this fixed
// limit).
var vmRunTimeout = 30 * time.Minute

type firecrackerRealBackend struct{}

// Name returns the registered backend identifier.
func (firecrackerRealBackend) Name() string { return BackendFirecracker }

// Verify boots a Firecracker microVM with the restored data dir
// attached, runs pg_verifybackup inside, and returns the structured
// Result. Aborts the VM and returns a partial result if the run
// exceeds vmRunTimeout.
func (firecrackerRealBackend) Verify(ctx context.Context, opts Options) (*Result, error) {
	res := &Result{
		Schema:    SchemaResult,
		Backend:   BackendFirecracker,
		PGMajor:   opts.PGMajor,
		Tool:      "pg_verifybackup",
		StartedAt: time.Now().UTC(),
	}
	defer func() {
		res.StoppedAt = time.Now().UTC()
		res.Duration = res.StoppedAt.Sub(res.StartedAt)
	}()

	if err := validateFirecrackerOpts(opts); err != nil {
		return res, err
	}

	// Temp socket for the firecracker API.  Living in /tmp
	// is safe — the socket has the agent's umask and is
	// unlinked at teardown.
	socketDir, err := os.MkdirTemp("", "pg_hs_fc_*")
	if err != nil {
		return res, fmt.Errorf("sandbox/firecracker: tempdir: %w", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "fc.sock")

	// Capture serial stdout into a thread-safe buffer.  The
	// init script's magic line lands here; we parse after
	// the microVM exits.
	var consoleBuf safeBuffer

	// v27 audit F4: open /dev/null explicitly rather than
	// `os.NewFile(0, "/dev/null")`, which mislabels fd 0 (the
	// agent's own stdin) as if it were /dev/null.  In normal
	// operation firecracker doesn't read stdin, but the
	// previous form would feed firecracker the agent's
	// terminal in interactive runs.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return res, fmt.Errorf("sandbox/firecracker: open /dev/null: %w", err)
	}
	defer devNull.Close()

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(opts.FirecrackerBin).
		WithSocketPath(socketPath).
		WithStdin(devNull).
		WithStdout(&consoleBuf).
		WithStderr(&consoleBuf).
		Build(ctx)

	cfg := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: opts.FirecrackerKernel,
		KernelArgs:      "console=ttyS0 reboot=k panic=1 pci=off init=/init",
		Drives: []models.Drive{
			{
				DriveID:      firecracker.String("rootfs"),
				PathOnHost:   firecracker.String(opts.FirecrackerRootfs),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(true),
			},
			{
				DriveID:      firecracker.String("pgdata"),
				PathOnHost:   firecracker.String(opts.DataDir),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(true),
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(opts.FirecrackerVCPU),
			MemSizeMib: firecracker.Int64(opts.FirecrackerMemMiB),
		},
	}

	startCtx, startCancel := context.WithTimeout(ctx, vmStartTimeout)
	defer startCancel()

	m, err := firecracker.NewMachine(startCtx, cfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return res, fmt.Errorf("sandbox/firecracker: NewMachine: %w", err)
	}
	if err := m.Start(startCtx); err != nil {
		return res, fmt.Errorf("sandbox/firecracker: Start: %w", err)
	}
	// v27 audit F3: explicitly stop the VMM on every exit
	// path.  Without this, a context-deadline-exceeded on
	// m.Wait leaves the firecracker process running and the
	// /dev/kvm slot allocated; the agent leaks a process per
	// timed-out verify.  StopVMM is idempotent — fine to call
	// even after a clean Wait return.
	defer func() { _ = m.StopVMM() }()

	runCtx, runCancel := context.WithTimeout(ctx, vmRunTimeout)
	defer runCancel()

	waitErr := m.Wait(runCtx)

	// `m.Wait` returns nil when the guest halted cleanly,
	// non-nil on context-deadline / killed.  Either way,
	// console output up to the halt is in consoleBuf and we
	// parse it.
	stdout := consoleBuf.String()
	res.Stdout = stripControl(stdout)

	parseRes, perr := parseMagic(stdout)
	if perr != nil {
		// No magic line found; honour wait error if any,
		// else surface the parse failure.
		if waitErr != nil {
			return res, fmt.Errorf("sandbox/firecracker: VM did not produce verify result before exit: %w", waitErr)
		}
		return res, fmt.Errorf("sandbox/firecracker: %w", perr)
	}

	switch parseRes.Verdict {
	case verdictPass:
		res.Passed = true
	case verdictSkip:
		res.Skipped = true
		res.SkipReason = parseRes.Detail
	case verdictFail:
		res.Passed = false
		res.Stderr = parseRes.Detail
	}
	return res, nil
}

// safeBuffer is a tiny mutex-guarded *bytes.Buffer.  The
// Firecracker SDK's process runner writes to it from a
// goroutine while the main goroutine waits on m.Wait; we
// also read it post-wait.  bytes.Buffer is not concurrent-
// safe so we wrap it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer under the mutex.
func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns the accumulated buffer contents under the mutex.
func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

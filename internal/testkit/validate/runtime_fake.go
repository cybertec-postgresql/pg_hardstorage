// runtime_fake.go — in-process fake CellRuntime used by the
// orchestrator's tests.  Records every call and lets the test
// inject per-method failure modes via exported fields.
package validate

import (
	"context"
	"fmt"
	"sync"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

// FakeCellRuntime is the test-only CellRuntime.  Every method
// records into a buffer; tests inject failure modes via the
// SetupErr / DriveErr / etc. fields.
type FakeCellRuntime struct {
	NameStr string

	// SetupFunc, when non-nil, runs INSIDE Setup() before
	// SetupErr is returned.  Tests use it to block / observe
	// concurrent Setup() entry — e.g. asserting that the
	// orchestrator's setup-concurrency semaphore caps the
	// number of cells in Setup() at once.  The cell name is
	// passed through so a single shared func can route on
	// caller.
	SetupFunc func(ctx context.Context, name string) error

	SetupErr     error
	SeedErr      error
	DriveErr     error
	DriveBytes   int64
	BackupErr    error
	BackupID     string
	VerifyErr    error
	VerifyAfterN int // make VerifyRestore fail starting at the Nth call

	// VerifyBlocksUntilCtx, when true, makes VerifyRestore wait
	// on ctx.Done() before returning and returns ctx.Err().
	// Simulates the real-world scenario where the wall-clock
	// deadline cancels an in-flight restore (the docker exec
	// subprocess gets SIGKILL'd) — used to pin the
	// verify_aborted_at_deadline path in the orchestrator.
	VerifyBlocksUntilCtx bool

	FaultErr error

	// Sustained-load and WAL-stream sidecar plumbing.  Tests
	// flip the *Started bools to simulate a runtime that
	// successfully launched its sidecar; *Err fields make the
	// corresponding Start call fail.  The fake doesn't
	// actually run pgbench / wal stream — it just records
	// invocation counts.
	SustainedStarted bool
	SustainedErr     error
	SustainedStats   *report.LoadStats // returned from StopSustainedLoad
	WALStreamStarted bool
	WALStreamErr     error
	WALStreamStats   *report.LoadStats // returned from StopWALStream

	mu                  sync.Mutex
	setupCalls          int
	seedCalls           int
	seedSizeGB          int
	driveCalls          int
	backupCalls         int
	verifyCalls         int
	faultCalls          int
	teardownCalls       int
	startSustainedCalls int
	stopSustainedCalls  int
	startWALCalls       int
	stopWALCalls        int
}

// Name implements CellRuntime.
func (f *FakeCellRuntime) Name() string {
	if f.NameStr == "" {
		return "fake"
	}
	return f.NameStr
}

// Setup bumps the setup-call counter, optionally invokes
// SetupFunc (released-lock hook so tests can block to observe
// concurrent entry), then returns SetupErr.
func (f *FakeCellRuntime) Setup(ctx context.Context) error {
	// Bump the call counter under the lock, then release it
	// before invoking SetupFunc — otherwise tests that block
	// inside SetupFunc to observe concurrent entry deadlock
	// against any other method that also takes f.mu.
	f.mu.Lock()
	f.setupCalls++
	hook := f.SetupFunc
	name := f.Name()
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, name); err != nil {
			return err
		}
	}
	return f.SetupErr
}

// Seed records the requested sizeGB and returns SeedErr.
// Use SeedCalls to assert the orchestrator routed the
// profile's SeedTargetGB through correctly.
func (f *FakeCellRuntime) Seed(_ context.Context, sizeGB int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedCalls++
	f.seedSizeGB = sizeGB
	return f.SeedErr
}

// SeedCalls returns (calls, last requested sizeGB) so tests can
// assert that the orchestrator routed the profile's
// SeedTargetGB into the runtime.
func (f *FakeCellRuntime) SeedCalls() (calls, lastSizeGB int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seedCalls, f.seedSizeGB
}

// DriveLoad returns (DriveBytes, DriveErr) and bumps the
// drive-call counter.
func (f *FakeCellRuntime) DriveLoad(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.driveCalls++
	return f.DriveBytes, f.DriveErr
}

// TakeBackup returns BackupErr if set, otherwise a synthesised
// backup ID (BackupID when non-empty; else
// "<name>.full.<NNNN>").
func (f *FakeCellRuntime) TakeBackup(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backupCalls++
	if f.BackupErr != nil {
		return "", f.BackupErr
	}
	id := f.BackupID
	if id == "" {
		id = fmt.Sprintf("%s.full.%04d", f.Name(), f.backupCalls)
	}
	return id, nil
}

// VerifyRestore honours VerifyBlocksUntilCtx (block on
// ctx.Done), VerifyErr (return immediately), and VerifyAfterN
// (synthesise a failure starting at the Nth call).
func (f *FakeCellRuntime) VerifyRestore(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.verifyCalls++
	blocks := f.VerifyBlocksUntilCtx
	verifyErr := f.VerifyErr
	verifyAfterN := f.VerifyAfterN
	calls := f.verifyCalls
	f.mu.Unlock()
	if blocks {
		<-ctx.Done()
		return ctx.Err()
	}
	if verifyErr != nil {
		return verifyErr
	}
	if verifyAfterN > 0 && calls >= verifyAfterN {
		return fmt.Errorf("fake: verify failed at call %d", calls)
	}
	return nil
}

// ApplyFault returns FaultErr if set, otherwise
// inject.NoRecovery.
func (f *FakeCellRuntime) ApplyFault(_ context.Context, _ string) (inject.Recovery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faultCalls++
	if f.FaultErr != nil {
		return nil, f.FaultErr
	}
	return inject.NoRecovery, nil
}

// Teardown bumps the teardown-call counter and returns nil.
func (f *FakeCellRuntime) Teardown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardownCalls++
	return nil
}

// SnapshotMetadataPaths returns nil — the fake doesn't bundle
// anything for the reproducer.
func (f *FakeCellRuntime) SnapshotMetadataPaths() []string { return nil }

// StartSustainedLoad returns SustainedErr or flips
// SustainedStarted to true.  Tests assert on the field and on
// the start-call counter.
func (f *FakeCellRuntime) StartSustainedLoad(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startSustainedCalls++
	if f.SustainedErr != nil {
		return f.SustainedErr
	}
	f.SustainedStarted = true
	return nil
}

// StopSustainedLoad returns nil when no writer is running,
// else flips SustainedStarted off and returns SustainedStats.
func (f *FakeCellRuntime) StopSustainedLoad(_ context.Context) (*report.LoadStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopSustainedCalls++
	if !f.SustainedStarted {
		return nil, nil
	}
	f.SustainedStarted = false
	return f.SustainedStats, nil
}

// StartWALStream returns WALStreamErr or flips
// WALStreamStarted to true.
func (f *FakeCellRuntime) StartWALStream(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startWALCalls++
	if f.WALStreamErr != nil {
		return f.WALStreamErr
	}
	f.WALStreamStarted = true
	return nil
}

// StopWALStream returns nil when no streamer is running, else
// flips WALStreamStarted off and returns WALStreamStats.
func (f *FakeCellRuntime) StopWALStream(_ context.Context) (*report.LoadStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopWALCalls++
	if !f.WALStreamStarted {
		return nil, nil
	}
	f.WALStreamStarted = false
	return f.WALStreamStats, nil
}

// SidecarCalls returns (startSustained, stopSustained,
// startWAL, stopWAL) so tests can assert the orchestrator
// drove both sidecars in the right order.
func (f *FakeCellRuntime) SidecarCalls() (startSustained, stopSustained, startWAL, stopWAL int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startSustainedCalls, f.stopSustainedCalls, f.startWALCalls, f.stopWALCalls
}

// Calls returns counts so tests can assert how many times each
// method was invoked.
func (f *FakeCellRuntime) Calls() (setup, drive, backup, verify, fault, teardown int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setupCalls, f.driveCalls, f.backupCalls, f.verifyCalls,
		f.faultCalls, f.teardownCalls
}

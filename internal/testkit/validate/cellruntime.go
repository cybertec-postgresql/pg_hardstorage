// Package validate is the soak-driver orchestrator that the
// `pg_hardstorage_testkit validate` subcommand wraps.
//
// The orchestrator runs one iteration loop per cell concurrently
// for the configured duration.  It calls into a CellRuntime to
// do the actual work — drive load, take backup, verify, apply
// fault — so the loop is testable against a fake runtime
// without touching a real PostgreSQL or Docker.
//
// Real soak runs use DockerCellRuntime, which connects to a
// PG host-mapped from a docker-compose container, drives load
// via pgx, and shells out to `pg_hardstorage` for backup +
// restore.  Tests construct FakeCellRuntime, which records
// every call and lets the test simulate failures.
package validate

import (
	"context"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

// CellRuntime is the orchestrator's view of one cell.  Every
// method receives a context the orchestrator can cancel when
// the soak duration elapses.
type CellRuntime interface {
	// Name returns the cell's identifier (matches the
	// fleet entry name).
	Name() string

	// Setup is called once per soak run before the iteration
	// loop kicks off.  Real runtimes use this to verify PG
	// is reachable + initialise the agent.  Tests can no-op.
	Setup(ctx context.Context) error

	// Seed is invoked once after Setup, before the iteration
	// loop.  When sizeGB ≥ 1 the implementation drives the
	// database to roughly sizeGB of on-disk data (for the
	// Docker runtime: `pgbench -i -s <scale>`).  sizeGB == 0
	// is an explicit no-op — the orchestrator calls Seed
	// unconditionally, so cells without a target opt out by
	// receiving 0.  If a runtime-internal SeedTargetGB
	// supersedes the orchestrator-passed value (e.g.
	// DockerCellRuntime reads it from its Profile), the
	// runtime is free to ignore the argument.
	Seed(ctx context.Context, sizeGB int) error

	// DriveLoad runs one batch of workload operations
	// (inserts, updates, selects).  Returns the approximate
	// number of bytes written so the orchestrator can
	// account for total churn.
	DriveLoad(ctx context.Context) (bytesWritten int64, err error)

	// TakeBackup invokes `pg_hardstorage backup`.  Returns
	// the backup ID for later restore-verify.
	TakeBackup(ctx context.Context) (backupID string, err error)

	// VerifyRestore restores backupID into a sandbox and
	// runs pg_verifybackup + an optional pg_amcheck.  Any
	// non-pass return is a fatal failure for the cell.
	VerifyRestore(ctx context.Context, backupID string) error

	// ApplyFault and Recover delegate to the inject
	// registry; the cell knows its target set, so the
	// orchestrator only needs to pass the action string.
	ApplyFault(ctx context.Context, action string) (inject.Recovery, error)

	// Teardown is called after the iteration loop ends
	// (success or failure).  Implementations clean up
	// transient state.
	Teardown(ctx context.Context) error

	// SnapshotMetadataPaths returns the paths the reproducer
	// should bundle if this cell hits a failure.  Typical:
	// the agent's audit log, the failing backup's manifest +
	// attestation.
	SnapshotMetadataPaths() []string

	// StartSustainedLoad launches an UPDATE-heavy background
	// writer (default pgbench TPC-B) running concurrently with
	// the iteration loop.  The orchestrator calls it after
	// Seed and before iter 1 when the profile sets
	// SustainedClients ≥ 1; otherwise this is a no-op.  The
	// writer must remain alive until StopSustainedLoad.  Errors
	// are fatal for the cell — a configured writer that fails
	// to start invalidates the rest of the run's "high load
	// during backup" semantics.
	StartSustainedLoad(ctx context.Context) error

	// StopSustainedLoad stops the writer started by
	// StartSustainedLoad, captures its final TPS / latency
	// report, and returns it via LoadStats.  Idempotent: a
	// runtime that never started a writer returns a nil stats
	// pointer with no error.
	StopSustainedLoad(ctx context.Context) (*report.LoadStats, error)

	// StartWALStream launches a background `pg_hardstorage wal
	// stream` against the cell's PG, archiving WAL into the
	// cell's repo for the duration of the soak.  No-op when
	// the runtime hasn't been told to enable streaming.
	// Errors fatal for the same reason as StartSustainedLoad.
	StartWALStream(ctx context.Context) error

	// StopWALStream stops the streamer started by
	// StartWALStream and returns the final lag (bytes the
	// streamer was behind the primary's WAL position) in
	// LoadStats.  Idempotent.
	StopWALStream(ctx context.Context) (*report.LoadStats, error)
}

// LoopOptions tune the per-cell iteration loop.
type LoopOptions struct {
	// IterationInterval is how long to wait between
	// iterations.  Production: ~10-30s.  Tests: 0.
	IterationInterval time.Duration

	// BackupEvery N iterations.  Default 5.
	BackupEvery int

	// VerifyEvery N iterations (after backup).  Default 25
	// — full restore-verify is expensive.
	VerifyEvery int

	// FaultProbability is the chance a fault is rolled per
	// iteration.  Default 0.2.
	FaultProbability float64

	// HealWindow is how long the orchestrator waits between
	// fault apply and recovery.  Default 30s.
	HealWindow time.Duration
}

// defaults fills LoopOptions with sane production defaults
// where the operator hasn't set them.
//
// FaultProbability and HealWindow have NO library-level default:
// zero is a meaningful operator choice (never fire faults; no
// heal wait — both useful for tests).  The CLI command supplies
// production defaults at the flag level (--fault-rate=0.2 etc).
// IterationInterval is the same — tests pass 0 for fast loops.
func (o *LoopOptions) defaults() {
	if o.BackupEvery == 0 {
		o.BackupEvery = 5
	}
	if o.VerifyEvery == 0 {
		o.VerifyEvery = 25
	}
}

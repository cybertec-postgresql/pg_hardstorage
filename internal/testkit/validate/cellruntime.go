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
	"errors"
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

	// SustainedWriterActive reports whether StartSustainedLoad
	// actually launched a writer, as opposed to no-opping because
	// the profile sets no SustainedClients. Both return nil, so
	// the orchestrator cannot otherwise tell them apart — and it
	// must, because it announces one of them to the event stream.
	SustainedWriterActive() bool

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

	// RetentionInterval is how often the fleet pauses for retention:
	// every cell's deployment is rotated, then the shared repository is
	// garbage-collected once. Default 15m; negative disables. Without it
	// the repository only grows — under enterprise_heavy's sustained
	// writer every backup stores the pages churned since the last one,
	// ~100 GB/h across 8 cells, which no host sustains for 8h.
	//
	// Fleet-wide, not per cell, because the cells share one repository
	// and gc rightly refuses while any backup is in flight (an in-flight
	// backup may have deduplicated against the chunks it would delete):
	// with 8 cells backing up every minute that is almost always. So
	// retention runs in a window: new backups and verifies are held,
	// in-flight ones drain, and the window gives up (deferred, not
	// failed) if they do not within RetentionQuiesceTimeout.
	RetentionInterval time.Duration

	// RetentionQuiesceTimeout bounds how long a retention window waits
	// for in-flight backups and verifies to drain. Default 10m: on a
	// saturated host heavy backups ran 2-7 minutes, so 5m never drained
	// a fleet of 8; still bounded, so one cell waiting out a long PG
	// recovery (up to 30m) does not stall the rest.
	RetentionQuiesceTimeout time.Duration

	// RetentionMaxDeferrals is how many consecutive windows one
	// repository's gc may be deferred — a live backup lease, a fleet
	// that did not drain, no cell up to run it — before the run fails.
	// Default 4 (an hour at the default interval: a killed backup's
	// lease expires well within that); negative never escalates. One
	// deferral is benign; one that never clears is a leaked lease or a
	// stuck backup hiding behind "the next window retries" while the
	// repository grows for the rest of the run.
	RetentionMaxDeferrals int
}

// RetentionApplier is implemented by runtimes that can apply retention
// the way a deployment does: rotate the cell's deployment to a count
// policy, and garbage-collect the repository. Optional, so the fakes
// that implement CellRuntime need not.
type RetentionApplier interface {
	Rotate(ctx context.Context) error
	GC(ctx context.Context) error
	// RepoKey identifies the repository the cell's deployment lives
	// in. Cells with equal keys share one repository, which a window
	// gc's once; cells with a sink of their own have their own.
	RepoKey() string
}

// ErrRetentionDeferred means gc refused to sweep for a documented,
// transient reason — a backup lease is still live (typically one a
// fault killed mid-backup, which expires within its TTL). The next
// retention window retries; it is not a failure.
var ErrRetentionDeferred = errors.New("retention deferred")

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
	if o.RetentionInterval == 0 {
		o.RetentionInterval = 15 * time.Minute
	}
	if o.RetentionQuiesceTimeout == 0 {
		o.RetentionQuiesceTimeout = 10 * time.Minute
	}
	if o.RetentionMaxDeferrals == 0 {
		o.RetentionMaxDeferrals = 4
	}
}

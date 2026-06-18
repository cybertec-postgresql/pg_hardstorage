// orchestrator.go — soak orchestrator: drives fault/recovery loops across compose cells.
package validate

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

// RunOptions wires the orchestrator to its inputs.
type RunOptions struct {
	Project  string
	Seed     int64
	Duration time.Duration
	Loop     LoopOptions
	Faults   *config.Faults
	Cells    []CellRuntime

	// SetupConcurrency caps the number of cells that may run
	// Setup() (initdb + waitForPG) simultaneously.  Once a
	// cell finishes setup it releases its slot for the next
	// cell.  Default 8 — fast enough to parallelise on a
	// generous host, slow enough that a parallel-soak run
	// (multiple slots × 30 cells) doesn't stampede 240
	// initdbs at once and starve every PG of CPU during its
	// own bring-up window.  Iteration bodies AFTER setup
	// run with no concurrency cap; only the bring-up storm
	// is bounded.
	//
	// 0 means "use the default"; pass a negative value to
	// disable throttling entirely (e.g. for tests where
	// every cell is a stub that doesn't really initdb).
	SetupConcurrency int

	// OnEvent receives every Event the loop produces.  Used
	// by the CLI to stream NDJSON to stdout while the soak
	// runs; tests collect into a slice.  Optional — nil
	// disables emission.
	OnEvent func(Event)
}

// defaultSetupConcurrency is the fallback applied when
// RunOptions.SetupConcurrency == 0.  Picked to keep a
// reasonable host responsive during the bring-up storm of
// a 30-cell parallel-soak slot; operators on bigger boxes
// can raise it via the flag.
const defaultSetupConcurrency = 8

// Event is one observable step of the soak.  Carries enough
// detail for the operator to reconstruct what's happening
// without staring at log files.
type Event struct {
	At        time.Time `json:"at"`
	Cell      string    `json:"cell"`
	Iteration int       `json:"iteration,omitempty"`
	Op        string    `json:"op"` // setup_started | drive | backup_started | ...
	Detail    string    `json:"detail,omitempty"`
	Err       string    `json:"err,omitempty"`
}

// Run kicks off the soak.  Each cell runs in its own goroutine.
// Returns when every cell finishes (duration elapsed, fatal
// failure on the cell, or context cancelled by the caller).
func Run(ctx context.Context, opts RunOptions) (*report.Report, error) {
	if len(opts.Cells) == 0 {
		return nil, fmt.Errorf("validate: no cells")
	}
	opts.Loop.defaults()
	if opts.Project == "" {
		opts.Project = "pgvalidate"
	}
	startedAt := time.Now().UTC()
	rep := report.New(opts.Project, opts.Seed, startedAt)

	// Run-wide deadline ctx.  Each cell sees the same.
	runCtx, cancel := context.WithTimeout(ctx, opts.Duration)
	defer cancel()

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		emit        = opts.OnEvent
		cellsByName = map[string]CellRuntime{}
	)
	for _, c := range opts.Cells {
		cellsByName[c.Name()] = c
	}
	if emit == nil {
		emit = func(Event) {}
	}

	emitEvent := func(ev Event) {
		ev.At = time.Now().UTC()
		mu.Lock()
		emit(ev)
		mu.Unlock()
	}

	// Setup-concurrency semaphore.  Bounds the bring-up
	// storm — each cell acquires a slot before Setup() and
	// releases it as soon as Setup() returns (the rest of
	// the iteration body runs unthrottled).  See
	// RunOptions.SetupConcurrency for the rationale.
	setupCap := opts.SetupConcurrency
	if setupCap == 0 {
		setupCap = defaultSetupConcurrency
	}
	var setupSem chan struct{}
	if setupCap > 0 {
		setupSem = make(chan struct{}, setupCap)
	}

	for i, cell := range opts.Cells {
		wg.Add(1)
		go func(idx int, cell CellRuntime) {
			defer wg.Done()
			cr := report.CellReport{
				Name: cell.Name(), Pass: true,
			}
			runCellLoop(runCtx, cell, &cr, opts, emitEvent, setupSem)
			mu.Lock()
			rep.Cells = append(rep.Cells, cr)
			mu.Unlock()
		}(i, cell)
	}
	wg.Wait()

	// Aggregate fault stats across cells.  We didn't track
	// per-prefix counts inside the cell loop (kept it simple);
	// the soak driver's caller can wire that in by intercepting
	// emit.  For v1 we expose totals only.
	for _, c := range rep.Cells {
		rep.FaultStats.TotalApplied += c.FaultsApplied
	}
	// Move any cell-attributed failures into rep.Failures so
	// AddFailure semantics match — but the loop already
	// populates rep.Failures via emitEvent? No — we collect
	// failures cell-side and copy them into rep here.
	mu.Lock()
	failures := pendingFailures
	pendingFailures = nil
	mu.Unlock()
	for _, f := range failures {
		rep.AddFailure(f)
	}

	// Sort cells alphabetically for deterministic report
	// output across runs.
	sort.Slice(rep.Cells, func(i, j int) bool {
		return rep.Cells[i].Name < rep.Cells[j].Name
	})

	rep.Finalize(time.Now().UTC())
	return rep, nil
}

// pendingFailures is a process-global slice the cell loop
// appends to via reportFailure.  We splice it onto the report
// after wg.Wait() so the failures arrive in the report in the
// order they happened; tests reset it via ResetForTesting.
var (
	pendingFailures []report.Failure
	pendingMu       sync.Mutex
)

// ResetForTesting clears any cross-test state.  Tests call this
// in t.Cleanup or at the top of TestMain — exposed so the
// orchestrator's tiny global doesn't leak between cases.
func ResetForTesting() {
	pendingMu.Lock()
	pendingFailures = nil
	pendingMu.Unlock()
}

// reportFailure stores a failure for the orchestrator to splice
// into the final report.  Returns true if this is the cell's
// first failure (so the caller knows to abort the iteration).
func reportFailure(f report.Failure) bool {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	first := true
	for _, existing := range pendingFailures {
		if existing.Cell == f.Cell {
			first = false
			break
		}
	}
	pendingFailures = append(pendingFailures, f)
	return first
}

// runCellLoop is the per-cell goroutine body.  setupSem,
// when non-nil, is the bounded channel that throttles
// concurrent Setup() calls — see RunOptions.SetupConcurrency
// for why.  We acquire BEFORE emitting setup_started so the
// "started" event reflects when the cell actually begins
// initdb, not when it joined the queue.  Released as soon
// as Setup() returns (success or failure) so the next
// queued cell can begin.
func runCellLoop(
	ctx context.Context,
	cell CellRuntime,
	cr *report.CellReport,
	opts RunOptions,
	emit func(Event),
	setupSem chan struct{},
) {
	rng := rand.New(rand.NewSource(opts.Seed ^ hashName(cell.Name())))
	cr.UpFor = 0
	startedAt := time.Now()

	if setupSem != nil {
		// Block until a slot is available or the run is
		// cancelled.  Cancellation aborts the wait without
		// emitting setup_started, so a cancelled queue
		// doesn't fill the report with phantom failures.
		select {
		case setupSem <- struct{}{}:
			// Held until Setup() returns; release happens
			// AFTER the Setup() call below so the next
			// queued cell can begin initdb without waiting
			// for this cell's entire iteration loop to end.
			// Using defer would hold the slot for the cell's
			// full lifetime, defeating the throttle.
		case <-ctx.Done():
			cr.Pass = false
			cr.FirstFailureMsg = "cancelled before setup: " + ctx.Err().Error()
			return
		}
	}

	releaseSetupSlot := func() {
		if setupSem == nil {
			return
		}
		select {
		case <-setupSem:
		default:
		}
	}

	emit(Event{Cell: cr.Name, Op: "setup_started"})
	if err := cell.Setup(ctx); err != nil {
		releaseSetupSlot()
		cr.Pass = false
		cr.FirstFailureMsg = "setup failed: " + err.Error()
		emit(Event{Cell: cr.Name, Op: "setup_failed", Err: err.Error()})
		reportFailure(report.Failure{
			At: time.Now().UTC(), Cell: cr.Name, Iteration: 0,
			Kind: "setup", Message: err.Error(),
		})
		return
	}
	releaseSetupSlot()
	emit(Event{Cell: cr.Name, Op: "setup_ok"})
	defer func() {
		_ = cell.Teardown(context.Background())
		cr.UpFor = time.Since(startedAt)
	}()

	// Optional bulk seed.  The cell honours its own
	// Profile.SeedTargetGB; passing 0 leaves seeding fully
	// driven by the runtime's profile.  Failure here is fatal
	// for the cell — without the seeded data the iteration
	// loop's assertions wouldn't be meaningful.
	emit(Event{Cell: cr.Name, Op: "seed_started"})
	if err := cell.Seed(ctx, 0); err != nil {
		cr.Pass = false
		cr.FirstFailureMsg = "seed failed: " + err.Error()
		emit(Event{Cell: cr.Name, Op: "seed_failed", Err: err.Error()})
		reportFailure(report.Failure{
			At: time.Now().UTC(), Cell: cr.Name, Iteration: 0,
			Kind: "seed", Message: err.Error(),
		})
		return
	}
	emit(Event{Cell: cr.Name, Op: "seed_ok"})

	// Sidecars: an UPDATE-heavy background writer + a
	// continuous WAL streamer.  Both are no-ops when the
	// profile doesn't enable them; their lifecycle is
	// bracketed by the deferred Stop calls so they wind down
	// cleanly even on a fatal failure inside the iteration
	// loop.  Order matters on stop: WAL stream first, so the
	// final WAL-lag sample is taken while the writer is
	// still pushing transactions through.
	if err := cell.StartSustainedLoad(ctx); err != nil {
		cr.Pass = false
		cr.FirstFailureMsg = "sustained load failed to start: " + err.Error()
		emit(Event{Cell: cr.Name, Op: "sustained_load_failed", Err: err.Error()})
		reportFailure(report.Failure{
			At: time.Now().UTC(), Cell: cr.Name, Iteration: 0,
			Kind: "sustained_load", Message: err.Error(),
		})
		return
	}
	emit(Event{Cell: cr.Name, Op: "sustained_load_started"})
	if err := cell.StartWALStream(ctx); err != nil {
		cr.Pass = false
		cr.FirstFailureMsg = "wal stream failed to start: " + err.Error()
		emit(Event{Cell: cr.Name, Op: "wal_stream_failed", Err: err.Error()})
		reportFailure(report.Failure{
			At: time.Now().UTC(), Cell: cr.Name, Iteration: 0,
			Kind: "wal_stream", Message: err.Error(),
		})
		return
	}
	emit(Event{Cell: cr.Name, Op: "wal_stream_started"})
	defer func() {
		// Use a fresh context so an already-cancelled soak ctx
		// doesn't bypass the stop sequence.  Both helpers
		// merge their stats into cr.LoadStats so the report
		// gets a single composed picture.
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if s, _ := cell.StopWALStream(stopCtx); s != nil {
			mergeLoadStats(cr, s)
		}
		if s, _ := cell.StopSustainedLoad(stopCtx); s != nil {
			mergeLoadStats(cr, s)
		}
	}()

	var lastBackupID string
	for iter := 1; ; iter++ {
		// Check ctx.Done before each iteration; the loop
		// exits cleanly when the soak duration elapses.
		select {
		case <-ctx.Done():
			cr.LastIteration = iter - 1
			emit(Event{Cell: cr.Name, Op: "duration_elapsed",
				Iteration: iter - 1})
			return
		default:
		}

		emit(Event{Cell: cr.Name, Op: "iter_start", Iteration: iter})
		cr.IterationsRun = iter
		cr.LastIteration = iter

		// 1. Drive load.
		if _, err := cell.DriveLoad(ctx); err != nil {
			emit(Event{Cell: cr.Name, Op: "drive_failed",
				Iteration: iter, Err: err.Error()})
			// Non-fatal: continue.  The fault catalogue may
			// be the root cause and the next backup will
			// surface it.
		}

		// 2. Maybe inject a fault.
		if rng.Float64() < opts.Loop.FaultProbability && opts.Faults != nil &&
			len(opts.Faults.Faults) > 0 {
			fault := pickWeighted(opts.Faults.Faults, rng)
			emit(Event{Cell: cr.Name, Op: "fault_apply",
				Iteration: iter, Detail: fault.Action})
			recovery, err := cell.ApplyFault(ctx, fault.Action)
			switch {
			case err != nil && errors.Is(err, inject.ErrTargetNotRunning):
				// A fault that fires on a cell a PRIOR fault already
				// downed (kill/OOM/SIGTERM) can't be applied — you
				// can't fill the disk of a stopped container or pause
				// a dead PG's archiver. That's a benign race, not a
				// fault-injection failure (signal/cgroup_squeeze
				// already swallow it internally; disk_full /
				// pause_archive surface the typed sentinel). Record it
				// as a skip, mirroring backup_skipped_cell_down /
				// verify_skipped_cell_down, so it doesn't pollute the
				// fault_apply_failed signal the soak triages on.
				emit(Event{Cell: cr.Name, Op: "fault_skipped_cell_down",
					Iteration: iter, Detail: fault.Action})
			case err != nil:
				emit(Event{Cell: cr.Name, Op: "fault_apply_failed",
					Iteration: iter, Detail: fault.Action, Err: err.Error()})
				// Continue — the heal window doesn't
				// fire and the cell may still recover
				// on its own.
			default:
				cr.FaultsApplied++
				select {
				case <-ctx.Done():
				case <-time.After(opts.Loop.HealWindow):
				}
				if recovery != nil {
					if rerr := recovery(ctx); rerr != nil {
						if ctx.Err() != nil {
							// Run-wide deadline elapsed: the
							// recovery's docker calls were
							// cancelled by orchestrator shutdown,
							// not by a genuine fault-cleanup
							// failure.  Mirrors the
							// backup_aborted_at_deadline path —
							// without this distinction every
							// soak whose 4-min timer happens to
							// land inside a heal window reports
							// alarming "recovery_failed" lines
							// for what is just teardown.
							emit(Event{Cell: cr.Name,
								Op:        "recovery_aborted_at_deadline",
								Iteration: iter, Err: rerr.Error()})
						} else {
							emit(Event{Cell: cr.Name,
								Op:        "recovery_failed",
								Iteration: iter, Err: rerr.Error()})
						}
					}
				}
				emit(Event{Cell: cr.Name, Op: "fault_recovered",
					Iteration: iter, Detail: fault.Action})
			}
		}

		// 3. Backup every N iterations.
		if iter%opts.Loop.BackupEvery == 0 {
			emit(Event{Cell: cr.Name, Op: "backup_started", Iteration: iter})
			cr.BackupsTaken++
			id, err := cell.TakeBackup(ctx)
			switch {
			case err != nil && ctx.Err() != nil:
				// Run-wide deadline elapsed mid-backup; the
				// orchestrator is shutting down and any
				// SIGKILL'd in-flight backup is a runner
				// artefact, not a system failure.  Roll back
				// the optimistic BackupsTaken increment so
				// the report doesn't double-count an attempt
				// that never had a fair chance to complete.
				cr.BackupsTaken--
				cr.LastIteration = iter - 1
				emit(Event{Cell: cr.Name, Op: "backup_aborted_at_deadline",
					Iteration: iter, Err: err.Error()})
				return
			case errors.Is(err, ErrCellNotReady):
				// A fault knocked the cell offline and
				// recovery hasn't completed; skip this
				// dispatch rather than counting it as a
				// failure.  The next iteration retries.
				cr.BackupsTaken--
				emit(Event{Cell: cr.Name, Op: "backup_skipped_cell_down",
					Iteration: iter})
			case err != nil:
				cr.BackupsFailed++
				emit(Event{Cell: cr.Name, Op: "backup_failed",
					Iteration: iter, Err: err.Error()})
				if reportFailure(report.Failure{
					At: time.Now().UTC(), Cell: cr.Name, Iteration: iter,
					Kind: "backup", Message: err.Error(),
				}) {
					cr.Pass = false
					cr.FirstFailureMsg = "backup failed: " + err.Error()
					return
				}
			default:
				lastBackupID = id
				emit(Event{Cell: cr.Name, Op: "backup_completed",
					Iteration: iter, Detail: id})
			}
		}

		// 4. Restore-verify every M iterations (only if we
		// have a backup to verify).
		if iter%opts.Loop.VerifyEvery == 0 && lastBackupID != "" {
			emit(Event{Cell: cr.Name, Op: "verify_started",
				Iteration: iter, Detail: lastBackupID})
			cr.RestoresAttempted++
			err := cell.VerifyRestore(ctx, lastBackupID)
			switch {
			case err != nil && ctx.Err() != nil:
				// Run-wide deadline elapsed mid-verify; the
				// orchestrator is shutting down and any
				// SIGKILL'd in-flight restore is a runner
				// artefact, not a system failure.  Mirrors
				// the backup_aborted_at_deadline path above
				// — without this, a 10-min soak that fires
				// verifies near the end racks up bogus
				// verify_failed entries (signal: killed,
				// empty output) once the wall-clock timer
				// cancels the docker exec subprocess.  Roll
				// back the optimistic RestoresAttempted
				// increment so the report doesn't count an
				// attempt that never had a fair chance to
				// complete.
				cr.RestoresAttempted--
				cr.LastIteration = iter - 1
				emit(Event{Cell: cr.Name, Op: "verify_aborted_at_deadline",
					Iteration: iter, Err: err.Error()})
				return
			case errors.Is(err, ErrCellNotReady):
				// A fault knocked the cell offline and recovery
				// hasn't completed; skip this verify rather than
				// counting it as a failure — mirrors the
				// backup_skipped_cell_down path above.  The next
				// verify-eligible iteration retries.
				cr.RestoresAttempted--
				emit(Event{Cell: cr.Name, Op: "verify_skipped_cell_down",
					Iteration: iter})
			case err != nil:
				cr.RestoresFailed++
				emit(Event{Cell: cr.Name, Op: "verify_failed",
					Iteration: iter, Err: err.Error()})
				if reportFailure(report.Failure{
					At: time.Now().UTC(), Cell: cr.Name, Iteration: iter,
					Kind: "verify", Message: err.Error(),
				}) {
					cr.Pass = false
					cr.FirstFailureMsg = "verify failed: " + err.Error()
					return
				}
			default:
				emit(Event{Cell: cr.Name, Op: "verify_ok",
					Iteration: iter, Detail: lastBackupID})
			}
		}

		// 5. Optional sleep between iterations.
		if opts.Loop.IterationInterval > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(opts.Loop.IterationInterval):
			}
		}
	}
}

// pickWeighted picks a fault using the catalogue's Weight as
// a probability mass.  All-zero weights treat every fault
// uniformly.
func pickWeighted(faults []config.Fault, rng *rand.Rand) config.Fault {
	total := 0
	for _, f := range faults {
		if f.Weight > 0 {
			total += f.Weight
		}
	}
	if total == 0 {
		return faults[rng.Intn(len(faults))]
	}
	pick := rng.Intn(total)
	for _, f := range faults {
		if f.Weight <= 0 {
			continue
		}
		if pick < f.Weight {
			return f
		}
		pick -= f.Weight
	}
	return faults[len(faults)-1] // unreachable
}

// mergeLoadStats folds Stop{SustainedLoad,WALStream} output
// into the cell report.  We merge rather than overwrite
// because the two sidecars contribute disjoint fields
// (sustained writer → TPS / latency / WAL bytes; WAL streamer
// → lag), and either may be enabled independently.
func mergeLoadStats(cr *report.CellReport, src *report.LoadStats) {
	if src == nil {
		return
	}
	if cr.LoadStats == nil {
		cr.LoadStats = &report.LoadStats{}
	}
	dst := cr.LoadStats
	if src.TPSAvg != 0 {
		dst.TPSAvg = src.TPSAvg
	}
	if src.LatencyP95Ms != 0 {
		dst.LatencyP95Ms = src.LatencyP95Ms
	}
	if src.WALBytesWritten != 0 {
		dst.WALBytesWritten = src.WALBytesWritten
	}
	if src.WALStreamLagBytes != 0 {
		dst.WALStreamLagBytes = src.WALStreamLagBytes
	}
	if src.WALRepoLagBytes != 0 {
		dst.WALRepoLagBytes = src.WALRepoLagBytes
	}
	if src.WALSegmentsCommitted != 0 {
		dst.WALSegmentsCommitted = src.WALSegmentsCommitted
	}
	if src.SustainedWriterRan {
		dst.SustainedWriterRan = true
	}
	if src.WALStreamRan {
		dst.WALStreamRan = true
	}
}

// hashName produces a stable per-cell uint64 used to mix into
// the shared seed so different cells take different fault
// trajectories.
func hashName(s string) int64 {
	h := uint64(1469598103934665603)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return int64(h)
}

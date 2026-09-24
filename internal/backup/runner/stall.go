// stall.go — the --stall-timeout watchdog and the abort-cause mapping.
package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// ErrIOStarved is the cancel cause the stall watchdog aborts a backup
// with. Take maps it to the backup.io_starved error code.
var ErrIOStarved = errors.New("backup.io_starved")

// stallWatchdogTick is how often the watchdog samples. The granularity
// is fine because StallTimeout is on the order of minutes; tests shrink
// it.
var stallWatchdogTick = 30 * time.Second

// stallWatchdog cancels its context with ErrIOStarved when touch has
// not been called for longer than timeout.
type stallWatchdog struct {
	mu      sync.Mutex
	last    time.Time
	timeout time.Duration
}

func (w *stallWatchdog) touch() {
	w.mu.Lock()
	w.last = time.Now()
	w.mu.Unlock()
}

// startStallWatchdog derives a context the watchdog cancels on a stall.
// The returned stop func releases it (and must be called).
func startStallWatchdog(ctx context.Context, timeout time.Duration) (context.Context, *stallWatchdog, func()) {
	w := &stallWatchdog{last: time.Now(), timeout: timeout}
	wctx, cancel := context.WithCancelCause(ctx)
	go func() {
		ticker := time.NewTicker(stallWatchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				stalled := time.Since(w.last)
				w.mu.Unlock()
				if stalled > timeout {
					cancel(fmt.Errorf("%w: no progress for %s (StallTimeout=%s) — "+
						"likely host disk saturation; check `iostat -x 1` and reduce concurrent backup load",
						ErrIOStarved, stalled.Round(time.Second), timeout))
					return
				}
			}
		}
	}()
	return wctx, w, func() { cancel(nil) }
}

// progressSink resets the stall watchdog on every frame BASE_BACKUP
// delivers. Events alone are not progress: none is emitted between
// "started" and "stream_complete", so a healthy backup that streamed
// for longer than --stall-timeout used to be killed as io_starved.
type progressSink struct {
	basebackup.Sink
	touch func()
}

func (s progressSink) OnTablespaceStart(idx int, info basebackup.TablespaceInfo) error {
	s.touch()
	return s.Sink.OnTablespaceStart(idx, info)
}

func (s progressSink) OnTablespaceData(idx int, data []byte) error {
	s.touch()
	return s.Sink.OnTablespaceData(idx, data)
}

func (s progressSink) OnTablespaceEnd(idx int) error {
	s.touch()
	return s.Sink.OnTablespaceEnd(idx)
}

// abortCauseError replaces a bare "context canceled" with the reason
// the backup was actually aborted. The stall watchdog and the lease
// maintainer cancel with a cause; the pipeline below only ever sees
// ctx.Err(), so without this Take reported "context canceled" for a
// starved disk or a stolen lease — indistinguishable from Ctrl-C.
func abortCauseError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(cause, ErrIOStarved):
		return output.NewError("backup.io_starved", cause.Error()).
			WithSuggestion(&output.Suggestion{
				Human: "the backup made no progress within --stall-timeout; check host disk/network saturation (`iostat -x 1`) or raise --stall-timeout",
			}).Wrap(cause)
	case errors.Is(cause, backup.ErrLeaseLost):
		return output.NewError("conflict.backup_lease_lost",
			fmt.Sprintf("backup aborted: %v — another process now holds this deployment's backup lease", cause)).
			WithSuggestion(&output.Suggestion{
				Human: "another backup of this deployment took over the lease (a clock jump or a stall longer than the lease TTL); check for an overlapping scheduler before retrying",
			}).Wrap(cause)
	}
	return fmt.Errorf("%w (aborted: %v)", err, cause)
}

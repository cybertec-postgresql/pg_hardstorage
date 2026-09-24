package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
)

type nopSink struct{}

func (nopSink) OnTablespaceStart(int, basebackup.TablespaceInfo) error { return nil }
func (nopSink) OnTablespaceData(int, []byte) error                     { return nil }
func (nopSink) OnTablespaceEnd(int) error                              { return nil }

// The --stall-timeout watchdog was reset only by emitted events, and
// none is emitted while BASE_BACKUP streams — so any healthy backup that
// streamed for longer than the timeout was killed as io_starved. Frames
// arriving through the sink must count as progress; silence must still
// trip it.
func TestStallWatchdog_StreamFramesAreProgress(t *testing.T) {
	old := stallWatchdogTick
	stallWatchdogTick = 5 * time.Millisecond
	defer func() { stallWatchdogTick = old }()

	ctx, wd, stop := startStallWatchdog(context.Background(), 60*time.Millisecond)
	defer stop()
	sink := progressSink{Sink: nopSink{}, touch: wd.touch}

	// Stream for ~5x the timeout, one frame every 15ms, no events.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := sink.OnTablespaceData(0, []byte("frame")); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatalf("watchdog killed a backup that was streaming: %v", context.Cause(ctx))
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Now go silent: it must trip, with the io_starved cause.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never tripped on a real stall")
	}
	if !errors.Is(context.Cause(ctx), ErrIOStarved) {
		t.Fatalf("cause = %v, want ErrIOStarved", context.Cause(ctx))
	}
}

// Take returned "context canceled" for a watchdog or lease-loss abort —
// indistinguishable from Ctrl-C, and mapped to the wrong exit code. The
// cancel cause must surface as the documented code.
func TestAbortCauseError_MapsCancelCauses(t *testing.T) {
	pipelineErr := func(ctx context.Context) error { return errors.Join(errors.New("backup: BASE_BACKUP"), ctx.Err()) }

	for _, tc := range []struct {
		name  string
		cause error
		code  string
		exit  output.ExitCode
	}{
		{"stall", errors.Join(ErrIOStarved, errors.New("no progress for 5m")), "backup.io_starved", output.ExitError},
		{"lease", errors.Join(errors.New("backup lease lost"), backup.ErrLeaseLost), "conflict.backup_lease_lost", output.ExitConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(tc.cause)
			err := abortCauseError(ctx, pipelineErr(ctx))
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
			if got := output.ExitCodeFor(err); got != tc.exit {
				t.Errorf("exit = %d, want %d", got, tc.exit)
			}
		})
	}

	// A plain cancel (Ctrl-C) keeps the original error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orig := pipelineErr(ctx)
	if got := abortCauseError(ctx, orig); got != orig {
		t.Errorf("plain cancel rewritten: %v", got)
	}
}

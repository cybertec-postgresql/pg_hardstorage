package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression (concurrency audit): lease LOSS must abort the backup —
// the old Maintain callback only emitted a warning, so two backups ran
// to completion after a lease was reclaimed. Transient renew errors
// must stay warning-only (no abort).
func TestLeaseLossAborter_AbortsOnLossOnly(t *testing.T) {
	var events []*output.Event
	emit := func(ev *output.Event) { events = append(events, ev) }

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cb := leaseLossAborter("db1", emit, cancel)

	// Transient renew failure: warning, ctx stays alive.
	cb(errors.New("transient: repo blip"))
	if ctx.Err() != nil {
		t.Fatalf("transient renew error aborted the backup: %v", context.Cause(ctx))
	}
	if len(events) != 1 || events[0].Op != "lease_renew_failed" || events[0].Severity != output.SeverityWarning {
		t.Fatalf("transient error event = %+v, want warning lease_renew_failed", events[0])
	}

	// Lease lost: CRITICAL event + backup ctx cancelled with the cause.
	cb(backup.ErrLeaseLost)
	if ctx.Err() == nil {
		t.Fatal("lease loss did NOT abort the backup (old warning-only regression)")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, backup.ErrLeaseLost) {
		t.Errorf("cancel cause = %v, want ErrLeaseLost", cause)
	}
	last := events[len(events)-1]
	if last.Op != "lease_lost" || last.Severity != output.SeverityCritical {
		t.Errorf("loss event = op %q sev %v, want critical lease_lost", last.Op, last.Severity)
	}
}

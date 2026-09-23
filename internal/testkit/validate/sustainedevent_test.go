package validate_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// The soak's event stream is what a human reads to decide whether a
// run exercised what it claims. v1.4 shipped with that stream
// overstating reality — the WAL-stream sidecar was dead for most of a
// run and nothing said so — and the release notes for that fix are
// explicit that the report must distinguish "was interrupted" from
// "fell behind".
//
// The same shape was still present for the sustained writer.
// StartSustainedLoad is a documented no-op when the profile leaves
// SustainedClients at 0, and it signals that by returning nil, which
// is exactly what a successfully-launched writer returns. The
// orchestrator announced "sustained_load_started" for both.
//
// An 8 h, 21-cell run on the default oltp_smoke profile therefore
// logged 21 sustained_load_started events while running no sustained
// writer at all. Nothing was broken — oltp_smoke sets no
// sustained_clients on purpose, and the report's own Writer column
// correctly read "—" — but the event stream said otherwise, and the
// event stream is the thing people grep.

func TestSustainedLoadSkippedIsNotReportedAsStarted(t *testing.T) {
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)

	// SustainedStarted stays false: the profile asked for no writer.
	cell := &validate.FakeCellRuntime{NameStr: "quiet", SustainedNoop: true}

	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 120 * time.Millisecond,
		Loop: validate.LoopOptions{
			IterationInterval: 5 * time.Millisecond,
			BackupEvery:       99,
			FaultProbability:  0,
		},
		Cells:   []validate.CellRuntime{cell},
		OnEvent: emit,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var started, skipped int
	for _, e := range *events {
		switch e.Op {
		case "sustained_load_started":
			started++
		case "sustained_load_skipped":
			skipped++
		}
	}
	if started != 0 {
		t.Errorf("no writer ran, yet %d sustained_load_started event(s) were emitted — "+
			"the event stream must not claim a writer that a zero-SustainedClients "+
			"profile never launched", started)
	}
	if skipped != 1 {
		t.Errorf("expected exactly one sustained_load_skipped event, got %d — "+
			"a skipped writer has to be visible, not merely unannounced", skipped)
	}
}

// The other direction: a writer that really starts must still be
// announced, or the fix trades one silence for another.
func TestSustainedLoadStartedIsStillReported(t *testing.T) {
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)

	cell := &validate.FakeCellRuntime{NameStr: "busy"}

	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 120 * time.Millisecond,
		Loop: validate.LoopOptions{
			IterationInterval: 5 * time.Millisecond,
			BackupEvery:       99,
			FaultProbability:  0,
		},
		Cells:   []validate.CellRuntime{cell},
		OnEvent: emit,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var started int
	for _, e := range *events {
		if e.Op == "sustained_load_started" {
			started++
		}
	}
	if started != 1 {
		t.Errorf("a writer that actually started must be announced exactly once, got %d", started)
	}
}

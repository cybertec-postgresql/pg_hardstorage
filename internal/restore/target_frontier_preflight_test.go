package restore

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testfixture"
)

// An LSN target past the end of the archived WAL cannot be reached:
// PostgreSQL replays to the end of the archive and refuses to start.
// timetravel create --at FFFFFFFF/FFFFFFFF built such a session (found
// by the L2_timetravel_lifecycle scenario once it ran against a real
// backup). Refuse up front — and only then.
func TestTargetBeyondArchivePreflight(t *testing.T) {
	ctx := context.Background()
	sp := gapTestRepo(t)
	dep := gapTestDeployment
	m := manifestWith("base/1/1259") // stop 0/3000120, timeline 1
	rec := func(lsn string) *Recovery { return &Recovery{Enable: true, TargetLSN: lsn} }

	// Nothing archived at all: a target past the stop is unreachable.
	err := preflightTargetBeyondArchive(ctx, sp, dep, m, rec("0/3500000"), false)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "restore.target_unreachable" {
		t.Fatalf("nothing archived: want restore.target_unreachable, got %v", err)
	}

	testfixture.PlantArchivedWAL(t, sp, dep, 1) // segment 3: archive ends at 0/4000000
	for _, c := range []struct {
		lsn     string
		refused bool
	}{
		{"0/3000120", false},        // the stop itself: CheckTargetReachable's domain
		{"0/3500000", false},        // inside the archive
		{"0/3FFFFFF", false},        // last archived byte
		{"0/4000000", true},         // first byte not archived
		{"FFFFFFFF/FFFFFFFF", true}, // timetravel's scenario value
	} {
		err := preflightTargetBeyondArchive(ctx, sp, dep, m, rec(c.lsn), false)
		if got := err != nil; got != c.refused {
			t.Errorf("target %s: refused=%v, want %v (%v)", c.lsn, got, c.refused, err)
		}
	}
	if err := preflightTargetBeyondArchive(ctx, sp, dep, m, rec("FFFFFFFF/FFFFFFFF"), true); err != nil {
		t.Errorf("--skip-gap-check must override: %v", err)
	}
	if err := preflightTargetBeyondArchive(ctx, sp, dep, m, &Recovery{Enable: true}, false); err != nil {
		t.Errorf("an unbounded recovery has no LSN target to check: %v", err)
	}
	// A later timeline may carry WAL past this one's end: do not refuse.
	testfixture.PlantArchivedWAL(t, sp, dep, 2)
	if err := preflightTargetBeyondArchive(ctx, sp, dep, m, rec("0/4000000"), false); err != nil {
		t.Errorf("with WAL on a later timeline the target may be reachable: %v", err)
	}
}

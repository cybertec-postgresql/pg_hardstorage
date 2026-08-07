package cli

// wal_history_capture_test.go — somebody has to WRITE the timeline
// history file.
//
// wal_history_fetch_test.go covers serving `<tli>.history` to PG's
// restore_command, and it plants the file by hand. That was the whole
// story on the read side and it hid a hole on the write side: in a
// `wal stream`-only deployment nothing produced the file at all.
//
// The only other producer, internal/wal/follower.Coordinator, runs
// solely under `agent` with a Patroni URL configured. A streaming-only
// HA setup — the posture the docs describe — runs neither that nor an
// archive_command, so the follower store was the only possible copy and
// it stayed empty.
//
// The consequence is the quietest kind. Our default
// recovery_target_timeline is 'latest', which makes PG follow the
// highest timeline it can resolve a history file FOR. A missing file
// therefore does not fail the restore: PG silently recovers along the
// pre-failover timeline and promotes a database missing everything
// written after the promotion. The operator asked for latest and got
// less, and nothing anywhere said so.

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// TestCaptureStreamTimelineHistory_SkipsTimelineOne: TLI 1 has no
// parent, so PG has no history file for it and its absence is normal.
// Warning about it would train operators to ignore the warning that
// matters.
func TestCaptureStreamTimelineHistory_SkipsTimelineOne(t *testing.T) {
	sp, _ := newFsRepo(t)
	d, buf := captureDispatcher(t)

	captureStreamTimelineHistory(context.Background(), d, sp,
		walStreamOptions{deployment: "db1", pgConn: "postgres://unreachable:1/x"}, 1)

	if got := buf.String(); strings.Contains(got, "history_not_captured") {
		t.Errorf("warned about a missing history file for timeline 1:\n%s\n\n"+
			"TLI 1 has no parent and PG has no .history for it. A warning here is noise, "+
			"and noise is what makes the real one get ignored.", got)
	}
}

// TestCaptureStreamTimelineHistory_UnreachablePGWarnsAndContinues is
// the posture check.
//
// Best-effort is right: refusing to stream because a history file could
// not be stored would trade a PITR limitation for losing all subsequent
// WAL. But best-effort must not mean silent — a dropped capture is a
// restore that will quietly land on the wrong timeline, and the only
// moment anyone can act on it is now.
func TestCaptureStreamTimelineHistory_UnreachablePGWarnsAndContinues(t *testing.T) {
	sp, _ := newFsRepo(t)
	d, buf := captureDispatcher(t)

	// The call must return rather than block or panic; the stream
	// depends on it not being fatal.
	captureStreamTimelineHistory(context.Background(), d, sp,
		walStreamOptions{deployment: "db1", pgConn: "postgres://127.0.0.1:1/x"}, 2)

	got := buf.String()
	if !strings.Contains(got, "history_not_captured") {
		t.Fatalf("no warning emitted when the capture failed:\n%s\n\n"+
			"Streaming continuing is correct. Continuing SILENTLY is not: the operator's "+
			"next PITR across this failover will recover along an earlier timeline and "+
			"report success.", got)
	}
	// The warning has to explain the consequence, not just the error.
	// "could not connect" tells nobody that their restore is at risk.
	for _, want := range []string{"recovery_target_timeline", "EARLIER timeline"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, got)
		}
	}
}

// TestCaptureStreamTimelineHistory_NoConnDoesNothing: the unit-test and
// --no-slot paths can reach this with an empty DSN, and it must not
// warn about a capture it was never in a position to make.
func TestCaptureStreamTimelineHistory_NoConnDoesNothing(t *testing.T) {
	sp, _ := newFsRepo(t)
	d, buf := captureDispatcher(t)

	captureStreamTimelineHistory(context.Background(), d, sp,
		walStreamOptions{deployment: "db1"}, 3)

	if got := buf.String(); strings.Contains(got, "history_not_captured") {
		t.Errorf("warned with no PG connection configured:\n%s", got)
	}
}

// TestTimelineStoreRoundTrip pins the store contract the capture relies
// on: what is written under a decimal TLI is what fetchAuxBody reads
// back for the hex archive name PG asks for.
//
// The two sides are keyed differently — PG asks for `00000002.history`,
// the store holds `wal/<dep>/timelines/2.history` — and this is the
// join. wal_history_fetch_test.go proves the read half against a
// hand-planted file; this proves a real Put lands where that read looks.
func TestTimelineStoreRoundTrip(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	want := []byte("1\t0/3000000\tno recovery target specified\n")

	if err := timeline.New(sp).Put(context.Background(), dep, 2, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	auxKey := walsink.AuxiliaryFilePath(dep, "00000002.history", walsink.AuxiliaryHistory)
	got, err := fetchAuxBody(context.Background(), sp, auxKey,
		walsink.AuxiliaryHistory, dep, "00000002.history")
	if err != nil {
		t.Fatalf("fetchAuxBody could not read back what the capture wrote: %v\n\n"+
			"The capture keys by DECIMAL timeline and PG asks by HEX archive name. If "+
			"these two disagree the file is written and never found, which looks exactly "+
			"like never writing it.", err)
	}
	if string(got) != string(want) {
		t.Errorf("served %q, want %q", got, want)
	}
}

// captureDispatcher builds a Dispatcher whose events land in a buffer.
func captureDispatcher(t *testing.T) (*output.Dispatcher, *strings.Builder) {
	t.Helper()
	buf := &strings.Builder{}
	d := output.NewDispatcher(rendererjson.New(), buf, buf)
	return d, buf
}

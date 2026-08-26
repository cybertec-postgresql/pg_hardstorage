package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// fakeHistory serves .history bodies from a map, so the timeline
// decision is testable without a repository or a live PostgreSQL.
type fakeHistory struct {
	bodies map[uint32]string
	err    error
	asked  []uint32
}

func (f *fakeHistory) Get(_ context.Context, _ string, tli uint32) ([]byte, error) {
	f.asked = append(f.asked, tli)
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.bodies[tli]
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(body), nil
}

func collectEvents() (func(*output.Event), *[]*output.Event) {
	var got []*output.Event
	return func(e *output.Event) { got = append(got, e) }, &got
}

const seg16 = 16 << 20

func mustLSN(t *testing.T, s string) pglogrepl.LSN {
	t.Helper()
	v, err := pglogrepl.ParseLSN(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The regression: the geometry the chaos gate produced. The stream
// must open on timeline 27, not on the 29 the server reports.
func TestResolveStreamTimeline_ResumeBelowForkOpensTheOldTimeline(t *testing.T) {
	h := &fakeHistory{bodies: map[uint32]string{
		29: "27\t0/A3079D40\tno recovery target specified\n" +
			"28\t0/A60B6668\tno recovery target specified\n",
	}}
	emit, events := collectEvents()

	got := resolveStreamTimeline(context.Background(), h, "db1", 29,
		mustLSN(t, "0/A1000000"), seg16, emit)

	if got != 27 {
		t.Fatalf("stream timeline = %d, want 27 — opening on %d asks for segment "+
			"0000001D00000000000000A1, a file that cannot exist, and PG refuses it "+
			"permanently as \"already been removed\"", got, got)
	}
	// The operator has to be able to tell this apart from the failure
	// it replaces, so the divergence must be announced.
	var found bool
	for _, e := range *events {
		if e.Op == "streaming_historic" {
			found = true
		}
	}
	if !found {
		t.Errorf("no wal.timeline.streaming_historic event; the stream silently changed timelines")
	}
}

func TestResolveStreamTimeline_CaughtUpStreamStaysOnTheLiveTimeline(t *testing.T) {
	h := &fakeHistory{bodies: map[uint32]string{
		29: "27\t0/A3079D40\tr\n28\t0/A60B6668\tr\n",
	}}
	emit, events := collectEvents()

	got := resolveStreamTimeline(context.Background(), h, "db1", 29,
		mustLSN(t, "0/A7000000"), seg16, emit)

	if got != 29 {
		t.Fatalf("stream timeline = %d, want 29", got)
	}
	if len(*events) != 0 {
		t.Errorf("the ordinary path must be silent; emitted %d events", len(*events))
	}
}

func TestResolveStreamTimeline_DegradedPathsKeepThePreExistingBehaviour(t *testing.T) {
	lsn := mustLSN(t, "0/A1000000")
	for name, tc := range map[string]struct {
		store     historyReader
		serverTLI uint32
		wantWarn  bool
	}{
		"no store":           {nil, 29, false},
		"timeline 1":         {&fakeHistory{}, 1, false},
		"history missing":    {&fakeHistory{bodies: map[uint32]string{}}, 29, true},
		"history unreadable": {&fakeHistory{err: errors.New("storage down")}, 29, true},
		"history corrupt": {&fakeHistory{bodies: map[uint32]string{
			29: "not a history file at all\n"}}, 29, true},
	} {
		t.Run(name, func(t *testing.T) {
			emit, events := collectEvents()
			got := resolveStreamTimeline(context.Background(), tc.store, "db1", tc.serverTLI, lsn, seg16, emit)
			if got != tc.serverTLI {
				t.Fatalf("got %d, want the server's timeline %d — guessing an ancestor "+
					"when we cannot read the history is worse than letting PG refuse",
					got, tc.serverTLI)
			}
			var warned bool
			for _, e := range *events {
				if e.Op == "history_unreadable" {
					warned = true
				}
			}
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v", warned, tc.wantWarn)
			}
		})
	}
}

// A nil emit must not panic: the degraded paths are reached from a
// stream that may have no dispatcher wired yet.
func TestResolveStreamTimeline_NilEmit(t *testing.T) {
	h := &fakeHistory{err: errors.New("boom")}
	if got := resolveStreamTimeline(context.Background(), h, "db1", 29, 0, seg16, nil); got != 29 {
		t.Fatalf("got %d, want 29", got)
	}
}

// The history is fetched for the SERVER's timeline — that one file
// carries the whole ancestry, so one read answers for every LSN.
func TestResolveStreamTimeline_ReadsOnlyTheCurrentTimelinesHistory(t *testing.T) {
	h := &fakeHistory{bodies: map[uint32]string{29: "27\t0/A3079D40\tr\n"}}
	resolveStreamTimeline(context.Background(), h, "db1", 29, mustLSN(t, "0/A1000000"), seg16, nil)
	if len(h.asked) != 1 || h.asked[0] != 29 {
		t.Fatalf("fetched %v, want exactly [29]", h.asked)
	}
}

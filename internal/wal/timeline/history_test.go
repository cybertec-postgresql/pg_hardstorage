package timeline

import (
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"
)

func lsn(t *testing.T, s string) pglogrepl.LSN {
	t.Helper()
	v, err := pglogrepl.ParseLSN(s)
	if err != nil {
		t.Fatalf("ParseLSN(%q): %v", s, err)
	}
	return v
}

// A real file, verbatim from a cluster that failed over twice.
const twoPromotions = "1\t0/15A2B388\tno recovery target specified\n" +
	"2\t0/2A000000\tno recovery target specified\n"

func TestParseHistory_RealFile(t *testing.T) {
	got, err := ParseHistory([]byte(twoPromotions))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Timeline != 1 || got[0].SwitchPoint != lsn(t, "0/15A2B388") {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].Timeline != 2 || got[1].SwitchPoint != lsn(t, "0/2A000000") {
		t.Errorf("entry 1 = %+v", got[1])
	}
	if got[0].Reason != "no recovery target specified" {
		t.Errorf("reason = %q", got[0].Reason)
	}
}

func TestParseHistory_SkipsBlanksAndComments(t *testing.T) {
	got, err := ParseHistory([]byte("\n# a comment\n   \n   # indented comment\n1\t0/3000000\tx\n"))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(got) != 1 || got[0].Timeline != 1 {
		t.Fatalf("got %+v, want the single real entry", got)
	}
}

func TestParseHistory_EmptyIsNotAnError(t *testing.T) {
	// Timeline 1 has no ancestors; PG returns an empty body. That is
	// a cluster that never failed over, not a corrupt file.
	got, err := ParseHistory(nil)
	if err != nil {
		t.Fatalf("empty history rejected: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want no entries", got)
	}
}

func TestParseHistory_Rejects(t *testing.T) {
	for name, in := range map[string]string{
		"one field":      "1\n",
		"bad tli":        "x\t0/3000000\tr\n",
		"zero tli":       "0\t0/3000000\tr\n",
		"bad lsn":        "1\tnotanlsn\tr\n",
		"decreasing tli": "2\t0/3000000\tr\n1\t0/4000000\tr\n",
		"repeated tli":   "1\t0/3000000\tr\n1\t0/4000000\tr\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHistory([]byte(in)); err == nil {
				t.Fatalf("accepted malformed history %q", in)
			}
		})
	}
}

// The regression proper. Reconstructed from the chaos gate's failure
// (soak 17, seed 3123180132316635255): the archive's frontier sat at
// 0/A1000000 on timeline 27, the cluster had promoted twice to
// timeline 29, and the streamer asked timeline 29 for it — producing a
// request for 0000001D00000000000000A1, a file that cannot exist, and
// a terminal "already been removed" refusal.
func TestContaining_ResumePointBelowTheForkPicksTheOldTimeline(t *testing.T) {
	hist, err := ParseHistory([]byte(
		"27\t0/A3079D40\tno recovery target specified\n" +
			"28\t0/A60B6668\tno recovery target specified\n"))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}

	got := Containing(hist, 29, lsn(t, "0/A1000000"))
	if got != 27 {
		t.Fatalf("Containing = %d, want 27 — asking timeline %d for an LSN below its fork "+
			"makes PG look for a segment that has never existed and refuse permanently", got, got)
	}
}

func TestContaining_WalksTheChainOnePromotionPerReconnect(t *testing.T) {
	hist, _ := ParseHistory([]byte("27\t0/A3079D40\tr\n28\t0/A60B6668\tr\n"))

	// This is the loop the retry path actually performs: archive up to
	// a switchpoint, resume exactly ON it, resolve again. It must
	// advance every time and terminate on the live timeline.
	for _, tc := range []struct {
		at   string
		want uint32
		why  string
	}{
		{"0/A1000000", 27, "below timeline 27's switchpoint"},
		{"0/A3079D40", 28, "exactly ON 27's switchpoint — that byte belongs to 28"},
		{"0/A4000000", 28, "inside timeline 28"},
		{"0/A60B6668", 29, "exactly ON 28's switchpoint — that byte belongs to 29"},
		{"0/A7000000", 29, "inside the live timeline"},
	} {
		if got := Containing(hist, 29, lsn(t, tc.at)); got != tc.want {
			t.Errorf("Containing(%s) = %d, want %d (%s)", tc.at, got, tc.want, tc.why)
		}
	}
}

func TestContaining_ZeroLengthTimelineIsSkipped(t *testing.T) {
	// A promotion that re-promoted before writing anything leaves two
	// entries sharing a switchpoint. Resuming there must land on the
	// live timeline, not on the empty one — asking for a timeline with
	// no WAL at all would reconnect forever without progressing.
	hist, _ := ParseHistory([]byte("27\t0/A3079D40\tr\n28\t0/A3079D40\tr\n"))
	if got := Containing(hist, 29, lsn(t, "0/A3079D40")); got != 29 {
		t.Fatalf("Containing = %d, want 29 — timeline 28 holds no WAL to serve", got)
	}
}

func TestContaining_NoHistoryKeepsTheCurrentTimeline(t *testing.T) {
	// A cluster that never failed over, and the degraded case where
	// the history file could not be fetched. Both must behave exactly
	// as the code did before this fix existed.
	if got := Containing(nil, 1, lsn(t, "0/3000000")); got != 1 {
		t.Errorf("Containing(nil, 1) = %d, want 1", got)
	}
	if got := Containing(nil, 29, lsn(t, "0/A1000000")); got != 29 {
		t.Errorf("Containing(nil, 29) = %d, want 29", got)
	}
}

func TestContaining_IgnoresEntriesAtOrAboveCurrent(t *testing.T) {
	// A malformed file must never steer the stream onto a timeline
	// that cannot hold the LSN — that is the failure being fixed.
	hist := []Switch{{Timeline: 29, SwitchPoint: lsn(t, "0/FF000000")}}
	if got := Containing(hist, 29, lsn(t, "0/A1000000")); got != 29 {
		t.Fatalf("Containing = %d, want 29", got)
	}
}

// The parser must not accept something that would make Containing
// silently wrong, so fuzz the two together: any history that parses
// must yield a timeline that is either current or one genuinely
// listed below it, and the answer must be monotonic in lsn.
func FuzzHistoryContaining(f *testing.F) {
	f.Add(twoPromotions, uint64(0x15A2B388))
	f.Add("27\t0/A3079D40\tr\n28\t0/A60B6668\tr\n", uint64(0xA1000000))
	f.Add("", uint64(0))

	f.Fuzz(func(t *testing.T, body string, raw uint64) {
		hist, err := ParseHistory([]byte(body))
		if err != nil {
			return
		}
		const current = 29
		got := Containing(hist, current, pglogrepl.LSN(raw))
		if got == current {
			return
		}
		found := false
		for _, e := range hist {
			if e.Timeline == got {
				found = true
				if pglogrepl.LSN(raw) >= e.SwitchPoint {
					t.Fatalf("chose timeline %d for %X, but it ended at %X",
						got, raw, uint64(e.SwitchPoint))
				}
			}
		}
		if !found {
			t.Fatalf("chose timeline %d, which the history does not list:\n%q", got, body)
		}
		// Monotonicity: a later LSN can never resolve to an EARLIER
		// timeline, or the resume loop could walk backwards.
		if raw < ^uint64(0) {
			if later := Containing(hist, current, pglogrepl.LSN(raw+1)); later < got {
				t.Fatalf("Containing went backwards: %X->%d but %X->%d", raw, got, raw+1, later)
			}
		}
		_ = strings.TrimSpace(body)
	})
}

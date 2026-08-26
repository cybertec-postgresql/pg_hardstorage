// history.go — parsing .history files into the switchpoints a resuming
// streamer needs to pick the RIGHT timeline to ask PostgreSQL for.
//
// Why this file exists
// --------------------
// A resuming `wal stream` computes where to pick up and then opens
// START_REPLICATION. Those are two separate decisions, and for a long
// time only the first one was made carefully: the resume LSN walked
// back to the previous timeline's frontier when the current timeline
// had nothing archived yet, but the stream was always opened on the
// timeline IDENTIFY_SYSTEM reported — the newest one.
//
// An LSN and a timeline are not independent. A WAL segment file is
// named for BOTH, so asking timeline 29's walsender for an LSN that
// belongs to timeline 27's era makes it look for a file such as
//
//	0000001D00000000000000A1
//
// that has never existed and never can: timeline 29 begins at its fork
// point, and has no segments below it. PostgreSQL cannot distinguish
// "recycled" from "never existed" — both are just a missing file — so
// it answers
//
//	requested WAL segment ... has already been removed
//
// which our classifier correctly treats as terminal, and the streamer
// stops for good. The message sends the operator hunting for recycled
// WAL while the bytes are almost certainly still on disk under
// 0000001B..., perfectly servable.
//
// The bug needs the streamer to fall more than ONE segment behind
// across a promotion, which is why it hid for so long: PostgreSQL
// copies the last partial segment of the old timeline to the new
// timeline's name when it promotes, so a caught-up streamer finds its
// resume segment under the new name by luck. A streamer that was down
// for a while — the exact case the resume walk-back exists to serve —
// asks for a segment well below the fork and dies. Found by the chaos
// gate after a demotion storm left the streamer reconnecting through
// two promotions (soak 17, seed 3123180132316635255).
//
// The fix is to ask the timeline that actually CONTAINS the LSN, and
// let PostgreSQL stream forward and hand us off at the switchpoint —
// which is what the history file is for. Nothing in the tree parsed
// one before: `wal stream` captured these files faithfully and only
// ever stored them for a future restore to read.
package timeline

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pglogrepl"
)

// Switch is one line of a .history file: the timeline that ENDED, and
// the LSN it ended at. The timeline that follows begins at exactly
// that LSN, so a switchpoint is a half-open boundary — SwitchPoint
// itself belongs to the NEXT timeline, not to Timeline.
type Switch struct {
	// Timeline is the timeline this entry describes — the one that
	// ended at SwitchPoint.
	Timeline uint32
	// SwitchPoint is the LSN at which Timeline ended.
	SwitchPoint pglogrepl.LSN
	// Reason is the verbatim trailing text PostgreSQL wrote ("no
	// recovery target specified", "before %s", ...). Carried for
	// diagnostics; nothing keys off it.
	Reason string
}

// ParseHistory parses the bytes of a PostgreSQL .history file into its
// switchpoints, oldest timeline first.
//
// The accepted grammar mirrors PostgreSQL's own readTimeLineHistory:
// blank lines and lines whose first non-blank character is '#' are
// skipped, fields are whitespace-separated, everything after the
// second field is free-text, and timeline IDs must strictly increase.
// That last rule is PostgreSQL's own corruption check, and it is worth
// keeping here for the same reason it has it: entries out of order
// make every lookup below silently wrong rather than loudly broken.
//
// The history file for timeline N describes N's ancestors only —
// timeline N itself has no entry, because it has not ended.
func ParseHistory(content []byte) ([]Switch, error) {
	var out []Switch
	sc := bufio.NewScanner(bytes.NewReader(content))
	// History files are tiny (one line per promotion, ever), but a
	// corrupt/truncated object should fail with a real error rather
	// than a scanner default that silently drops the tail.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) < 2 {
			return nil, fmt.Errorf("timeline: history line %d: expected \"<tli> <lsn> [reason]\", got %q", line, s)
		}
		tli, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil || tli == 0 {
			return nil, fmt.Errorf("timeline: history line %d: bad timeline id %q", line, fields[0])
		}
		lsn, err := pglogrepl.ParseLSN(fields[1])
		if err != nil {
			return nil, fmt.Errorf("timeline: history line %d: bad switchpoint %q: %w", line, fields[1], err)
		}
		e := Switch{Timeline: uint32(tli), SwitchPoint: lsn}
		if len(fields) > 2 {
			e.Reason = strings.Join(fields[2:], " ")
		}
		if n := len(out); n > 0 && e.Timeline <= out[n-1].Timeline {
			return nil, fmt.Errorf("timeline: history line %d: timeline ids must increase (found %d after %d) — history file is corrupt",
				line, e.Timeline, out[n-1].Timeline)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("timeline: read history: %w", err)
	}
	return out, nil
}

// Containing returns the timeline whose WAL contains lsn, given the
// history of timeline current (as returned by ParseHistory) and
// current itself as the fallback.
//
// A switchpoint is the EXCLUSIVE end of its timeline: an LSN equal to
// a switchpoint is the first byte of the timeline that follows. That
// boundary is what makes a resuming streamer advance instead of
// looping — having archived timeline 27 right up to its switchpoint,
// the next resume lands exactly ON it and resolves to 28, then 29, one
// reconnect per promotion, until it reaches the live timeline.
//
// An empty history (timeline 1, or a file we could not fetch) yields
// current, which is the pre-existing behaviour: on a cluster that has
// never failed over there is no other answer, and on one we cannot
// read the history for, guessing an ancestor would be worse than
// letting PostgreSQL refuse.
func Containing(history []Switch, current uint32, lsn pglogrepl.LSN) uint32 {
	for _, e := range history {
		// Defensive: a history file for timeline N should list only
		// ancestors. An entry at or above N is nonsense; ignoring it
		// keeps a malformed file from steering the stream ONTO a
		// timeline that does not hold the LSN, which is the very
		// failure this function exists to prevent.
		if e.Timeline >= current {
			continue
		}
		if lsn < e.SwitchPoint {
			return e.Timeline
		}
	}
	return current
}

// timeline.go — TIMELINE_HISTORY wrapper returning parsed history files with byte-identical content.
package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TimelineHistory is the parsed result of TIMELINE_HISTORY <tli>
// on a replication-mode connection. It carries:
//
//   - Timeline   — the timeline ID we asked for (echoed back so the
//     caller has a self-describing payload).
//   - Filename   — PG's own filename, e.g. "00000002.history". The
//     byte-identical filename is what archive-style
//     consumers expect, and what we use as the storage
//     key suffix.
//   - Content    — verbatim file bytes. Restore replays these so PG
//     understands the timeline lineage; we never modify
//     them.
type TimelineHistory struct {
	Timeline uint32
	Filename string
	Content  []byte
}

// TimelineHistoryFor issues TIMELINE_HISTORY <tli> on the given
// replication-mode connection and returns the .history file PG
// stores at PGDATA/pg_wal/<TLI>.history.
//
// PG returns two columns:
//
//	filename | content
//	---------+-----------------------------------------
//	00000002.history | "1\t0/15A2B388\tno recovery target..."
//
// TIMELINE_HISTORY 1 is a special case: there's no parent for the
// initial timeline, so PG returns an empty result set. We surface
// that as ErrNoHistoryForTLI1 rather than letting the caller
// interpret an empty content as a corruption signal.
//
// This helper is the building block for the leader-follow loop's
// "capture the new TLI's history file on every promotion" step
// (Plan §Patroni Mechanism 1, item 5).
func TimelineHistoryFor(ctx context.Context, c *Conn, tli uint32) (*TimelineHistory, error) {
	if c == nil || c.pg == nil {
		return nil, errors.New("pg: nil connection")
	}
	if c.mode != ModeReplication {
		return nil, output.NewError("usage.wrong_mode",
			"TIMELINE_HISTORY requires ModeReplication; got "+c.mode.String()).
			Wrap(output.ErrUsage)
	}
	if tli == 0 {
		return nil, output.NewError("usage.bad_arg",
			"pg: TIMELINE_HISTORY requires a non-zero timeline").
			Wrap(output.ErrUsage)
	}
	// The wire form is `TIMELINE_HISTORY <tli>` with the timeline
	// ID injected as a literal int — pglogrepl/pgconn don't bind
	// parameters on replication mode. strconv keeps it explicit
	// (no fmt-format-string ambiguity for negative-looking numbers).
	q := "TIMELINE_HISTORY " + strconv.FormatUint(uint64(tli), 10)
	results, err := c.pg.Exec(ctx, q).ReadAll()
	if err != nil {
		// TLI 1 has no parent, so PG has no .history file for it.
		// Older PG returned an empty result set; PG 14+ surfaces it
		// as "could not open file pg_wal/00000001.history: No such
		// file or directory" (SQLSTATE 58P01).  Treat both shapes as
		// the same sentinel — callers (notably the leader-follow
		// loop's per-reconnect history capture) should be able to
		// errors.Is against ErrNoHistoryForTLI1 regardless of PG
		// version.
		if tli == 1 && isNoHistoryFileError(err) {
			return nil, ErrNoHistoryForTLI1
		}
		return nil, fmt.Errorf("pg: %s: %w", q, err)
	}
	// Empty result set = the historical (pre-PG-14) "no history file"
	// shape for the initial timeline.  Same sentinel as the
	// "No such file" branch above; keeping both paths so an operator
	// running against a mixed-version fleet doesn't have to care
	// which PG variant they're on.
	if len(results) == 0 || len(results[0].Rows) == 0 {
		return nil, ErrNoHistoryForTLI1
	}
	row := results[0].Rows[0]
	if len(row) < 2 {
		return nil, fmt.Errorf("pg: TIMELINE_HISTORY %d returned %d columns (want 2)", tli, len(row))
	}
	return &TimelineHistory{
		Timeline: tli,
		Filename: string(row[0]),
		Content:  append([]byte(nil), row[1]...),
	}, nil
}

// ErrNoHistoryForTLI1 is returned by TimelineHistoryFor for TLI 1
// (the initial timeline; PG has nothing to return). Callers that
// want to opportunistically capture histories without bailing on
// fresh clusters should errors.Is against this sentinel.
var ErrNoHistoryForTLI1 = errors.New("pg: TIMELINE_HISTORY for timeline 1 has no parent (no .history file)")

// isNoHistoryFileError reports whether err is the "could not open
// file pg_wal/00000001.history: No such file or directory" error
// PG returns for TIMELINE_HISTORY 1.  Substring-match because
// pgconn doesn't surface the SQLSTATE here on every wire version,
// and the message is stable across PG 14..18.
func isNoHistoryFileError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "00000001.history") &&
		strings.Contains(s, "No such file or directory")
}

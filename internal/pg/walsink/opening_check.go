//go:build !mutation_stream_first_record_unchecked

package walsink

import (
	"fmt"

	"github.com/jackc/pglogrepl"
)

// checkOpeningRecord validates the FIRST record of a (re)connected
// stream against the position the caller asked PG to resume from.
// Split into its own file so the mutation harness can swap in the
// pre-fix variant (see the sibling mutation file): without this check,
// every reconnect's opening record was accepted at whatever LSN it
// carried, and a stream that resumed past a hole looked contiguous
// forever after. Forward-only on purpose — a walsender may open at or
// below the requested position, and refusing that would crash-loop a
// healthy stream.
func (s *Sink) checkOpeningRecord(pos uint64, walStart pglogrepl.LSN) error {
	if s.firstChecked {
		return nil
	}
	s.firstChecked = true
	if s.expectedFirst != 0 && pos > s.expectedFirst {
		return fmt.Errorf("walsink: gap detected at stream start: asked PG to resume at %s "+
			"but the first record begins at %s, so %d byte(s) were skipped and are not "+
			"coming; refusing to commit a segment past an unrecorded hole",
			pglogrepl.LSN(s.expectedFirst), walStart, pos-s.expectedFirst)
	}
	return nil
}

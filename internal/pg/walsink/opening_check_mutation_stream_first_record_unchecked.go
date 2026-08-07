//go:build mutation_stream_first_record_unchecked

package walsink

import "github.com/jackc/pglogrepl"

// checkOpeningRecord — MUTATED variant: the exact pre-20afaf5 world.
// The opening record of every reconnect is accepted at whatever LSN it
// carries; only record-to-record contiguity is checked afterwards, so
// a stream that resumed past a hole looks contiguous for the rest of
// its life and PG recycles the missing WAL. Caught by
// TestSink_OpeningRecordPastTheResumePoint_Refused.
func (s *Sink) checkOpeningRecord(pos uint64, walStart pglogrepl.LSN) error {
	_ = pos
	_ = walStart
	s.firstChecked = true
	return nil
}

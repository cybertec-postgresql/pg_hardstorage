// search_stream.go — Search's global ordering without Search's slice.
package audit

import (
	"context"
	"encoding/binary"
	"encoding/json"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// searchSortMemBytes is the in-memory buffer of the ordered-search sorter
// (0 = the sorter's default); a variable so tests can force the spill path.
var searchSortMemBytes int

// SearchEach streams the events Search returns, in the same order and
// with the same Limit/Reverse meaning, to fn one at a time.
//
// Search holds every match as a slice because the global order (Timestamp,
// then ID) only exists after all shards have been read. SearchEach keeps
// the matches in an external sorter instead, so its heap is the sorter's
// buffer however many events match; a large result costs temp-file space
// under os.TempDir() instead. Search stays the convenient form for
// bounded queries.
//
// The *Event passed to fn is decoded afresh for each call and may be kept.
func (s *Store) SearchEach(ctx context.Context, f ListFilters, fn func(*Event) error) error {
	m, err := s.sortMatches(ctx, f, "")
	if err != nil {
		return err
	}
	defer m.Close()
	return m.each(ctx, func(line []byte) error {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
		return fn(&ev)
	})
}

// sortedMatches is the result of one search, held in an external sorter:
// each record is an event's JSON encoding under a key that sorts the way
// Search orders its slice.
type sortedMatches struct {
	sorter *spill.Sorter
	limit  int
}

// sortMatches scans the chain for events matching f and returns them
// ordered on disk. spoolDir is the parent of the sorter's run files
// ("" = os.TempDir()). The caller must Close the result.
func (s *Store) sortMatches(ctx context.Context, f ListFilters, spoolDir string) (*sortedMatches, error) {
	sorter := spill.NewSorter(spill.SorterOptions{MemBytes: searchSortMemBytes, Dir: spoolDir})
	var key []byte
	if err := s.scan(ctx, f, func(ev *Event) error {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		key = appendEventOrderKey(key[:0], ev, f.Reverse)
		return sorter.Add(key, line)
	}); err != nil {
		_ = sorter.Close()
		return nil, err
	}
	return &sortedMatches{sorter: sorter, limit: f.Limit}, nil
}

// each calls fn with every match's JSON encoding (json.Marshal of the
// event, no trailing newline) in Search order, stopping at the filter's
// Limit. line is valid only until fn returns.
func (m *sortedMatches) each(ctx context.Context, fn func(line []byte) error) error {
	it, err := m.sorter.Iter()
	if err != nil {
		return err
	}
	defer it.Close()
	for n := 0; m.limit <= 0 || n < m.limit; n++ {
		// The merge is local work: without this a cancelled export would
		// run a multi-gigabyte window to the end before noticing.
		if err := ctx.Err(); err != nil {
			return err
		}
		_, line, ok, err := it.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return nil
}

// Close removes the sorter's temp files. Safe to call more than once.
func (m *sortedMatches) Close() error { return m.sorter.Close() }

// appendEventOrderKey appends a key whose byte order is Search's event
// order: Timestamp ascending, ID breaking ties (sortAuditEvents). With
// reverse the key is complemented, so ascending byte order is that order
// backwards — what Search produces for Reverse.
//
// The instant is seconds (sign bit flipped so pre-1970 sorts first) then
// nanoseconds, which compares exactly as time.Time.Before does and, unlike
// UnixNano, does not overflow outside 1678–2262. The ID is written with
// 0x00 escaped as 0x00 0xFF and terminated by 0x00 0x00: that keeps the
// byte order of the raw IDs while making no key a prefix of another,
// without which complementing would not reverse the order ("a" sorts
// before "ab" both plain and complemented).
func appendEventOrderKey(b []byte, ev *Event, reverse bool) []byte {
	start := len(b)
	b = binary.BigEndian.AppendUint64(b, uint64(ev.Timestamp.Unix())^(1<<63))
	b = binary.BigEndian.AppendUint32(b, uint32(ev.Timestamp.Nanosecond()))
	for i := 0; i < len(ev.ID); i++ {
		c := ev.ID[i]
		b = append(b, c)
		if c == 0 {
			b = append(b, 0xFF)
		}
	}
	b = append(b, 0, 0)
	if reverse {
		for i := start; i < len(b); i++ {
			b[i] = ^b[i]
		}
	}
	return b
}

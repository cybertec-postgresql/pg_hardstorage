// walsegsize.go — probe the cluster's configured WAL segment size.
package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// probeWALSegSizeQuery resolves wal_segment_size to an integer byte count
// server-side via pg_size_bytes (PG 9.6+), so we never have to parse the
// "16MB"/"1GB" unit string ourselves. The default is 16 MiB; a cluster
// initialised with `initdb --wal-segsize` (PG 11+) can be 1 MiB–1 GiB.
const probeWALSegSizeQuery = "SELECT pg_size_bytes(current_setting('wal_segment_size'))"

// QueryWALSegmentSize returns the cluster's configured WAL segment size in
// bytes. The connection MUST be ModeRegular — a SELECT cannot run on a
// replication-mode connection. Mirrors QueryVersion's shape.
func QueryWALSegmentSize(ctx context.Context, c *Conn) (int64, error) {
	if c == nil || c.pg == nil {
		return 0, errors.New("pg: nil connection")
	}
	if c.mode != ModeRegular {
		return 0, output.NewError("usage.wrong_mode",
			"wal_segment_size probe requires ModeRegular; got "+c.mode.String()).Wrap(output.ErrUsage)
	}
	res := c.pg.ExecParams(ctx, probeWALSegSizeQuery, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return 0, fmt.Errorf("pg: probe wal_segment_size: %w", res.Err)
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return 0, errors.New("pg: empty wal_segment_size result")
	}
	raw := strings.TrimSpace(string(res.Rows[0][0]))
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pg: parse wal_segment_size %q: %w", raw, err)
	}
	return n, nil
}

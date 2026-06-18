// identify.go — IDENTIFY_SYSTEM wrapper returning SystemIdentity (system_id/timeline/xlogpos).
package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// SystemIdentity is the result of IDENTIFY_SYSTEM. It carries the
// cluster identity and the current WAL position at the moment of the
// query — useful as a sanity check in backup orchestration ("the LSN we
// observed before BASE_BACKUP started should be <= start_lsn").
type SystemIdentity struct {
	// SystemID is the 64-bit pg_control system identifier as an
	// unsigned-integer string. Stable across the cluster lifetime;
	// compares equal between primary and replicas of the same cluster.
	SystemID string
	// Timeline is the current WAL timeline.
	Timeline uint32
	// XLogPos is the current WAL flush location ("0/3000028" form).
	XLogPos string
	// DBName is the default database name set in the connection string,
	// or empty when the conn was opened without one. Echoed by the server.
	DBName string
}

// IdentifySystem issues IDENTIFY_SYSTEM on a replication-mode
// connection and parses the four-column result. The connection MUST
// be in ModeReplication; we surface a typed usage error otherwise.
//
// Wire format (PG 15+): IDENTIFY_SYSTEM returns one row with columns
// systemid (text), timeline (int4), xlogpos (text), dbname (text).
func IdentifySystem(ctx context.Context, c *Conn) (SystemIdentity, error) {
	if c == nil || c.pg == nil {
		return SystemIdentity{}, errors.New("pg: nil connection")
	}
	if c.mode != ModeReplication {
		return SystemIdentity{}, output.NewError("usage.wrong_mode",
			"IDENTIFY_SYSTEM requires ModeReplication; got "+c.mode.String()).Wrap(output.ErrUsage)
	}
	results, err := c.pg.Exec(ctx, "IDENTIFY_SYSTEM").ReadAll()
	if err != nil {
		return SystemIdentity{}, fmt.Errorf("pg: IDENTIFY_SYSTEM: %w", err)
	}
	if len(results) == 0 || len(results[0].Rows) == 0 {
		return SystemIdentity{}, errors.New("pg: IDENTIFY_SYSTEM returned no rows")
	}
	row := results[0].Rows[0]
	if len(row) < 3 {
		return SystemIdentity{}, fmt.Errorf("pg: IDENTIFY_SYSTEM returned %d columns", len(row))
	}
	tli, err := strconv.ParseUint(string(row[1]), 10, 32)
	if err != nil {
		return SystemIdentity{}, fmt.Errorf("pg: parse timeline %q: %w", row[1], err)
	}
	id := SystemIdentity{
		SystemID: string(row[0]),
		Timeline: uint32(tli),
		XLogPos:  string(row[2]),
	}
	if len(row) >= 4 {
		id.DBName = string(row[3])
	}
	return id, nil
}

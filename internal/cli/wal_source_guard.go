//go:build !mutation_standby_source_unguarded

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// sourceInRecovery reports whether the server at dsn is a standby.
//
// Nothing else in the product asks this question, which is how a
// streamer came to keep archiving from a node Patroni had demoted.
func sourceInRecovery(ctx context.Context, pgConn string) (bool, error) {
	conn, err := pg.Connect(ctx, pgConn, pg.ModeRegular)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.PgConn().Exec(ctx, "SELECT pg_is_in_recovery()::text").ReadAll()
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		for _, row := range r.Rows {
			if len(row) > 0 && row[0] != nil {
				return strings.EqualFold(string(row[0]), "true") || string(row[0]) == "t", nil
			}
		}
	}
	return false, fmt.Errorf("pg_is_in_recovery() returned no rows")
}

// guardSourceIsPrimary refuses to stream from a server in recovery.
//
// `wal stream` has no Patroni awareness; leader-following is delegated
// entirely to libpq, and the retry loop's comment
// ("target_session_attrs=primary routes to the new primary") holds only
// when the operator wrote a multi-host DSN carrying that parameter. A
// single-host DSN — the shape most people write first — has nothing to
// route, so after a failover the streamer reconnects to the node it
// always used, which is now a replica.
//
// PostgreSQL permits physical replication from a standby, so that does
// not fail. Measured against a real 3-node cluster: 90 seconds after
// its node was demoted the streamer had reconnected, resumed streaming
// and reported nothing unusual. It is archiving second-hand WAL from a
// replica while the operator believes they are archiving from the
// primary, and every health signal agrees with them. If that replica
// falls behind, is reinitialised, or leaves the cluster, WAL the
// primary has already recycled never reaches the archive and the gap
// is permanent.
//
// The refusal is RETRYABLE on purpose, not fatal. During a failover
// every node is briefly in recovery, and a leader-aware DSN reconnects
// onto the new primary within a few attempts — for those operators
// nothing changes but a transient warning. It is the pinned-DSN case
// that now stays visibly stuck instead of quietly succeeding against
// the wrong server.
//
// Archiving deliberately from a standby is a legitimate choice for
// offloading the primary, so --allow-standby-source keeps it available.
// It is off by default because the cost of the two mistakes is not
// symmetric: an operator who wants a standby gets one flag, while an
// operator who does not gets silence and a gap.
func guardSourceIsPrimary(ctx context.Context, d *output.Dispatcher, opts walStreamOptions) error {
	if opts.pgConn == "" || opts.allowStandbySource {
		return nil
	}
	inRecovery, err := sourceInRecovery(ctx, opts.pgConn)
	if err != nil {
		// Fail OPEN, but never silently. The connection has just served
		// IDENTIFY_SYSTEM, so a failure here is more likely transient
		// than meaningful — and blocking WAL archiving on an
		// inconclusive probe is itself a way to lose WAL.
		_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "wal.stream", "recovery_check_failed").
			WithSubject(output.Subject{Deployment: opts.deployment}).
			WithBody(map[string]any{
				"error": err.Error(),
				"message": "could not determine whether this source is a primary or a standby; " +
					"proceeding. If it is a standby, WAL reaches the archive second-hand and can " +
					"fall permanently behind the primary.",
			}))
		return nil
	}
	if !inRecovery {
		return nil
	}
	return output.NewError("wal.source_in_recovery",
		fmt.Sprintf("wal stream: the server at this --pg-connection is in recovery (a standby), "+
			"not a primary. Deployment %q would be archiving WAL second-hand from a replica: "+
			"PostgreSQL allows it, so nothing fails, but the archive silently trails the real "+
			"primary and any WAL the primary recycles before this replica forwards it is lost "+
			"for good. Refusing.", opts.deployment)).
		WithSuggestion(&output.Suggestion{
			Human: "this usually means a failover moved the primary while --pg-connection names a single host. Give the streamer a leader-aware DSN — list every node and add target_session_attrs=primary — so a reconnect lands on the new leader. To archive from a standby deliberately (offloading the primary), pass --allow-standby-source.",
		})
}

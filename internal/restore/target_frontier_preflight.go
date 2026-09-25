package restore

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// preflightTargetBeyondArchive refuses an LSN recovery target that lies
// past the end of the archived WAL.
//
// CheckTargetReachable refuses a target BEFORE the backup's stop; the
// other side was never checked. PostgreSQL replays to the end of the
// archive, finds no record at the target, and refuses to start ("recovery
// ended before configured recovery target was reached") — after the whole
// restore has run. `timetravel create --at FFFFFFFF/FFFFFFFF` built a
// session the cluster could never open.
//
// The archive's end is exclusive (HighestArchivedLSN returns the first
// byte of the segment that is not archived yet), so a target at or past
// it is unreachable NOW; WAL still being written becomes reachable once
// its segment is archived, which the refusal says. A later timeline can
// carry WAL past this timeline's end and recovery may follow it
// (recovery_target_timeline=latest), so when one exists this check stays
// out of the way rather than refuse a reachable target. Time and name
// targets cannot be resolved to an LSN without PostgreSQL.
// --skip-gap-check overrides.
func preflightTargetBeyondArchive(ctx context.Context, sp storage.StoragePlugin, deployment string, m *backup.Manifest, recovery *Recovery, skip bool) error {
	if recovery == nil || !recovery.Enable || recovery.TargetLSN == "" || m == nil || skip || recovery.SkipGapCheck {
		return nil
	}
	target, err := pglogrepl.ParseLSN(recovery.TargetLSN)
	if err != nil {
		return nil // malformed targets are refused elsewhere
	}
	stop, err := pglogrepl.ParseLSN(m.StopLSN)
	if err != nil || target <= stop {
		return nil
	}
	if _, later, lerr := inventory.HighestArchivedLSN(ctx, sp, deployment, m.Timeline+1); lerr != nil || later {
		return nil // a later timeline may reach further; a probe error must not refuse
	}
	frontier, found, ferr := inventory.HighestArchivedLSN(ctx, sp, deployment, m.Timeline)
	if ferr != nil {
		return nil
	}
	if found && target < frontier {
		return nil
	}
	end := "no WAL is archived on this timeline"
	if found {
		end = "the archive ends at " + frontier.String()
	}
	return output.NewError("restore.target_unreachable",
		fmt.Sprintf("restore: recovery target %s is beyond the archived WAL of %s on timeline %d (%s): "+
			"PostgreSQL would replay to the end of the archive and refuse to start",
			target, deployment, m.Timeline, end)).
		WithSuggestion(&output.Suggestion{
			Human: "pick an earlier target, or wait until WAL up to the target has been archived (a segment is archived once it is complete); --skip-gap-check overrides",
		})
}

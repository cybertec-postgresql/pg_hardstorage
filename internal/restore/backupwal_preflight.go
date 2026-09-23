package restore

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// preflightBackupWALAvailable refuses a restore whose backup can never
// reach consistency because the WAL between its start and stop LSN
// exists nowhere.
//
// A base backup is consistent only after replaying the WAL written while
// it ran. That WAL is either embedded in the backup (--include-wal puts
// pg_wal/ segments into it) or fetched from the archive. When neither
// holds any, PostgreSQL's recovery asks restore_command for the next
// segment, is told "not found" — which it reads as "not archived yet" —
// and waits forever. The restore itself "succeeds"; the cluster never
// starts. An operator found this by following the first-backup
// tutorial, which takes a backup without running `wal stream`; its
// doctest block carried a skip note saying recovery hangs.
//
// Deliberately narrow, so it cannot block a restore that would work:
// it refuses only when the manifest lists NO embedded WAL segment AND
// the archive holds NO segment at all on the backup's timeline. If
// either has anything, archiving is demonstrably happening and the
// ordinary gap checks judge the details. A repository that cannot be
// listed is not a reason to refuse. --skip-gap-check bypasses it, as it
// bypasses the other WAL pre-flights.
func preflightBackupWALAvailable(ctx context.Context, sp storage.StoragePlugin, deployment string, m *backup.Manifest, recovery *Recovery) error {
	if m == nil || (recovery != nil && recovery.SkipGapCheck) {
		return nil
	}
	if embedsWAL(m) {
		return nil
	}
	_, found, err := inventory.HighestArchivedLSN(ctx, sp, deployment, m.Timeline)
	if err != nil || found {
		return nil
	}
	return output.NewError("preflight.backup_wal_missing",
		fmt.Sprintf("restore: backup %s cannot become consistent: it does not embed its WAL, and no WAL "+
			"has been archived for %s on timeline %d — recovery would wait forever for WAL through %s",
			m.BackupID, deployment, m.Timeline, m.StopLSN)).
		WithSuggestion(&output.Suggestion{
			Human: "this backup was taken without --include-wal while nothing was archiving WAL for the deployment. " +
				"If the source still exists, take a new backup with --include-wal, or start `pg_hardstorage wal stream` " +
				"and take a new one. If the WAL exists elsewhere, archive it into the repository with `wal push` first. " +
				"--skip-gap-check overrides this refusal.",
		})
}

// embedsWAL reports whether the backup carries at least one WAL
// segment of its own under pg_wal/.
func embedsWAL(m *backup.Manifest) bool {
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "pg_wal/") && len(path.Base(f.Path)) == 24 {
			return true
		}
	}
	return false
}

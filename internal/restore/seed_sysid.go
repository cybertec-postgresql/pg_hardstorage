package restore

// seed_sysid.go — one shared resolver for the restore_command
// identity check's input, used by every site that generates a
// restore_command but does not already hold the seed manifest
// (standby bootstrap, time-travel, the agent's restore executor, the
// CLI). Best-effort BY DESIGN: the check is belt-and-braces —
// PostgreSQL validates xlp_sysid itself, just cryptically and
// mid-replay — so a transient manifest-read failure returns "" (check
// unarmed) and must never block a restore that would otherwise work.

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// SeedSystemIdentifier reads the backup's recorded system_identifier,
// or "" when it cannot.
func SeedSystemIdentifier(ctx context.Context, repoURL, deployment, backupID string, verifier *backup.Verifier) string {
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		return ""
	}
	defer sp.Close()
	m, _, err := backup.NewManifestStore(sp).ReadIncludingTombstoned(ctx, deployment, backupID, verifier)
	if err != nil || m == nil {
		return ""
	}
	return m.SystemIdentifier
}

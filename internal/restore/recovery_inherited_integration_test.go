//go:build integration

package restore_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// localInitdb creates a throwaway cluster with the host's initdb and
// returns its data dir. Skips when no local PostgreSQL is installed.
func localInitdb(t *testing.T) string {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		t.Skip("initdb not on PATH (export PATH=/usr/lib/postgresql/<v>/bin:$PATH)")
	}
	dir := filepath.Join(t.TempDir(), "pgdata")
	if out, err := exec.Command(initdb, "-D", dir, "-A", "trust", "--no-sync").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	return dir
}

// pgShow asks the real server binary what a GUC resolves to after it
// loads postgresql.conf + postgresql.auto.conf. `postgres -C` runs the
// same GUC assign hooks as startup, so "multiple recovery targets
// specified" surfaces here as a FATAL and non-zero exit.
func pgShow(t *testing.T, dataDir, guc string) (string, error) {
	t.Helper()
	pg, err := exec.LookPath("postgres")
	if err != nil {
		t.Skip("postgres not on PATH")
	}
	out, err := exec.Command(pg, "-D", dataDir, "-C", guc).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Regression (H45) against a real PostgreSQL: a backup of a cluster
// that an earlier pg_hardstorage restore produced carries that
// restore's recovery settings. The new restore's settings must be the
// ones PostgreSQL actually applies.
func TestIntegration_RecoverySettings_InheritedTargetsNeutralised(t *testing.T) {
	cases := []struct {
		name  string
		write func(dir string) error
		guc   string
		want  string
	}{
		{"to-time", func(dir string) error {
			return restore.WriteRecoveryFiles(dir, restore.Recovery{Enable: true, RestoreCommand: "true",
				TargetTime: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), Inclusive: true})
		}, "recovery_target_time", "2026-05-01 12:00:00+00"},
		{"to-latest", func(dir string) error {
			return restore.WriteRecoveryFiles(dir, restore.Recovery{Enable: true, RestoreCommand: "true", Action: "promote"})
		}, "recovery_target", ""},
		{"standby", func(dir string) error {
			return restore.WriteRecoveryFiles(dir, restore.Recovery{Enable: true, RestoreCommand: "true", StandbyMode: true})
		}, "recovery_target", ""},
		{"plain", func(dir string) error {
			return restore.WriteAutoRecovery(dir, "", "", "")
		}, "recovery_target_time", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := localInitdb(t)
			af := filepath.Join(dir, "postgresql.auto.conf")
			orig, err := os.ReadFile(af)
			if err != nil {
				t.Fatal(err)
			}
			// What an ALTER SYSTEM rewrite of a previously restored
			// cluster leaves behind: bare settings, no markers.
			inherited := string(orig) + "recovery_target = 'immediate'\n" +
				"recovery_target_time = '2025-01-01 00:00:00+00'\n"
			if err := os.WriteFile(af, []byte(inherited), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(dir); err != nil {
				t.Fatal(err)
			}
			got, err := pgShow(t, dir, tc.guc)
			if err != nil {
				body, _ := os.ReadFile(af)
				t.Fatalf("postgres -C %s: %v\n%s\nauto.conf:\n%s", tc.guc, err, got, body)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.guc, got, tc.want)
			}
		})
	}
}

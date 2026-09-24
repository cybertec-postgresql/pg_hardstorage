package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// TestBuildBackupTask_PathsFailureFailsTask pins that a scheduled
// backup never falls back to plaintext because the keyring location
// could not be resolved at fire time. Encryption was resolved only
// inside `if paths.Resolve(...) == nil`, so a resolution failure left
// enc nil and runner.Take wrote an unencrypted backup into what may be
// an encrypted repo. The task must fail instead.
func TestBuildBackupTask_PathsFailureFailsTask(t *testing.T) {
	prev := resolveAgentPaths
	resolveAgentPaths = func() (*paths.Paths, error) { return nil, errors.New("synthetic paths failure") }
	t.Cleanup(func() { resolveAgentPaths = prev })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	task, err := buildBackupTask("db1", config.DeploymentConfig{
		PGConnection: "postgres://u@127.0.0.1:1/db",
		Repo:         "file://" + t.TempDir(),
		Schedule:     config.DeploymentSchedule{Backup: config.ScheduleSpec{Every: "6h"}},
	}, config.KMSConfig{}, signer, verifier)
	if err != nil {
		t.Fatal(err)
	}
	runErr := task.Run(context.Background())
	if runErr == nil || !strings.Contains(runErr.Error(), "synthetic paths failure") {
		t.Fatalf("task error = %v; want the paths failure (no plaintext fallback to runner.Take)", runErr)
	}
}

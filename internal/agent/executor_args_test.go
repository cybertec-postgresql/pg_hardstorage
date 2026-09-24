package agent

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// TestParseBackupJobArgs pins the arg set `backup --control-plane`
// forwards: include_wal, incremental_from and stall_timeout reach the
// executor (stall_timeout was never read -- the executor only knew
// inactivity_timeout, which the CLI never sent).
func TestParseBackupJobArgs(t *testing.T) {
	a, err := parseBackupJobArgs(map[string]any{
		"fast": true, "label": "L", "include_wal": true,
		"incremental_from": "latest", "stall_timeout": "5m0s", "inactivity_timeout": "2m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.fast || a.label != "L" || !a.includeWAL || a.incrementalFrom != "latest" ||
		a.stall != 5*time.Minute || a.inactivity != 2*time.Minute {
		t.Fatalf("parsed = %+v", a)
	}
}

// TestParseBackupJobArgs_MalformedIsAnError: a malformed value used to
// fall back to the default silently (inactivity_timeout "banana" meant
// "use 90s"); now it fails the job so the operator sees it.
func TestParseBackupJobArgs_MalformedIsAnError(t *testing.T) {
	for _, args := range []map[string]any{
		{"inactivity_timeout": "banana"},
		{"stall_timeout": "-1m"},
		{"include_wal": "yes"},
		{"incremental_from": 7.0},
	} {
		if _, err := parseBackupJobArgs(args); err == nil {
			t.Errorf("args %v: want an error", args)
		}
	}
}

// TestBackupExecutor_IncrementalFromIsConsulted proves incremental_from
// is acted on (not dropped): with an unreadable parent the job fails in
// the incremental resolution, before any PostgreSQL connection.
func TestBackupExecutor_IncrementalFromIsConsulted(t *testing.T) {
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	repoURL := "file://" + t.TempDir() + "/no-repo-here"
	e := NewBackupExecutor(map[string]config.DeploymentConfig{
		"db1": {PGConnection: "postgres://u@127.0.0.1:1/db", Repo: repoURL},
	}, config.KMSConfig{}, signer, verifier)
	_, err = e.Execute(context.Background(), &ControlPlaneJob{
		ID: "j1", Kind: "backup", Deployment: "db1", RepoURL: repoURL,
		Args: map[string]any{"incremental_from": "db1.full.20260101T000000Z"},
	}, func(map[string]any) {})
	if err == nil || !strings.Contains(err.Error(), "incremental_from") {
		t.Fatalf("want an incremental_from resolution error; got %v", err)
	}
}

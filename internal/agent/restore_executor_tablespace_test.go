package agent

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// TestParseTablespaceMappingArg covers the JSON-arg → TablespaceRemap
// conversion the control-plane restore path uses.
func TestParseTablespaceMappingArg(t *testing.T) {
	t.Run("nil is nil", func(t *testing.T) {
		rm, err := parseTablespaceMappingArg(nil)
		if err != nil || rm != nil {
			t.Fatalf("nil -> (%v, %v); want (nil, nil)", rm, err)
		}
	})

	t.Run("[]any of strings", func(t *testing.T) {
		rm, err := parseTablespaceMappingArg([]any{"/old/ts=/new/ts"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rm) != 1 || rm[0].Old != "/old/ts" || rm[0].New != "/new/ts" {
			t.Fatalf("got %+v", rm)
		}
	})

	t.Run("native []string accepted", func(t *testing.T) {
		rm, err := parseTablespaceMappingArg([]string{"/a=/b"})
		if err != nil || len(rm) != 1 {
			t.Fatalf("got (%+v, %v)", rm, err)
		}
	})

	t.Run("control-char entry rejected", func(t *testing.T) {
		_, err := parseTablespaceMappingArg([]any{"/old/ts=/new/ts\n99999 /attacker"})
		if err == nil || !strings.Contains(err.Error(), "control character") {
			t.Fatalf("want control-character rejection; got %v", err)
		}
	})

	t.Run("non-string element rejected", func(t *testing.T) {
		_, err := parseTablespaceMappingArg([]any{42})
		if err == nil || !strings.Contains(err.Error(), "not a string") {
			t.Fatalf("want not-a-string error; got %v", err)
		}
	})

	t.Run("wrong type rejected", func(t *testing.T) {
		_, err := parseTablespaceMappingArg("oops-not-an-array")
		if err == nil || !strings.Contains(err.Error(), "array") {
			t.Fatalf("want type error; got %v", err)
		}
	})
}

// TestRestoreExecutor_TablespaceMapping_IsWiredAndValidated proves the
// executor actually consults tablespace_mapping (it was previously
// dropped on the floor): a malformed entry must surface as an error
// from Execute, before any restore work — confirming the arg reaches
// restore.Options.TablespaceRemap via the validating parser.
func TestRestoreExecutor_TablespaceMapping_IsWiredAndValidated(t *testing.T) {
	_, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, _ := backup.LoadVerifier(pub)

	e := NewRestoreExecutor(
		map[string]config.DeploymentConfig{"db1": {Repo: "file:///srv/repo"}},
		config.KMSConfig{}, verifier, "",
	)
	_, err = e.Execute(context.Background(), &ControlPlaneJob{
		Kind:       "restore",
		Deployment: "db1",
		RepoURL:    "file:///srv/repo",
		Args: map[string]any{
			"backup_id":  "some-backup", // not "latest" → no repo open before the gate
			"target_dir": "/tmp/restore-target",
			"tablespace_mapping": []any{
				"/old/ts=/new/ts\n99999 /attacker/path", // newline → injection attempt
			},
		},
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected error: a malformed tablespace_mapping must be rejected (proves it is no longer ignored)")
	}
	if !strings.Contains(err.Error(), "control character") {
		t.Errorf("expected control-character rejection through the control-plane path; got %v", err)
	}
}

// TestRestoreExecutor_RestoreRoots_GateEveryWriteRoot pins the agent
// half of restore_roots: when the control plane stamps its roots into
// the job, target_dir AND every tablespace_mapping destination must sit
// under them, and destinations must be normalised (no "/srv/../etc").
// The check runs before any repo work, so a job from a control plane
// that skipped the route check still cannot aim a tablespace at /etc.
func TestRestoreExecutor_RestoreRoots_GateEveryWriteRoot(t *testing.T) {
	_, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, _ := backup.LoadVerifier(pub)
	e := NewRestoreExecutor(
		map[string]config.DeploymentConfig{"db1": {Repo: "file:///srv/repo"}},
		config.KMSConfig{}, verifier, "",
	)
	run := func(args map[string]any) error {
		args["backup_id"] = "some-backup"
		_, err := e.Execute(context.Background(), &ControlPlaneJob{
			Kind: "restore", Deployment: "db1", RepoURL: "file:///srv/repo", Args: args,
		}, func(map[string]any) {})
		return err
	}
	roots := []any{"/srv/restores"}
	for name, args := range map[string]map[string]any{
		"mapping outside roots": {
			"target_dir": "/srv/restores/db1", "restore_roots": roots,
			"tablespace_mapping": []any{"/old/ts=/etc/cron.d"},
		},
		"target outside roots": {
			"target_dir": "/etc/pg", "restore_roots": roots,
		},
		"mapping not normalised": {
			"target_dir":         "/srv/restores/db1",
			"tablespace_mapping": []any{"/old/ts=/srv/restores/../../etc"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(args)
			if err == nil || !strings.Contains(err.Error(), "restore root") && !strings.Contains(err.Error(), "normalised") {
				t.Fatalf("want a restore-roots / normalisation refusal; got %v", err)
			}
		})
	}
}

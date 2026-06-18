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
		verifier, "",
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

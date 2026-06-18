package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// recordingKMSProvider registers a cloud KMS scheme whose builder records the
// cfg it receives (and unwraps by identity so the restore can decrypt). The
// returned getter yields the captured cfg.
func recordingKMSProvider(t *testing.T, scheme string) func() map[string]any {
	t.Helper()
	var mu sync.Mutex
	var cfg map[string]any
	kms.DefaultRegistry.Register(scheme, func(_ context.Context, ref string, c map[string]any) (kms.Provider, error) {
		mu.Lock()
		cfg = c
		mu.Unlock()
		return &identityKMSProvider{ref: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register(scheme, func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})
	return func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return cfg
	}
}

// TestRestore_CloudKMS_PassesKMSConfigToProvider is the end-to-end proof that
// `restore --kms-config region=...,endpoint=...` reaches the cloud KMS
// provider builder.
func TestRestore_CloudKMS_PassesKMSConfigToProvider(t *testing.T) {
	w := newReadWorld(t)
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	id := commitCloudEncryptedBackup(t, w, "db1", "kmscfg-restore://key", "data/file", dek, []byte("17\n"))
	target := t.TempDir() + "/restored"

	getCfg := recordingKMSProvider(t, "kmscfg-restore")

	stdout, stderr, exit := runCLI(t, "restore", "db1", id,
		"--repo", w.repoURL, "--target", target,
		"--kms-config", "region=eu-west-1,endpoint=https://kms.restore", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("restore exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	cfg := getCfg()
	if cfg == nil {
		t.Fatal("provider builder never called — restore did not reach the cloud KMS path")
	}
	if cfg["region"] != "eu-west-1" || cfg["endpoint"] != "https://kms.restore" {
		t.Errorf("provider cfg = %v, want region=eu-west-1 endpoint=https://kms.restore (the --kms-config flag)", cfg)
	}
}

// TestPartialRestore_CloudKMS_PassesKMSConfigToProvider is the end-to-end
// proof that `partial restore --kms-config ...` reaches the cloud KMS
// provider builder.
func TestPartialRestore_CloudKMS_PassesKMSConfigToProvider(t *testing.T) {
	w := newReadWorld(t)
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	id := commitCloudEncryptedBackup(t, w, "db1", "kmscfg-partial://key", "base/16384/2619", dek, []byte("heap-bytes"))

	// Relfilenode map: public.users → base/16384/2619.
	tmp := t.TempDir()
	mapPath := filepath.Join(tmp, "rfn.json")
	mp, _ := json.Marshal(map[string]map[string]any{
		"public.users": {"qualified": "public.users", "path": "base/16384/2619"},
	})
	if err := os.WriteFile(mapPath, mp, 0o600); err != nil {
		t.Fatal(err)
	}

	getCfg := recordingKMSProvider(t, "kmscfg-partial")

	target := filepath.Join(tmp, "extract")
	stdout, stderr, exit := runCLI(t, "partial", "restore", "db1",
		"--repo", w.repoURL, "--backup", id,
		"--tables", "public.users", "--relfilenode-map", mapPath,
		"--target", target,
		"--kms-config", "region=ap-south-1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("partial restore exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	cfg := getCfg()
	if cfg == nil {
		t.Fatal("provider builder never called — partial restore did not reach the cloud KMS path")
	}
	if cfg["region"] != "ap-south-1" {
		t.Errorf("provider cfg region = %v, want ap-south-1 (the --kms-config flag)", cfg["region"])
	}
}

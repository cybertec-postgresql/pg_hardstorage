// End-to-end proof for issue #44: a deployment that declares
// `kek_ref` in pg_hardstorage.yaml — with no --kek, no --kms-config,
// and no --encrypt — produces a backup whose manifest is wrapped by
// the named cloud KMS provider.
//
// The unit tests cover each hop (config parse → resolveDeploymentKMS →
// runner.ResolveEncryption → registry.Open). This one pins that the
// hops are actually connected in the shipped command, which is the
// part the issue was about: the documented config loaded fine in
// isolation but reached nothing.
//
//go:build integration

package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// recordingProvider wraps/unwraps with a fixed XOR so a round-trip is
// verifiable without a cloud account, and records the config map its
// builder was handed.
type recordingProvider struct {
	ref string
}

func (p *recordingProvider) Name() string   { return "rec-kms" }
func (p *recordingProvider) KEKRef() string { return p.ref }
func (p *recordingProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) {
	return xorAll(dek), nil
}
func (p *recordingProvider) UnwrapDEK(_ context.Context, wrapped []byte) ([]byte, error) {
	return xorAll(wrapped), nil
}
func (p *recordingProvider) Shred(_ context.Context) error { return nil }
func (p *recordingProvider) FIPSMode() bool                { return true }
func (p *recordingProvider) Close() error                  { return nil }

func xorAll(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ 0x5a
	}
	return out
}

func TestIntegration_DeploymentKEKRef_WrapsUnderConfiguredProvider(t *testing.T) {
	srv := testkit.StartPostgres(t)

	var (
		mu      sync.Mutex
		gotCfg  map[string]any
		gotRefs []string
	)
	kms.DefaultRegistry.Register("rec-kms", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		mu.Lock()
		defer mu.Unlock()
		gotCfg = cfg
		gotRefs = append(gotRefs, ref)
		return &recordingProvider{ref: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("rec-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("rec-kms: cleared")
		})
	})

	cfgDir := t.TempDir()
	keyringDir := t.TempDir()
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	// Stand up the repo the ordinary way. This also materialises a
	// local kek.bin, so the assertions below double as proof that a
	// configured cloud ref beats the on-disk KEK rather than racing it.
	if out, stderr, exit := runCmd(t,
		"init", "--yes",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--deployment", "db1",
		"--skip-backup",
		"--output", "json",
	); exit != 0 {
		t.Fatalf("init exit = %d\nstdout: %s\nstderr: %s", exit, out, stderr)
	}

	const kekRef = "rec-kms://acme-vault/db1-kek"
	// Exactly the shape docs/how-to/adding/kms-*.md teaches.
	cfgBody := "kms:\n" +
		"  providers:\n" +
		"    - kek_ref: " + kekRef + "\n" +
		"      config:\n" +
		"        region: eu-central-1\n" +
		"        use_fips_mode: true\n" +
		"deployments:\n" +
		"  db1:\n" +
		"    pg_connection: " + srv.DSN + "\n" +
		"    repo: " + repoURL + "\n" +
		"    kek_ref: " + kekRef + "\n"
	// Replace what init wrote: the operator's hand-edited file, in the
	// shape the how-tos teach.
	if err := os.WriteFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(keyringDir, "kek.bin")); err != nil {
		t.Fatalf("expected init to leave a local KEK to compete with: %v", err)
	}

	// No --kek, no --kms-config, no --encrypt, no --repo, no
	// --pg-connection: everything comes from the config file. Before
	// #44 this file didn't even parse.
	out, stderr, exit := runCmd(t, "backup", "db1", "--fast", "--output", "json")
	if exit != 0 {
		t.Fatalf("backup exit = %d\nstdout: %s\nstderr: %s", exit, out, stderr)
	}

	var resultDoc struct {
		Result struct {
			BackupID  string `json:"backup_id"`
			Encrypted bool   `json:"encrypted"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resultDoc); err != nil {
		t.Fatalf("decode result: %v\n%s", err, out)
	}
	if !resultDoc.Result.Encrypted {
		t.Error("a deployment with kek_ref produced an UNENCRYPTED backup — the config never reached the runner")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotRefs) == 0 {
		t.Fatal("the KMS provider was never opened; the backup fell back to local custody")
	}
	if gotRefs[0] != kekRef {
		t.Errorf("provider opened for %q, want %q", gotRefs[0], kekRef)
	}
	if gotCfg["region"] != "eu-central-1" || gotCfg["use_fips_mode"] != true {
		t.Errorf("builder config = %v, want the kms.providers entry (region + use_fips_mode)", gotCfg)
	}

	// The manifest must stamp the cloud ref, not local:default —
	// restore resolves the unwrap path from this field alone.
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	rc, err := sp.Get(context.Background(), backup.PrimaryPath("db1", resultDoc.Result.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var manifestRaw map[string]any
	if err := json.NewDecoder(rc).Decode(&manifestRaw); err != nil {
		t.Fatal(err)
	}
	encField, ok := manifestRaw["encryption"].(map[string]any)
	if !ok {
		t.Fatalf("manifest.encryption missing; manifest: %v", manifestRaw)
	}
	if encField["kek_ref"] != kekRef {
		t.Errorf("manifest kek_ref = %v, want %q", encField["kek_ref"], kekRef)
	}
	if encField["wrapped_dek"] == "" {
		t.Error("wrapped_dek empty")
	}
}

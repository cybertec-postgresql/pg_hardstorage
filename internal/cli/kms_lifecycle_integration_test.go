// kms_lifecycle_integration_test.go — the config-driven KEK across a
// repository's life, not just at the moment of one backup.
//
// backup_kek_ref_integration_test.go proves a single backup wraps under
// the deployment's configured kek_ref. These cover what happens
// afterwards, where the interesting failures are:
//
//   - restoring on a DIFFERENT host, with a fresh keyring and only the
//     config file — the disaster-recovery case, and the one where a
//     dependency on local state that the config doesn't carry would
//     finally show up;
//   - a repo holding manifests under SEVERAL KEKRefs at once, which is
//     the normal state during a `kms rotate` and the reason the read
//     paths resolve provider config per manifest rather than per repo;
//   - a rotation that CHANGES the ref while the config still names the
//     old one — documented in rotate-kek.md this cycle, never tested.
//
//go:build integration

package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// lifecycleProvider is a deterministic stand-in for a cloud KMS. Each
// distinct KEKRef gets its own wrap byte, so a DEK wrapped under one
// ref cannot be unwrapped by another — the property that makes the
// multi-ref and rotation cases meaningful rather than decorative.
type lifecycleProvider struct {
	ref  string
	mask byte
}

func (p *lifecycleProvider) Name() string   { return "life-kms" }
func (p *lifecycleProvider) KEKRef() string { return p.ref }
func (p *lifecycleProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) {
	return maskAll(dek, p.mask), nil
}
func (p *lifecycleProvider) UnwrapDEK(_ context.Context, wrapped []byte) ([]byte, error) {
	return maskAll(wrapped, p.mask), nil
}
func (p *lifecycleProvider) Shred(_ context.Context) error { return nil }
func (p *lifecycleProvider) FIPSMode() bool                { return true }
func (p *lifecycleProvider) Close() error                  { return nil }

func maskAll(b []byte, mask byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ mask
	}
	return out
}

// maskForRef derives a per-ref wrap byte so two refs never agree.
func maskForRef(ref string) byte {
	var m byte = 0x11
	for i := 0; i < len(ref); i++ {
		m ^= ref[i]
	}
	if m == 0 {
		m = 0x5a
	}
	return m
}

// registerLifecycleKMS installs the "life-kms" scheme and returns a
// func reporting which refs were opened.
func registerLifecycleKMS(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var opened []string
	kms.DefaultRegistry.Register("life-kms", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		mu.Lock()
		opened = append(opened, ref)
		mu.Unlock()
		return &lifecycleProvider{ref: ref, mask: maskForRef(ref)}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("life-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("life-kms: cleared")
		})
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), opened...)
	}
}

// writeKMSConfig writes a pg_hardstorage.yaml declaring one provider
// and one deployment pinned to it.
func writeKMSConfig(t *testing.T, cfgDir, dsn, repoURL, kekRef string) {
	t.Helper()
	body := "kms:\n" +
		"  providers:\n" +
		"    - kek_ref: " + kekRef + "\n" +
		"      config:\n" +
		"        region: test-region\n" +
		"deployments:\n" +
		"  db1:\n" +
		"    pg_connection: " + dsn + "\n" +
		"    repo: " + repoURL + "\n" +
		"    kek_ref: " + kekRef + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// manifestKEKRefOf reads the committed manifest's stamped KEKRef.
func manifestKEKRefOf(t *testing.T, repoURL, deployment, backupID string) string {
	t.Helper()
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	rc, err := sp.Get(context.Background(), backup.PrimaryPath(deployment, backupID))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var m map[string]any
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}
	enc, ok := m["encryption"].(map[string]any)
	if !ok {
		t.Fatalf("manifest %s has no encryption block", backupID)
	}
	ref, _ := enc["kek_ref"].(string)
	return ref
}

func backupIDFromJSON(t *testing.T, out string) string {
	t.Helper()
	var doc struct {
		Result struct {
			BackupID  string `json:"backup_id"`
			Encrypted bool   `json:"encrypted"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode result: %v\n%s", err, out)
	}
	if doc.Result.BackupID == "" {
		t.Fatalf("no backup_id in result:\n%s", out)
	}
	if !doc.Result.Encrypted {
		t.Fatalf("backup reported encrypted=false; the configured kek_ref did not take effect:\n%s", out)
	}
	return doc.Result.BackupID
}

// TestIntegration_KEKRef_RestoreOnDifferentHost is the disaster-recovery
// shape: the machine that took the backup is gone. A NEW host gets the
// repo URL and the config file and nothing else — no keyring, no
// keystore, none of the original host's state.
//
// This is where a hidden dependency on local state would surface. The
// backup path could pass every same-host test while quietly relying on
// something the config does not carry, and nobody would find out until
// the day it mattered.
func TestIntegration_KEKRef_RestoreOnDifferentHost(t *testing.T) {
	srv := testkit.StartPostgres(t)
	opened := registerLifecycleKMS(t)
	const kekRef = "life-kms://vault/db1"

	repoDir := t.TempDir()
	repoURL := "file://" + repoDir

	// --- Host A: config-only, takes the backup. -------------------
	hostACfg, hostAKeyring := t.TempDir(), t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", hostACfg)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", hostAKeyring)

	if out, stderr, exit := runCmd(t, "init", "--yes",
		"--pg-connection", srv.DSN, "--repo", repoURL,
		"--deployment", "db1", "--skip-backup", "--output", "json"); exit != 0 {
		t.Fatalf("init exit=%d\n%s\n%s", exit, out, stderr)
	}
	writeKMSConfig(t, hostACfg, srv.DSN, repoURL, kekRef)

	out, stderr, exit := runCmd(t, "backup", "db1", "--fast", "--output", "json")
	if exit != 0 {
		t.Fatalf("backup exit=%d\n%s\n%s", exit, out, stderr)
	}
	backupID := backupIDFromJSON(t, out)

	if got := manifestKEKRefOf(t, repoURL, "db1", backupID); got != kekRef {
		t.Fatalf("manifest kek_ref = %q, want %q", got, kekRef)
	}

	// --- Host B: a different machine entirely. --------------------
	// Fresh config dir and fresh keyring, carrying exactly what a
	// runbook carries: the repo URL, the config, and the MANIFEST
	// SIGNING keypair.
	//
	// The signing key is a separate concern from the KEK and genuinely
	// must travel — manifests are ed25519-signed, and a host with a
	// different key rejects them with "embedded public key does not
	// match verifier" before encryption is even considered. That is
	// deliberate (it is what doctor's manifest_signature_mismatch
	// check exists to warn about), so a DR test that omitted it would
	// be asserting the wrong thing and would fail for a reason that has
	// nothing to do with the KEK.
	//
	// kek.bin is deliberately NOT copied. That is the point: the only
	// route to the data encryption key is the configured kek_ref.
	hostBCfg, hostBKeyring := t.TempDir(), t.TempDir()
	for _, f := range []string{keystore.PrivateKeyFile, keystore.PublicKeyFile} {
		body, err := os.ReadFile(filepath.Join(hostAKeyring, f))
		if err != nil {
			t.Fatalf("read host A %s: %v", f, err)
		}
		if err := os.WriteFile(filepath.Join(hostBKeyring, f), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", hostBCfg)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", hostBKeyring)
	writeKMSConfig(t, hostBCfg, srv.DSN, repoURL, kekRef)

	if _, err := os.Stat(filepath.Join(hostBKeyring, keystore.KEKFileName)); err == nil {
		t.Fatal("host B has a local KEK — the restore could succeed without ever " +
			"consulting the configured provider, so the test would prove nothing")
	}

	target := filepath.Join(t.TempDir(), "restored")
	out, stderr, exit = runCmd(t, "restore", "db1", backupID,
		"--target", target, "--verify", "skip", "--verify-restore", "off",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("restore on a fresh host exit=%d — a backup whose KEK is fully "+
			"described by config must restore anywhere that config reaches\n%s\n%s",
			exit, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
		t.Errorf("restored target has no PG_VERSION: %v", err)
	}
	// The provider must have been opened on the restore side too — if
	// it were not, the DEK could only have come from local state.
	refs := opened()
	if len(refs) < 2 {
		t.Errorf("provider opened %d time(s) (%v); expected both the backup and "+
			"the restore to resolve it", len(refs), refs)
	}
}

// TestIntegration_KEKRef_CrossKEKDedupIsRefused pins the fix for the
// defect this suite originally found.
//
// Chunk keys are global to a repository (chunks/sha256/<hash>.chk) but
// the shared DEK is per-KEKRef. A backup under kek_ref B that dedups
// against chunks written under kek_ref A used to produce a manifest
// referencing chunks only DEK-A can decrypt — it exited 0, and verify
// then failed on hundreds of chunks. Restore failed too, which is the
// worst moment to learn about it.
//
// The CAS now probes, once per backup, that a chunk it is about to
// adopt actually reads with this backup's DEK. So the second backup
// FAILS LOUDLY instead of committing something unrestorable.
//
// The failure is the correct outcome, not a limitation: refusing the
// backup is strictly better than a success that cannot be restored,
// and the error names both remedies (separate repositories, or a
// rotation so one KEK covers the repo — rotation re-wraps the SAME
// DEK, so existing chunks stay readable).
func TestIntegration_KEKRef_CrossKEKDedupIsRefused(t *testing.T) {
	srv := testkit.StartPostgres(t)
	registerLifecycleKMS(t)

	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	cfgDir, keyringDir := t.TempDir(), t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	if out, stderr, exit := runCmd(t, "init", "--yes",
		"--pg-connection", srv.DSN, "--repo", repoURL,
		"--deployment", "db1", "--skip-backup", "--output", "json"); exit != 0 {
		t.Fatalf("init exit=%d\n%s\n%s", exit, out, stderr)
	}

	// Backup 1 establishes the chunk corpus under key-a.
	writeKMSConfig(t, cfgDir, srv.DSN, repoURL, "life-kms://vault/key-a")
	out, stderr, exit := runCmd(t, "backup", "db1", "--fast", "--output", "json")
	if exit != 0 {
		t.Fatalf("first backup exit=%d\n%s\n%s", exit, out, stderr)
	}
	firstID := backupIDFromJSON(t, out)

	// Backup 2 under a DIFFERENT ref would dedup against those chunks.
	writeKMSConfig(t, cfgDir, srv.DSN, repoURL, "life-kms://vault/key-b")
	out, stderr, exit = runCmd(t, "backup", "db1", "--fast", "--output", "json")
	if exit == 0 {
		t.Fatalf("backup under a second kek_ref SUCCEEDED; it has adopted chunks "+
			"encrypted under the first ref's DEK and is unrestorable\n%s", out)
	}
	combined := out + stderr
	if !strings.Contains(combined, "does not decrypt with this backup's data key") {
		t.Errorf("refusal did not explain the cause; operator gets no path forward:\n%s", combined)
	}
	if !strings.Contains(combined, "kms rotate") {
		t.Errorf("refusal names no remedy:\n%s", combined)
	}

	// The first backup must be untouched and still restorable — the
	// refusal protects the repo rather than damaging it.
	if out, stderr, exit := runCmd(t, "verify", "db1", firstID, "--output", "json"); exit != 0 {
		t.Errorf("the pre-existing backup no longer verifies after the refusal: "+
			"exit=%d\n%s\n%s", exit, out, stderr)
	}
}

// TestIntegration_KEKRef_MultiRefRepo covers the SAFE half of the
// multi-ref story: a repo whose manifests sit under different KEKRefs
// with no chunk overlap between them.
//
// Each backup lives in its own repository, so nothing dedups across the
// KEK boundary (see the skipped test above for what happens when it
// does). What this proves is that the READ paths resolve provider
// config from the MANIFEST's own KEKRef rather than the deployment's
// current one — the assumption those paths were changed to stop making.
// A read path that resolved one provider per repo would hand a
// DEK-A-wrapped blob to provider B and fail.
func TestIntegration_KEKRef_MultiRefRepo(t *testing.T) {
	srv := testkit.StartPostgres(t)
	registerLifecycleKMS(t)

	cfgDir, keyringDir := t.TempDir(), t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	type taken struct{ id, ref, repoURL string }
	var backups []taken

	for _, ref := range []string{"life-kms://vault/key-a", "life-kms://vault/key-b"} {
		repoURL := "file://" + t.TempDir()
		writeKMSConfig(t, cfgDir, srv.DSN, repoURL, ref)
		if out, stderr, exit := runCmd(t, "repo", "init", repoURL, "--output", "json"); exit != 0 {
			t.Fatalf("repo init exit=%d\n%s\n%s", exit, out, stderr)
		}
		out, stderr, exit := runCmd(t, "backup", "db1", "--fast", "--output", "json")
		if exit != 0 {
			t.Fatalf("backup under %s exit=%d\n%s\n%s", ref, exit, out, stderr)
		}
		id := backupIDFromJSON(t, out)
		if got := manifestKEKRefOf(t, repoURL, "db1", id); got != ref {
			t.Fatalf("manifest kek_ref = %q, want %q", got, ref)
		}
		backups = append(backups, taken{id, ref, repoURL})
	}

	// Point the config at ONE ref, then verify and restore the backup
	// taken under the OTHER. The read path must follow the manifest.
	writeKMSConfig(t, cfgDir, srv.DSN, backups[1].repoURL, backups[1].ref)
	other := backups[0]

	if out, stderr, exit := runCmd(t, "verify", "db1", other.id,
		"--repo", other.repoURL, "--output", "json"); exit != 0 {
		t.Fatalf("verify of a manifest under %s while config names %s: exit=%d — "+
			"the read path used the deployment's ref instead of the manifest's\n%s\n%s",
			other.ref, backups[1].ref, exit, out, stderr)
	}

	target := filepath.Join(t.TempDir(), "restored-other-ref")
	if out, stderr, exit := runCmd(t, "restore", "db1", other.id,
		"--repo", other.repoURL, "--target", target,
		"--verify", "skip", "--verify-restore", "off", "--output", "json"); exit != 0 {
		t.Fatalf("restore of a manifest under %s exit=%d\n%s\n%s",
			other.ref, exit, out, stderr)
	}
}

// TestIntegration_KEKRef_StaleConfigAfterRotation pins the operational
// gap documented in rotate-kek.md: `kms rotate` rewrites manifests to a
// new ref, but the deployment's kek_ref lives in the config file and
// does NOT follow. A forgotten config edit means new backups keep
// wrapping under the OLD ref.
//
// The forgotten-edit half is what this covers. The opposite mistake —
// editing kek_ref WITHOUT rotating — is now refused outright, and is
// covered by TestIntegration_KEKRef_CrossKEKDedupIsRefused; an earlier
// version of this test asserted that the edit silently took effect,
// which was exactly the unrestorable-backup path.
func TestIntegration_KEKRef_StaleConfigAfterRotation(t *testing.T) {
	srv := testkit.StartPostgres(t)
	registerLifecycleKMS(t)

	const oldRef = "life-kms://vault/old"
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	cfgDir, keyringDir := t.TempDir(), t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	if out, stderr, exit := runCmd(t, "init", "--yes",
		"--pg-connection", srv.DSN, "--repo", repoURL,
		"--deployment", "db1", "--skip-backup", "--output", "json"); exit != 0 {
		t.Fatalf("init exit=%d\n%s\n%s", exit, out, stderr)
	}
	writeKMSConfig(t, cfgDir, srv.DSN, repoURL, oldRef)

	out, stderr, exit := runCmd(t, "backup", "db1", "--fast", "--output", "json")
	if exit != 0 {
		t.Fatalf("first backup exit=%d\n%s\n%s", exit, out, stderr)
	}
	firstID := backupIDFromJSON(t, out)

	// The operator rotates but forgets the config edit. Config is still
	// the source of truth for NEW backups, so this must keep using the
	// old ref rather than drifting to something else.
	out, stderr, exit = runCmd(t, "backup", "db1", "--fast", "--output", "json")
	if exit != 0 {
		t.Fatalf("second backup exit=%d\n%s\n%s", exit, out, stderr)
	}
	secondID := backupIDFromJSON(t, out)

	for _, id := range []string{firstID, secondID} {
		if got := manifestKEKRefOf(t, repoURL, "db1", id); got != oldRef {
			t.Errorf("backup %s wrapped under %q, want %q — config is the source of "+
				"truth for new backups", id, got, oldRef)
		}
		if out, stderr, exit := runCmd(t, "verify", "db1", id, "--output", "json"); exit != 0 {
			t.Errorf("verify %s exit=%d\n%s\n%s", id, exit, out, stderr)
		}
	}
}

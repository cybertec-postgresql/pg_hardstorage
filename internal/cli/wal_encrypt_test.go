package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// fakeWALKMS is a reversible fake cloud-KMS provider for tests: WrapDEK XORs
// the DEK with a constant (a non-identity wrap, so the envelope is a genuine
// wrap distinct from the raw DEK), UnwrapDEK reverses it.
type fakeWALKMS struct{ kekRef string }

func xorMask(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ 0x5A
	}
	return out
}

func (f *fakeWALKMS) Name() string                                          { return "fake-wal-kms" }
func (f *fakeWALKMS) KEKRef() string                                        { return f.kekRef }
func (f *fakeWALKMS) WrapDEK(_ context.Context, dek []byte) ([]byte, error) { return xorMask(dek), nil }
func (f *fakeWALKMS) UnwrapDEK(_ context.Context, w []byte) ([]byte, error) { return xorMask(w), nil }
func (f *fakeWALKMS) Shred(_ context.Context) error                         { return nil }
func (f *fakeWALKMS) FIPSMode() bool                                        { return false }
func (f *fakeWALKMS) Close() error                                          { return nil }

// registerFakeWALKMS adds the fake cloud-KMS scheme for the duration of t.
func registerFakeWALKMS(t *testing.T) {
	t.Helper()
	kms.DefaultRegistry.Register("fake-wal-kms", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		return &fakeWALKMS{kekRef: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("fake-wal-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("fake-wal-kms: cleared")
		})
	})
}

// TestWalPushFetch_CloudKMSRoundTrip is the #108 proof: with NO local kek.bin,
// `wal push --kek <cloud> --kms-config ...` encrypts the segment under a DEK
// minted+wrapped by the cloud provider, stamps the cloud envelope on the
// manifest, and `wal fetch` resolves the DEK back through the provider and
// reproduces the segment byte-for-byte. The chunks are ciphertext at rest.
func TestWalPushFetch_CloudKMSRoundTrip(t *testing.T) {
	registerFakeWALKMS(t)
	// No keyring/kek.bin: cloud KMS must not require one.
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)
	repoRoot := repoURL[len("file://"):]
	kekRef := "fake-wal-kms://prod-key"

	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte((i*13 + 7) % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, errb, exit := runCLI(t, "wal", "push", "db1", segPath,
		"--repo", repoURL, "--system-identifier", "7000000000000000001",
		"--kek", kekRef, "--kms-config", "region=eu-central-1", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("wal push (cloud) exit=%d\n%s", exit, errb)
	}

	segManifest := filepath.Join(repoRoot, "wal", "db1", "00000001", segmentName+".json")
	sraw, err := os.ReadFile(segManifest)
	if err != nil {
		t.Fatalf("read segment manifest: %v", err)
	}
	var sm walsink.SegmentManifest
	if err := json.Unmarshal(sraw, &sm); err != nil {
		t.Fatalf("parse segment manifest: %v", err)
	}
	if sm.Encryption == nil || sm.Encryption.KEKRef != kekRef || sm.Encryption.Scheme != "aes-256-gcm" {
		t.Fatalf("segment manifest must record the cloud envelope; got %+v", sm.Encryption)
	}
	assertChunksAreCiphertext(t, repoRoot, body)

	target := filepath.Join(t.TempDir(), "restored.wal")
	if _, errb, exit := runCLI(t, "wal", "fetch", "db1", segmentName, target,
		"--repo", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("wal fetch (cloud) exit=%d\n%s", exit, errb)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read fetched segment: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cloud-KMS fetched segment differs from original (%d vs %d bytes)", len(got), len(body))
	}
}

// TestWalPushFetch_EncryptedRoundTrip is the issue-#106 end-to-end CLI proof
// (no PostgreSQL needed): with a local KEK present, `wal push` writes the
// segment encrypted under the shared DEK and stamps the envelope on the
// manifest; the chunks at rest are ciphertext; and `wal fetch` resolves the
// DEK from the segment's own envelope (via the keyring) and reproduces the
// segment byte-for-byte.
func TestWalPushFetch_EncryptedRoundTrip(t *testing.T) {
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)
	// Establish a local KEK — this is the condition under which WAL encrypts.
	if _, _, err := keystore.LoadOrGenerateKEK(keyringDir); err != nil {
		t.Fatalf("LoadOrGenerateKEK: %v", err)
	}

	repoURL := initRepoForTest(t)

	// Synthetic 16 MiB segment with non-trivial content.
	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte((i*11 + 5) % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// Push (archive_command shape: explicit system identifier, no PG).
	if _, errb, exit := runCLI(t, "wal", "push", "db1", segPath,
		"--repo", repoURL, "--system-identifier", "7000000000000000001", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("wal push exit=%d\n%s", exit, errb)
	}

	// The committed segment manifest must record an aes-256-gcm envelope.
	manifestKey := filepath.Join(repoURL[len("file://"):], "wal", "db1", "00000001", segmentName+".json")
	raw, err := os.ReadFile(manifestKey)
	if err != nil {
		t.Fatalf("read segment manifest: %v", err)
	}
	var m walsink.SegmentManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse segment manifest: %v", err)
	}
	if m.Encryption == nil || m.Encryption.Scheme != "aes-256-gcm" || m.Encryption.KEKRef != keystore.KEKRefLocal {
		t.Fatalf("segment manifest must carry the shared-DEK envelope; got %+v", m.Encryption)
	}

	// The stored chunks must be ciphertext: the manifest's plaintext bytes
	// must not appear verbatim in any chunk object on disk.
	assertChunksAreCiphertext(t, repoURL[len("file://"):], body)

	// Fetch resolves the DEK from the segment envelope and reproduces bytes.
	target := filepath.Join(t.TempDir(), "restored.wal")
	if _, errb, exit := runCLI(t, "wal", "fetch", "db1", segmentName, target,
		"--repo", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("wal fetch exit=%d\n%s", exit, errb)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read fetched segment: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("fetched segment differs from original (%d vs %d bytes)", len(got), len(body))
	}
}

// TestWalFetch_EncryptedWithoutKeyringFails: an encrypted segment cannot be
// fetched when the keyring is unreachable — recovery needs key access, the
// same as an encrypted base-backup restore. The error must be explicit, not a
// silent garbage write.
func TestWalFetch_EncryptedWithoutKeyringFails(t *testing.T) {
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)
	if _, _, err := keystore.LoadOrGenerateKEK(keyringDir); err != nil {
		t.Fatalf("LoadOrGenerateKEK: %v", err)
	}
	repoURL := initRepoForTest(t)

	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errb, exit := runCLI(t, "wal", "push", "db1", segPath,
		"--repo", repoURL, "--system-identifier", "7000000000000000001", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("wal push exit=%d\n%s", exit, errb)
	}

	// Point the keyring at an empty dir (no kek.bin) → DEK unresolvable.
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	target := filepath.Join(t.TempDir(), "restored.wal")
	_, errb, exit := runCLI(t, "wal", "fetch", "db1", segmentName, target, "--repo", repoURL)
	if exit == int(output.ExitOK) {
		t.Fatalf("fetch of an encrypted segment must fail without the keyring; got exit OK\n%s", errb)
	}
}

// TestWalPush_MixedCloudPostureIsRefused guards the divergence hazard: a
// local kek.bin is present, but the deployment's backups are encrypted under a
// CLOUD KEKRef. Neither encrypting WAL under the local KEK nor falling back to
// plaintext is safe in a global plaintext-addressed CAS: either posture can
// deduplicate against bytes the other writer cannot restore. Refuse before a
// segment manifest is committed.
func TestWalPush_MixedCloudPostureIsRefused(t *testing.T) {
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)
	if _, _, err := keystore.LoadOrGenerateKEK(keyringDir); err != nil {
		t.Fatalf("LoadOrGenerateKEK: %v", err)
	}
	repoURL := initRepoForTest(t)
	repoRoot := repoURL[len("file://"):]

	// Plant a base-backup manifest encrypted under a cloud KEKRef.
	bm := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.cloud", Deployment: "db1",
		Type: backup.BackupTypeFull,
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: "aws-kms://alias/foo",
			WrappedDEK: "ZHVtbXk=", EnvelopeVersion: 2,
		},
	}
	raw, err := json.Marshal(bm)
	if err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(repoRoot, "manifests", "db1", "backups", "db1.full.cloud", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(mPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Push a WAL segment — it must be refused before committing a manifest.
	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte(i % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errb, exit := runCLI(t, "wal", "push", "db1", segPath,
		"--repo", repoURL, "--system-identifier", "7000000000000000001", "-o", "json"); exit == int(output.ExitOK) {
		t.Fatalf("wal push unexpectedly accepted a conflicting encryption posture\n%s", errb)
	} else if !strings.Contains(errb, "undecryptable dedup collision") {
		t.Fatalf("wal push returned the wrong refusal\n%s", errb)
	}

	segManifest := filepath.Join(repoRoot, "wal", "db1", "00000001", segmentName+".json")
	if _, err := os.Stat(segManifest); !os.IsNotExist(err) {
		t.Fatalf("conflicting WAL push committed a segment manifest: %v", err)
	}
}

// assertChunksAreCiphertext walks chunks/ under repoRoot and fails if any
// chunk object contains a long run of the plaintext body verbatim.
func assertChunksAreCiphertext(t *testing.T, repoRoot string, plaintext []byte) {
	t.Helper()
	// A distinctive 4 KiB window from the middle of the plaintext.
	needle := plaintext[len(plaintext)/2 : len(plaintext)/2+4096]
	chunksDir := filepath.Join(repoRoot, "chunks")
	found := false
	_ = filepathWalk(chunksDir, func(path string, isDir bool) error {
		if isDir {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(b, needle) {
			found = true
		}
		return nil
	})
	if found {
		t.Error("plaintext WAL bytes found verbatim in a chunk object — segment is NOT encrypted at rest")
	}
}

// filepathWalk is a tiny dependency-free directory walker (avoids pulling
// io/fs ceremony into the test).
func filepathWalk(root string, fn func(path string, isDir bool) error) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if e.IsDir() {
			if err := fn(p, true); err != nil {
				return err
			}
			if err := filepathWalk(p, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(p, false); err != nil {
			return err
		}
	}
	return nil
}

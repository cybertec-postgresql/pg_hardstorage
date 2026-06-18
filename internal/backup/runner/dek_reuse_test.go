package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"iter"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func newRunnerSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatalf("fs.Open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func putManifest(t *testing.T, sp storage.StoragePlugin, key string, m *backup.Manifest) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// TestLooksLikePrimaryManifest covers the path classifier — replicas
// and tombstones must not be treated as primary manifests, otherwise
// dek-reuse could lock in on a stale or pseudo entry.
func TestLooksLikePrimaryManifest(t *testing.T) {
	for _, c := range []struct {
		key  string
		want bool
	}{
		{"manifests/db1/backups/X/manifest.json", true},
		{"manifests/_replicas/X.manifest.json", false},
		{"manifests/db1/backups/X/manifest.json.tombstone", false},
		{"chunks/abcd", false},
		{"", false},
	} {
		if got := looksLikePrimaryManifest(c.key); got != c.want {
			t.Errorf("looksLikePrimaryManifest(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestUnwrapDEKForReuse_LocalKEK_RoundTrip: a DEK wrapped with the
// local-KEK path unwraps back to the same bytes via unwrapDEKForReuse.
// This is the building block runner.Take uses to load an existing
// DEK before constructing the per-backup encryptor.
func TestUnwrapDEKForReuse_LocalKEK_RoundTrip(t *testing.T) {
	var kek [encryption.KeyLen]byte
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	cfg := &EncryptionConfig{KEK: kek, KEKRef: "local:default"}
	got, err := unwrapDEKForReuse(context.Background(), cfg, wrapped)
	if err != nil {
		t.Fatalf("unwrapDEKForReuse: %v", err)
	}
	if !bytes.Equal(got, dek[:]) {
		t.Errorf("unwrapped DEK differs from original")
	}
}

// listErrSP injects a List error to simulate a transient backend failure
// during DEK lookup.
type listErrSP struct {
	storage.StoragePlugin
	err error
}

func (s listErrSP) List(_ context.Context, _ string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		yield(storage.ObjectInfo{}, s.err)
	}
}

func TestSelectDEK_EmptyRepoGeneratesFresh(t *testing.T) {
	sp := newRunnerSP(t)
	cfg := &EncryptionConfig{KEKRef: "local:default"}
	dek, err := selectDEK(context.Background(), sp, "local:default", cfg)
	if err != nil {
		t.Fatalf("selectDEK on empty repo: %v", err)
	}
	if dek == ([encryption.KeyLen]byte{}) {
		t.Error("fresh DEK must be non-zero")
	}
}

func TestSelectDEK_ReusesExistingDEK(t *testing.T) {
	sp := newRunnerSP(t)
	var kek, want [encryption.KeyLen]byte
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	for i := range want {
		want[i] = byte(0x40 + i)
	}
	wrapped, err := encryption.Wrap(kek, want)
	if err != nil {
		t.Fatal(err)
	}
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.x", Deployment: "db1",
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: "local:default",
			WrappedDEK: base64.StdEncoding.EncodeToString(wrapped), EnvelopeVersion: 2,
		},
	}
	putManifest(t, sp, "manifests/db1/backups/db1.full.x/manifest.json", m)
	cfg := &EncryptionConfig{KEK: kek, KEKRef: "local:default"}
	got, err := selectDEK(context.Background(), sp, "local:default", cfg)
	if err != nil {
		t.Fatalf("selectDEK: %v", err)
	}
	if got != want {
		t.Errorf("reused DEK = %x, want %x", got, want)
	}
}

// TestSelectDEK_ListErrorFailsNotFresh pins the core fix: a transient List
// error must FAIL the backup, not silently generate a fresh DEK that the
// CAS's plaintext-hash dedup would leave unrestorable against existing
// chunks.
func TestSelectDEK_ListErrorFailsNotFresh(t *testing.T) {
	sp := listErrSP{StoragePlugin: newRunnerSP(t), err: errors.New("boom: transient list failure")}
	cfg := &EncryptionConfig{KEKRef: "local:default"}
	if _, err := selectDEK(context.Background(), sp, "local:default", cfg); err == nil {
		t.Fatal("selectDEK must FAIL on a List error, not silently generate a fresh DEK")
	}
}

// TestSelectDEK_UnwrappableExistingFailsNotFresh: a prior DEK exists for
// the KEK but won't unwrap (wrong KEK material / corrupt blob) → fail,
// never fork into a fresh DEK.
func TestSelectDEK_UnwrappableExistingFailsNotFresh(t *testing.T) {
	sp := newRunnerSP(t)
	bogus := make([]byte, encryption.WrappedKeyLen)
	for i := range bogus {
		bogus[i] = byte(i)
	}
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.x", Deployment: "db1",
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: "local:default",
			WrappedDEK: base64.StdEncoding.EncodeToString(bogus), EnvelopeVersion: 2,
		},
	}
	putManifest(t, sp, "manifests/db1/backups/db1.full.x/manifest.json", m)
	var kek [encryption.KeyLen]byte
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	cfg := &EncryptionConfig{KEK: kek, KEKRef: "local:default"}
	if _, err := selectDEK(context.Background(), sp, "local:default", cfg); err == nil {
		t.Fatal("selectDEK must FAIL when a prior DEK exists but can't be unwrapped, not generate a fresh DEK")
	}
}

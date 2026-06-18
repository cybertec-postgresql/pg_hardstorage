package partial_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// setup builds an init'd file:// repo and a signer/verifier pair.
type partialWorld struct {
	repoURL  string
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
}

func setupPartialWorld(t *testing.T) *partialWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return &partialWorld{
		repoURL:  repoURL,
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
	}
}

// commitWithFiles writes a real (signed) manifest with the given
// FileEntries against w.sp. Each file's chunks are pre-written to
// the CAS so a subsequent restore call can find them.
func (w *partialWorld) commitWithFiles(t *testing.T, deployment, backupID string, files []fileSpec) {
	t.Helper()
	cas := casdefault.New(w.sp)
	var entries []backup.FileEntry
	for _, f := range files {
		// Plant each chunk into the CAS; collect ChunkRefs.
		var refs []backup.ChunkRef
		var size int64
		for _, body := range f.chunks {
			info, err := cas.PutChunk(context.Background(), body)
			if err != nil {
				t.Fatalf("PutChunk: %v", err)
			}
			refs = append(refs, backup.ChunkRef{
				Hash:   info.Hash,
				Offset: size,
				Len:    int64(len(body)),
			})
			size += int64(len(body))
		}
		entries = append(entries, backup.FileEntry{
			Path:   f.path,
			Size:   size,
			Mode:   0o600,
			Chunks: refs,
		})
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            entries,
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// fileSpec is a tiny test struct describing one manifest entry.
type fileSpec struct {
	path   string
	chunks [][]byte
}

// TestPartialRestore_SelectsOnlyRequestedRelfilenode: a manifest
// with three tables' heap files; partial.Restore against one of
// them writes ONLY that table's heap (and TOAST when present),
// skipping the others.
func TestPartialRestore_SelectsOnlyRequestedRelfilenode(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.partial-x", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("users-page-0")}},
		{"base/16384/2619_vm", [][]byte{[]byte("users-vm")}},
		{"base/16384/2620", [][]byte{[]byte("orders-page-0")}},
		{"base/16384/2620.1", [][]byte{[]byte("orders-page-1")}},
		{"base/16384/2999", [][]byte{[]byte("events-page-0")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.partial-x",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Schema:    "public",
				Table:     "users",
				Qualified: "public.users",
				Path:      "base/16384/2619",
			},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// users heap + _vm should be there. orders + events should NOT.
	wantPresent := []string{
		"base/16384/2619",
		"base/16384/2619_vm",
	}
	wantAbsent := []string{
		"base/16384/2620",
		"base/16384/2620.1",
		"base/16384/2999",
	}
	for _, p := range wantPresent {
		if _, err := os.Stat(filepath.Join(target, p)); err != nil {
			t.Errorf("expected %q to be present: %v", p, err)
		}
	}
	for _, p := range wantAbsent {
		if _, err := os.Stat(filepath.Join(target, p)); err == nil {
			t.Errorf("%q should NOT have been extracted (different table)", p)
		}
	}
	if res.FilesWritten != 2 {
		t.Errorf("FilesWritten=%d, want 2", res.FilesWritten)
	}
}

// TestPartialRestore_ToastFamilyIncluded: when the relfilenode
// has a ToastPath, both the heap family AND the TOAST family are
// extracted.
func TestPartialRestore_ToastFamilyIncluded(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.with-toast", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("heap")}},
		{"base/16384/16400", [][]byte{[]byte("toast")}},
		{"base/16384/16400.1", [][]byte{[]byte("toast-seg-1")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.with-toast",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Schema:    "public",
				Table:     "users",
				Qualified: "public.users",
				Path:      "base/16384/2619",
				ToastPath: "base/16384/16400",
			},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, p := range []string{"base/16384/2619", "base/16384/16400", "base/16384/16400.1"} {
		if _, err := os.Stat(filepath.Join(target, p)); err != nil {
			t.Errorf("expected %q present: %v", p, err)
		}
	}
	if res.FilesWritten != 3 {
		t.Errorf("FilesWritten=%d, want 3", res.FilesWritten)
	}
	if len(res.Mappings) != 1 || res.Mappings[0].ToastPath != "base/16384/16400" {
		t.Errorf("mapping toast info missing: %+v", res.Mappings)
	}
}

// TestPartialRestore_NotFoundTable_PropagatesAndContinues: a table
// missing from the relfilenode map is recorded in NotFound; other
// tables still extract.
func TestPartialRestore_NotFoundTable_PropagatesAndContinues(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.mixed", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("real-table")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.mixed",
		Verifier:   w.verifier,
		Tables:     []string{"public.users", "public.does_not_exist"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Qualified: "public.users",
				Path:      "base/16384/2619",
			},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.NotFound) != 1 || res.NotFound[0] != "public.does_not_exist" {
		t.Errorf("NotFound=%+v, want [public.does_not_exist]", res.NotFound)
	}
	if res.FilesWritten != 1 {
		t.Errorf("FilesWritten=%d, want 1 (real table only)", res.FilesWritten)
	}
}

// TestPartialRestore_RefusesNonEmptyTarget: by default, a non-empty
// target dir is refused.
func TestPartialRestore_RefusesNonEmptyTarget(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.x", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("x")}},
	})

	target := filepath.Join(t.TempDir(), "with-content")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "garbage"), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.x",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Qualified: "public.users",
				Path:      "base/16384/2619",
			},
		},
		TargetDir: target,
	})
	if err == nil {
		t.Error("expected error for non-empty target without --force")
	}
}

// TestPartialRestore_AllowOverwrite_ProceedsAnyway: with
// AllowOverwrite=true, a non-empty target is permitted.
func TestPartialRestore_AllowOverwrite_ProceedsAnyway(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.y", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("y")}},
	})

	target := filepath.Join(t.TempDir(), "with-content")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "garbage"), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.y",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {Qualified: "public.users", Path: "base/16384/2619"},
		},
		TargetDir:      target,
		AllowOverwrite: true,
	})
	if err != nil {
		t.Errorf("--force should allow the restore: %v", err)
	}
}

// TestPartialRestore_ValidationErrors: required fields surface as
// usage errors (ErrEmptyTables, ErrNoTableResolution, etc.).
func TestPartialRestore_ValidationErrors(t *testing.T) {
	w := setupPartialWorld(t)
	base := partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.x",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {Qualified: "public.users", Path: "base/16384/2619"},
		},
		TargetDir: t.TempDir(),
	}
	cases := []struct {
		name string
		mut  func(o *partial.RestoreOptions)
	}{
		{"empty tables", func(o *partial.RestoreOptions) { o.Tables = nil }},
		{"unqualified table", func(o *partial.RestoreOptions) {
			o.Tables = []string{"users"}
		}},
		{"no resolution", func(o *partial.RestoreOptions) {
			o.RelfilenodeMap = nil
			o.PGConnString = ""
		}},
		{"both resolution paths", func(o *partial.RestoreOptions) {
			o.PGConnString = "postgres:///x"
		}},
		{"no target", func(o *partial.RestoreOptions) { o.TargetDir = "" }},
		{"no verifier", func(o *partial.RestoreOptions) { o.Verifier = nil }},
	}
	for _, c := range cases {
		opts := base
		c.mut(&opts)
		if _, err := partial.Restore(context.Background(), opts); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

// TestPartialRestore_FamilyMatcher_PrefixCollisionAvoided: a path
// like "base/16384/2619" must NOT pull in "base/16384/26190"
// (different relation that happens to share a prefix). This is
// the precision-of-family-walk invariant.
func TestPartialRestore_FamilyMatcher_PrefixCollisionAvoided(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.collision", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("ours")}},
		{"base/16384/26190", [][]byte{[]byte("not-ours")}},        // numeric prefix collision
		{"base/16384/2619.1", [][]byte{[]byte("ours-seg-1")}},     // legitimate family member
		{"base/16384/2619a", [][]byte{[]byte("not-ours-letter")}}, // non-digit suffix
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.collision",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {Qualified: "public.users", Path: "base/16384/2619"},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	wantPresent := []string{"base/16384/2619", "base/16384/2619.1"}
	wantAbsent := []string{"base/16384/26190", "base/16384/2619a"}
	for _, p := range wantPresent {
		if _, err := os.Stat(filepath.Join(target, p)); err != nil {
			t.Errorf("expected %q to be present: %v", p, err)
		}
	}
	for _, p := range wantAbsent {
		if _, err := os.Stat(filepath.Join(target, p)); err == nil {
			t.Errorf("%q must NOT match the family of base/16384/2619", p)
		}
	}
	if res.FilesWritten != 2 {
		t.Errorf("FilesWritten=%d, want 2", res.FilesWritten)
	}
}

// TestPartialRestore_EncryptedRoundTrip: plant an encrypted backup
// (chunks written via an encryption-aware CAS, manifest has
// EncryptionInfo populated), then partial-restore with the matching
// KEK. The extracted bytes should match the original plaintext.
//
// This is the canonical "encrypted backups now supported" test
// that the previous commit deferred to a follow-up.
func TestPartialRestore_EncryptedRoundTrip(t *testing.T) {
	w := setupPartialWorld(t)

	// Generate a KEK + DEK; wrap the DEK; build the matching
	// encryption-aware CAS for planting chunks.
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	var dek [encryption.KeyLen]byte
	if _, err := rand.Read(dek[:]); err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	cas := casdefault.NewEncrypted(w.sp, enc)

	// Plant the heap chunk via the encryption-aware CAS — bytes on
	// disk are encrypted with the per-chunk derived key.
	heapBody := []byte("encrypted-heap-body")
	info, err := cas.PutChunk(context.Background(), heapBody)
	if err != nil {
		t.Fatalf("PutChunk encrypted: %v", err)
	}

	// Build the manifest with EncryptionInfo populated.
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.encrypted",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          "local:default",
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "base/16384/2619",
			Size: int64(len(heapBody)),
			Mode: 0o600,
			Chunks: []backup.ChunkRef{{
				Hash:   info.Hash,
				Offset: 0,
				Len:    int64(len(heapBody)),
			}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit encrypted: %v", err)
	}

	// Run partial.Restore with the matching KEK resolver.
	target := filepath.Join(t.TempDir(), "extract-encrypted")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.encrypted",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {Qualified: "public.users", Path: "base/16384/2619"},
		},
		TargetDir: target,
		KEKForRef: func(ref string) ([encryption.KeyLen]byte, error) {
			if ref != "local:default" {
				return [encryption.KeyLen]byte{}, fmt.Errorf("unexpected ref %q", ref)
			}
			return kek, nil
		},
	})
	if err != nil {
		t.Fatalf("encrypted Restore: %v", err)
	}
	if res.FilesWritten != 1 {
		t.Errorf("FilesWritten=%d, want 1", res.FilesWritten)
	}

	// Confirm the extracted bytes are the plaintext.
	got, err := os.ReadFile(filepath.Join(target, "base/16384/2619"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(heapBody) {
		t.Errorf("extracted bytes wrong: got %q want %q", got, heapBody)
	}
}

// TestPartialRestore_EncryptedWithoutKEK_Errors: an encrypted
// backup with KEKForRef=nil surfaces a structured "no KEK
// resolver" error rather than silently producing junk bytes.
func TestPartialRestore_EncryptedWithoutKEK_Errors(t *testing.T) {
	w := setupPartialWorld(t)

	var kek, dek [encryption.KeyLen]byte
	rand.Read(kek[:])
	rand.Read(dek[:])
	wrapped, _ := encryption.Wrap(kek, dek)
	enc, _ := aesgcm.New(dek[:])
	cas := casdefault.NewEncrypted(w.sp, enc)
	info, err := cas.PutChunk(context.Background(), []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.enc-no-kek",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028", StopLSN: "0/30001A0",
		Timeline:    1,
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          "local:default",
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "base/16384/2619",
			Size: 4, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: 4}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "extract-no-kek")
	_, err = partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.enc-no-kek",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {Qualified: "public.users", Path: "base/16384/2619"},
		},
		TargetDir: target,
		KEKForRef: nil, // <- the key omission this test exercises
	})
	if err == nil {
		t.Fatal("expected error for encrypted backup with nil KEKForRef")
	}
	if !strings.Contains(err.Error(), "KEK") {
		t.Errorf("error should mention KEK: %v", err)
	}
}

// TestPartialRestore_EncryptedWrongKEK_Errors: supplying a KEK
// that doesn't match the manifest's wrapped DEK surfaces an unwrap
// failure with a clear message.
func TestPartialRestore_EncryptedWrongKEK_Errors(t *testing.T) {
	w := setupPartialWorld(t)

	var rightKEK, wrongKEK, dek [encryption.KeyLen]byte
	rand.Read(rightKEK[:])
	rand.Read(wrongKEK[:])
	rand.Read(dek[:])
	wrapped, _ := encryption.Wrap(rightKEK, dek)
	enc, _ := aesgcm.New(dek[:])
	cas := casdefault.NewEncrypted(w.sp, enc)
	info, err := cas.PutChunk(context.Background(), []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.wrong-kek",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028", StopLSN: "0/30001A0",
		Timeline:    1,
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          "local:default",
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "base/16384/2619",
			Size: 4, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: 4}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "extract-wrong-kek")
	_, err = partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.wrong-kek",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {Qualified: "public.users", Path: "base/16384/2619"},
		},
		TargetDir: target,
		KEKForRef: func(ref string) ([encryption.KeyLen]byte, error) {
			return wrongKEK, nil // doesn't match the wrap
		},
	})
	if err == nil {
		t.Fatal("expected unwrap failure with the wrong KEK")
	}
	if !strings.Contains(err.Error(), "unwrap") {
		t.Errorf("error should mention 'unwrap': %v", err)
	}
}

// TestPartialRestore_BytesMatchManifest: the BytesWritten counter
// equals the sum of the extracted files' Size fields.
func TestPartialRestore_BytesMatchManifest(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.bytes", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("aaaa"), []byte("bbbb")}}, // 8 bytes
		{"base/16384/2619.1", [][]byte{[]byte("cc")}},                 // 2 bytes
		{"base/16384/2999", [][]byte{[]byte("zzzz")}},                 // 4 bytes — NOT requested
	})
	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.bytes",
		Verifier:   w.verifier,
		Tables:     []string{"public.x"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.x": {Qualified: "public.x", Path: "base/16384/2619"},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.BytesWritten != 10 {
		t.Errorf("BytesWritten=%d, want 10", res.BytesWritten)
	}
}

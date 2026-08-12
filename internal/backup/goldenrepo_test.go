package backup_test

// Golden-repo compatibility fixture: a miniature repo (plaintext +
// encrypted manifests, chunks, audit chain, shared-DEK object) built
// by a KNOWN release and committed under testdata/. Every future
// version must still open it, verify its signatures, decrypt and
// restore its exact bytes, and verify its audit chain.
//
// Failure-class rationale: format-interpretation drift — the
// verify-sandbox major bug (pg_version parsed as PG_VERSION_NUM) and
// the verify-anchor index-vs-sequence bug were both "current code
// misreads bytes written earlier". Unit tests write and read with the
// SAME code, so they can't see drift; a frozen on-disk artifact can.
//
// Regenerate ONLY intentionally (a deliberate format change):
//
//	PGHS_REGEN_GOLDEN=1 go test -run TestGoldenRepo_Regenerate ./internal/backup/
//
// and commit the diff with the format-change rationale.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey"
)

const (
	goldenTar = "testdata/golden-repo.tar.gz"
	goldenKEK = "testdata/golden-kek.bin"
	goldenSig = "testdata/golden-signing-key.pem"
	goldenPub = "testdata/golden-signing-pub.pem"
)

// Fixed, committed key material — test-only, never used outside this
// fixture. Deterministic so regeneration changes only what the format
// change actually changed.
func goldenKEKBytes() [encryption.KeyLen]byte {
	var k [encryption.KeyLen]byte
	for i := range k {
		k[i] = byte(i*7 + 3)
	}
	return k
}

const goldenPlainBody = "golden plaintext chunk body v1"
const goldenEncBody = "golden encrypted chunk body v1"

func openGoldenWorkdir(t *testing.T) (string, storage.StoragePlugin) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Open(goldenTar)
	if err != nil {
		t.Fatalf("golden fixture missing (%v) — regenerate with PGHS_REGEN_GOLDEN=1", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") {
			t.Fatalf("path traversal in fixture: %s", hdr.Name)
		}
		dst := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := os.Create(dst)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				t.Fatal(err)
			}
			_ = out.Close()
		}
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: dir},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return dir, sp
}

// TestGoldenRepo_StillReadable is the compatibility gate.
func TestGoldenRepo_StillReadable(t *testing.T) {
	if _, err := os.Stat(goldenTar); err != nil {
		t.Fatalf("golden fixture missing: %v — run PGHS_REGEN_GOLDEN=1 go test -run TestGoldenRepo_Regenerate ./internal/backup/ and commit testdata/", err)
	}
	dir, sp := openGoldenWorkdir(t)

	// 1. The repo must OPEN (HSREPO schema + version gate).
	if _, spOpened, err := repo.Open(context.Background(), "file://"+dir); err != nil {
		t.Fatalf("current binary cannot open a golden repo it wrote earlier: %v", err)
	} else {
		_ = spOpened.Close()
	}

	// 2. Manifests must list and VERIFY under the committed pubkey.
	pub, err := os.ReadFile(goldenPub)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	store := backup.NewManifestStore(sp)
	live := map[string]*backup.Manifest{}
	for m, lerr := range store.List(context.Background(), "db1", verifier) {
		if lerr != nil {
			t.Fatalf("golden manifest failed signature/read: %v", lerr)
		}
		if m != nil {
			live[m.BackupID] = m
		}
	}
	if len(live) != 2 {
		t.Fatalf("golden repo lists %d manifests, want 2 (plain + encrypted)", len(live))
	}

	// 3. Plaintext restore round-trip: exact bytes.
	plain := live["db1.full.golden-plain"]
	if plain == nil {
		t.Fatal("golden plain manifest missing")
	}
	cas := casdefault.New(sp)
	body, err := cas.GetChunkBytes(context.Background(), plain.Files[0].Chunks[0].Hash)
	if err != nil {
		t.Fatalf("golden plain chunk unreadable: %v", err)
	}
	if string(body) != goldenPlainBody {
		t.Fatalf("golden plain chunk bytes drifted: %q", body)
	}

	// 4. Encrypted restore round-trip via the committed KEK: unwrap
	//    the manifest DEK, decrypt the chunk, compare exact bytes.
	encM := live["db1.full.golden-encrypted"]
	if encM == nil || encM.Encryption == nil {
		t.Fatal("golden encrypted manifest missing/plaintext")
	}
	kek := goldenKEKBytes()
	wrapped, err := base64.StdEncoding.DecodeString(encM.Encryption.WrappedDEK)
	if err != nil {
		t.Fatal(err)
	}
	dek, err := encryption.Unwrap(kek, wrapped)
	if err != nil {
		t.Fatalf("golden wrapped DEK no longer unwraps — envelope format drift: %v", err)
	}
	aead, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	encCAS := casdefault.NewEncrypted(sp, aead)
	encBody, err := encCAS.GetChunkBytes(context.Background(), encM.Files[0].Chunks[0].Hash)
	if err != nil {
		t.Fatalf("golden encrypted chunk unreadable — chunk envelope drift: %v", err)
	}
	if string(encBody) != goldenEncBody {
		t.Fatalf("golden encrypted chunk bytes drifted: %q", encBody)
	}

	// 5. The shared-DEK object must still resolve to the SAME DEK.
	res, err := sharedkey.ResolveOrMint(context.Background(), sp, "local:default",
		func(w []byte) ([]byte, error) {
			d, e := encryption.Unwrap(kek, w)
			if e != nil {
				return nil, e
			}
			return d[:], nil
		},
		func(d [encryption.KeyLen]byte) ([]byte, error) { return encryption.Wrap(kek, d) }, time.Time{}, "")
	if err != nil || !res.Have {
		t.Fatalf("golden shared-DEK object no longer resolves: have=%v err=%v", res.Have, err)
	}
	if res.DEK != dek {
		t.Fatal("golden shared-DEK object resolves to a DIFFERENT DEK than the manifest wrap")
	}

	// 6. The audit chain must still verify.
	if vres, err := audit.NewStore(sp).VerifyChain(context.Background()); err != nil || !vres.OK {
		t.Fatalf("golden audit chain no longer verifies: res=%+v err=%v", vres, err)
	}
}

// TestGoldenRepo_Regenerate rebuilds the fixture. Opt-in only.
func TestGoldenRepo_Regenerate(t *testing.T) {
	if os.Getenv("PGHS_REGEN_GOLDEN") == "" {
		t.Skip("set PGHS_REGEN_GOLDEN=1 to rebuild the golden fixture (a deliberate format change)")
	}
	dir := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + dir}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dir}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// Deterministic-ish signing keypair: generate fresh, commit both
	// halves next to the tar.
	priv, pub, err := backup.GenerateKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenSig, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenPub, pub, 0o644); err != nil {
		t.Fatal(err)
	}
	kek := goldenKEKBytes()
	if err := os.WriteFile(goldenKEK, kek[:], 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)

	store := backup.NewManifestStore(sp)
	ts := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	mk := func(id string, enc *backup.EncryptionInfo, chunks []backup.ChunkRef, size int64) {
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1", Tenant: "default",
			Type: backup.BackupTypeFull, PGVersion: 17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
			StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Encryption:  enc,
			Files: []backup.FileEntry{{Path: "data/f", Size: size, Mode: 0o600,
				Chunks: chunks}},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	// Plain backup.
	plainCAS := casdefault.New(sp)
	pInfo, err := plainCAS.PutChunk(context.Background(), []byte(goldenPlainBody))
	if err != nil {
		t.Fatal(err)
	}
	mk("db1.full.golden-plain", nil,
		[]backup.ChunkRef{{Hash: pInfo.Hash, Offset: 0, Len: int64(len(goldenPlainBody))}},
		int64(len(goldenPlainBody)))

	// Encrypted backup, DEK from the shared mint under the golden KEK.
	res, err := sharedkey.ResolveOrMint(context.Background(), sp, "local:default",
		func(w []byte) ([]byte, error) {
			d, e := encryption.Unwrap(kek, w)
			if e != nil {
				return nil, e
			}
			return d[:], nil
		},
		func(d [encryption.KeyLen]byte) ([]byte, error) { return encryption.Wrap(kek, d) }, time.Time{}, "")
	if err != nil || !res.Have {
		t.Fatalf("mint: %v", err)
	}
	aead, err := aesgcm.New(res.DEK[:])
	if err != nil {
		t.Fatal(err)
	}
	encCAS := casdefault.NewEncrypted(sp, aead)
	eInfo, err := encCAS.PutChunk(context.Background(), []byte(goldenEncBody))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, res.DEK)
	if err != nil {
		t.Fatal(err)
	}
	mk("db1.full.golden-encrypted", &backup.EncryptionInfo{
		Scheme: "aes-256-gcm", KEKRef: "local:default",
		WrappedDEK: base64.StdEncoding.EncodeToString(wrapped), EnvelopeVersion: 2,
	}, []backup.ChunkRef{{Hash: eInfo.Hash, Offset: 0, Len: int64(len(goldenEncBody))}},
		int64(len(goldenEncBody)))

	// A few audit events so the chain has substance.
	astore := audit.NewStore(sp)
	for _, action := range []string{"backup.committed", "backup.committed", "verify.run"} {
		if err := astore.Append(context.Background(), &audit.Event{
			Action: action, Subject: audit.Subject{Deployment: "db1"}, Timestamp: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Tar it up.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if werr := tw.WriteHeader(&tar.Header{
			Name: rel, Mode: 0o644, Size: int64(len(body)),
			ModTime: ts, Typeflag: tar.TypeReg,
		}); werr != nil {
			return werr
		}
		_, werr := tw.Write(body)
		return werr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenTar, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("golden fixture regenerated: %s (%d bytes)", goldenTar, buf.Len())
}

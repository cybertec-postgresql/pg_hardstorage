package backup_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func streamKeys(t *testing.T) (*backup.Signer, *backup.Verifier) {
	t.Helper()
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	v, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

func signedBytes(t *testing.T, m *backup.Manifest, s *backup.Signer) []byte {
	t.Helper()
	if err := m.Sign(s); err != nil {
		t.Fatal(err)
	}
	b, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collect(t *testing.T, raw []byte, opts backup.StreamOptions) ([]backup.FileEntry, *backup.StreamInfo, error) {
	t.Helper()
	var got []backup.FileEntry
	info, err := backup.StreamManifest(bytes.NewReader(raw), opts, func(fe *backup.FileEntry) error {
		got = append(got, *fe)
		return nil
	})
	return got, info, err
}

func TestStreamManifest_VerifiesWhatCommitWrites(t *testing.T) {
	signer, ver := streamKeys(t)
	shapes := map[string]func(*backup.Manifest){
		"normal": func(m *backup.Manifest) {},
		"html-and-unicode paths": func(m *backup.Manifest) {
			m.Files[0].Path = `base/<a&b>/"q"/ünï/日本`
		},
		"empty files array": func(m *backup.Manifest) { m.Files = []backup.FileEntry{} },
		"null files":        func(m *backup.Manifest) { m.Files = nil },
		"fields after files": func(m *backup.Manifest) {
			m.WALRequired = []string{"000000010000000000000003"}
			m.Dirs = []backup.DirEntry{{Path: "pg_wal", Mode: 0o700}}
		},
	}
	for name, mut := range shapes {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			mut(m)
			raw := signedBytes(t, m, signer)
			want, err := backup.ParseAndVerify(raw, ver)
			if err != nil {
				t.Fatalf("reference ParseAndVerify: %v", err)
			}
			got, info, err := collect(t, raw, backup.StreamOptions{Verifier: ver})
			if err != nil {
				t.Fatalf("StreamManifest: %v", err)
			}
			if !info.Verified {
				t.Error("not marked verified")
			}
			if len(want.Files) != len(got) || (len(got) > 0 && !reflect.DeepEqual(want.Files, got)) {
				t.Errorf("entries differ from ParseAndVerify:\n got %+v\nwant %+v", got, want.Files)
			}
			if info.Manifest.BackupID != want.BackupID || info.Manifest.Files != nil {
				t.Errorf("header wrong: %+v", info.Manifest)
			}
			if info.Files != int64(len(want.Files)) {
				t.Errorf("Files = %d, want %d", info.Files, len(want.Files))
			}
		})
	}
}

// A signed manifest whose top-level keys are in a different order still
// verifies (it parses to the same struct): the rebuild path.
func TestStreamManifest_NonCanonicalFieldOrder(t *testing.T) {
	signer, ver := streamKeys(t)
	raw := signedBytes(t, validManifest(), signer)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	// files first, everything else after.
	var b bytes.Buffer
	b.WriteString(`{"files":`)
	b.Write(top["files"])
	for k, v := range top {
		if k == "files" {
			continue
		}
		fmt.Fprintf(&b, `,%q:`, k)
		b.Write(v)
	}
	b.WriteString("}")
	if _, err := backup.ParseAndVerify(b.Bytes(), ver); err != nil {
		t.Fatalf("reference should accept reordered document: %v", err)
	}
	if _, _, err := collect(t, b.Bytes(), backup.StreamOptions{Verifier: ver}); err != nil {
		t.Fatalf("StreamManifest on reordered document: %v", err)
	}
}

func TestStreamManifest_RejectsTamperingWrongKeyAndUnsigned(t *testing.T) {
	signer, ver := streamKeys(t)
	raw := signedBytes(t, validManifest(), signer)

	tampered := bytes.Replace(raw, []byte(`"size":100`), []byte(`"size":101`), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("test bug: tamper did nothing")
	}
	if _, _, err := collect(t, tampered, backup.StreamOptions{Verifier: ver}); err == nil {
		t.Error("tampered file size was accepted")
	}
	tamperedHdr := bytes.Replace(raw, []byte(`"pg_version":17`), []byte(`"pg_version":18`), 1)
	if _, _, err := collect(t, tamperedHdr, backup.StreamOptions{Verifier: ver}); err == nil {
		t.Error("tampered header was accepted")
	}

	_, other := streamKeys(t)
	if _, _, err := collect(t, raw, backup.StreamOptions{Verifier: other}); !errors.Is(err, backup.ErrPublicKeyMismatch) {
		t.Errorf("wrong key: err = %v, want ErrPublicKeyMismatch", err)
	}
	unsigned, _ := validManifest().MarshalToBytes()
	if _, _, err := collect(t, unsigned, backup.StreamOptions{Verifier: ver}); !errors.Is(err, backup.ErrUnsigned) {
		t.Errorf("unsigned: err = %v, want ErrUnsigned", err)
	}
	// Attestationless read of the same bytes works.
	if _, _, err := collect(t, unsigned, backup.StreamOptions{}); err != nil {
		t.Errorf("attestationless: %v", err)
	}
	// Embedded-key verification accepts the genuine one, rejects tamper.
	if _, _, err := collect(t, raw, backup.StreamOptions{VerifyEmbedded: true}); err != nil {
		t.Errorf("embedded: %v", err)
	}
	if _, _, err := collect(t, tampered, backup.StreamOptions{VerifyEmbedded: true}); err == nil {
		t.Error("embedded accepted tampered content")
	}
}

func TestStreamManifest_RejectsAmbiguousDocuments(t *testing.T) {
	good, _ := validManifest().MarshalToBytes()
	cases := map[string][]byte{
		"duplicate top-level key": bytes.Replace(good, []byte(`"deployment":"db1"`), []byte(`"deployment":"db1","deployment":"evil"`), 1),
		"case-variant key":        bytes.Replace(good, []byte(`"deployment"`), []byte(`"Deployment"`), 1),
		"duplicate key in entry":  bytes.Replace(good, []byte(`"path":"PG_VERSION"`), []byte(`"path":"PG_VERSION","path":"x"`), 1),
	}
	for name, raw := range cases {
		if bytes.Equal(raw, good) {
			t.Fatalf("%s: test bug, no change", name)
		}
		if _, _, err := collect(t, raw, backup.StreamOptions{}); !errors.Is(err, backup.ErrAmbiguousManifest) {
			t.Errorf("%s: err = %v, want ErrAmbiguousManifest", name, err)
		}
	}
}

// Validate mode must agree with Manifest.Validate on every invariant.
func TestStreamManifest_ValidateMatchesManifestValidate(t *testing.T) {
	h := repo.Hash{9}
	mutations := map[string]func(*backup.Manifest){
		"valid":            func(m *backup.Manifest) {},
		"duplicate path":   func(m *backup.Manifest) { m.Files = append(m.Files, m.Files[0]) },
		"escaping path":    func(m *backup.Manifest) { m.Files[0].Path = "../etc/passwd" },
		"chunk sum":        func(m *backup.Manifest) { m.Files[0].Size = 99 },
		"zero hash":        func(m *backup.Manifest) { m.Files[0].Chunks[0].Hash = repo.Hash{} },
		"non-contiguous":   func(m *backup.Manifest) { m.Files[2].Chunks[1].Offset = 61 },
		"empty path":       func(m *backup.Manifest) { m.Files[0].Path = "" },
		"dir/file collide": func(m *backup.Manifest) { m.Dirs = append(m.Dirs, backup.DirEntry{Path: "PG_VERSION"}) },
		"dup dir":          func(m *backup.Manifest) { m.Dirs = append(m.Dirs, m.Dirs[0]) },
		"empty label":      func(m *backup.Manifest) { m.BackupLabel = "" },
		"bad encryption":   func(m *backup.Manifest) { m.Encryption = &backup.EncryptionInfo{Scheme: "x"} },
		"same path in 2 ts": func(m *backup.Manifest) {
			f := m.Files[0]
			f.TablespaceOID = 16400
			f.Chunks = []backup.ChunkRef{{Hash: h, Len: 3}}
			m.Files = append(m.Files, f)
		},
	}
	for name, mut := range mutations {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			mut(m)
			raw, err := m.MarshalToBytes()
			if err != nil {
				t.Fatal(err)
			}
			wantErr := m.Validate()
			_, _, gotErr := collect(t, raw, backup.StreamOptions{Validate: true})
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("Validate() = %v but streaming Validate = %v", wantErr, gotErr)
			}
		})
	}
}

func TestStreamManifest_ConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a ~100 MB document")
	}
	signer, ver := streamKeys(t)
	const n = 250_000
	h := repo.Hash{7}
	// Sign the real thing: build via the struct once, sign, then discard it.
	full := validManifest()
	full.Files = make([]backup.FileEntry, n)
	for i := range full.Files {
		full.Files[i] = backup.FileEntry{Path: fmt.Sprintf("base/1/%08d", i), Size: 10,
			Chunks: []backup.ChunkRef{{Hash: h, Offset: 0, Len: 10}}}
	}
	raw := signedBytes(t, full, signer)
	full = nil
	runtime.GC()

	var base, peak uint64
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base = ms.HeapAlloc
	count := 0
	info, err := backup.StreamManifest(bytes.NewReader(raw), backup.StreamOptions{Verifier: ver, Validate: true, PathSetMemEntries: 20_000}, func(*backup.FileEntry) error {
		count++
		if count%50000 == 0 {
			runtime.GC()
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != n || !info.Verified {
		t.Fatalf("count=%d verified=%v", count, info.Verified)
	}
	grew := int64(peak) - int64(base)
	t.Logf("document %d MB, heap growth while streaming+verifying+validating: %d MB", len(raw)>>20, grew>>20)
	if grew > int64(len(raw))/2 {
		t.Errorf("heap grew %d MB streaming a %d MB document: not constant-memory", grew>>20, len(raw)>>20)
	}
}

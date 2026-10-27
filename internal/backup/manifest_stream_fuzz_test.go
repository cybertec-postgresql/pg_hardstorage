package backup_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// Differential: for ANY input, the streaming reader and the original
// parser must agree on accept/reject, on the header, and on the entries.
// A disagreement is a manifest one reader trusts and the other doesn't.
func FuzzStreamManifest_AgreesWithParseAttestationless(f *testing.F) {
	good, _ := validManifest().MarshalToBytes()
	f.Add(good)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema":"pg_hardstorage.manifest.v1","files":null}`))
	f.Add([]byte(`{"schema":"pg_hardstorage.manifest.v1","files":[{"path":"a","chunks":null}]}`))
	f.Add(bytes.Replace(good, []byte(`"deployment"`), []byte(`"Deployment"`), 1))
	f.Add(bytes.Replace(good, []byte(`"size":3`), []byte(`"size":-3`), 1))
	f.Fuzz(func(t *testing.T, raw []byte) {
		want, werr := backup.ParseAttestationless(raw)
		var got []backup.FileEntry
		info, gerr := backup.StreamManifest(bytes.NewReader(raw), backup.StreamOptions{}, func(fe *backup.FileEntry) error {
			got = append(got, *fe)
			return nil
		})
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("accept/reject disagree: ParseAttestationless=%v StreamManifest=%v\ninput: %q", werr, gerr, raw)
		}
		if werr != nil {
			return
		}
		if len(want.Files) != len(got) || (len(got) > 0 && !reflect.DeepEqual(want.Files, got)) {
			t.Fatalf("entries differ\ninput: %q", raw)
		}
		h := *want
		h.Files = nil
		hh := *info.Manifest
		a, _ := json.Marshal(h)
		b, _ := json.Marshal(hh)
		if !bytes.Equal(a, b) {
			t.Fatalf("headers differ:\n%s\n%s\ninput: %q", a, b, raw)
		}
	})
}

// A signed manifest with any single byte flipped must never verify under
// the streaming verifier unless the original verifier also accepts it.
func FuzzStreamManifest_VerifyAgreesWithParseAndVerify(f *testing.F) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	ver, _ := backup.LoadVerifier(pub)
	m := validManifest()
	if err := m.Sign(signer); err != nil {
		f.Fatal(err)
	}
	good, _ := m.MarshalToBytes()
	f.Add(0, byte(0))
	f.Add(len(good)/2, byte('x'))
	f.Fuzz(func(t *testing.T, pos int, b byte) {
		raw := append([]byte(nil), good...)
		if pos < 0 {
			pos = -pos
		}
		raw[pos%len(raw)] = b
		_, werr := backup.ParseAndVerify(raw, ver)
		_, gerr := backup.StreamManifest(bytes.NewReader(raw), backup.StreamOptions{Verifier: ver}, nil)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("verify disagrees at byte %d=%q: ParseAndVerify=%v Stream=%v", pos%len(raw), b, werr, gerr)
		}
	})
}

// Truncation at every byte is rejected, never accepted or panicking.
func TestStreamManifest_EveryTruncationRejected(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	ver, _ := backup.LoadVerifier(pub)
	m := validManifest()
	m.Files = manyFiles(30)
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, _ := m.MarshalToBytes()
	for i := 0; i < len(raw); i++ {
		if _, err := backup.StreamManifest(bytes.NewReader(raw[:i]), backup.StreamOptions{Verifier: ver}, nil); err == nil {
			t.Fatalf("truncation at %d/%d accepted", i, len(raw))
		}
	}
}

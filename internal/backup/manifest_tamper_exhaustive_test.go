package backup_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

func signedSampleTamper(t testing.TB) ([]byte, *backup.Verifier, []byte) {
	t.Helper()
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	m := sampleManifest()
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := m.Canonicalize()
	return raw, verifier, canon
}

// Every single-byte mutation, and every truncation / extension, of a
// signed manifest must either fail verification or parse to a manifest
// whose canonical (signed) form is identical to the original -- i.e. no
// mutation can change any signed field and still verify.
func TestParseAndVerify_ExhaustiveByteTamper(t *testing.T) {
	raw, verifier, canon := signedSampleTamper(t)
	check := func(label string, mut []byte) {
		m, err := backup.ParseAndVerify(mut, verifier)
		if err != nil {
			return
		}
		got, cerr := m.Canonicalize()
		if cerr != nil || !bytes.Equal(got, canon) {
			t.Fatalf("%s: tampered manifest VERIFIED with different signed content", label)
		}
	}
	for i := range raw {
		for _, x := range []byte{0x01, 0x20, 0xff} {
			mut := append([]byte(nil), raw...)
			mut[i] ^= x
			check("flip", mut)
		}
	}
	for n := 0; n < len(raw); n++ {
		check("truncate", raw[:n])
	}
	check("append", append(append([]byte(nil), raw...), []byte(`,"x":1`)...))
}

func FuzzParseAndVerify_NoForgery(f *testing.F) {
	raw, verifier, canon := signedSampleTamper(f)
	f.Add(raw)
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := backup.ParseAndVerify(data, verifier)
		if err != nil {
			return
		}
		got, cerr := m.Canonicalize()
		if cerr != nil || !bytes.Equal(got, canon) {
			t.Fatalf("forged manifest verified")
		}
	})
}

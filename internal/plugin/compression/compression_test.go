package compression_test

import (
	"bytes"
	"crypto/sha256"
	mathrand "math/rand"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/none"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
)

func TestEnvelope_RoundTrip_V2(t *testing.T) {
	cases := []struct {
		algo    compression.AlgorithmID
		enc     compression.EncryptionFields
		payload []byte
	}{
		{compression.AlgoNone, compression.EncryptionFields{}, []byte{}},
		{compression.AlgoNone, compression.EncryptionFields{}, []byte("hello")},
		{compression.AlgoZstd, compression.EncryptionFields{}, []byte{0x1, 0x2, 0x3}},
		// Encrypted: nonzero EncryptionAlgo + nonce.
		{
			compression.AlgoZstd,
			compression.EncryptionFields{EncryptionAlgo: 1, Nonce: [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
			[]byte("ciphertext+tag"),
		},
	}
	for _, c := range cases {
		env := compression.WriteEnvelope(c.algo, c.enc, c.payload)
		if len(env) != 15+len(c.payload) {
			t.Errorf("envelope len = %d, want %d (15 byte header + %d payload)",
				len(env), 15+len(c.payload), len(c.payload))
		}
		algo, gotEnc, payload, err := compression.ReadEnvelope(env)
		if err != nil {
			t.Errorf("ReadEnvelope: %v", err)
			continue
		}
		if algo != c.algo {
			t.Errorf("compression algo = %d, want %d", algo, c.algo)
		}
		if gotEnc != c.enc {
			t.Errorf("encryption fields = %+v, want %+v", gotEnc, c.enc)
		}
		if !bytes.Equal(payload, c.payload) {
			t.Errorf("payload mismatch")
		}
	}
}

func TestEnvelope_BackCompat_V1(t *testing.T) {
	// Legacy v0x01 envelopes (no encryption byte / no nonce) must still
	// round-trip cleanly and report EncryptionAlgo=0.
	body := []byte("some legacy bytes")
	env := compression.WriteEnvelopeV1(compression.AlgoZstd, body)
	if len(env) != 2+len(body) {
		t.Errorf("v1 envelope len = %d, want %d", len(env), 2+len(body))
	}
	algo, encFields, payload, err := compression.ReadEnvelope(env)
	if err != nil {
		t.Fatalf("v1 ReadEnvelope: %v", err)
	}
	if algo != compression.AlgoZstd {
		t.Errorf("v1 compression algo = %d", algo)
	}
	if encFields.IsEncrypted() {
		t.Errorf("v1 envelope should report unencrypted; got %+v", encFields)
	}
	if !bytes.Equal(payload, body) {
		t.Error("v1 payload round-trip mismatch")
	}
}

func TestReadEnvelope_RejectsUnknownVersion(t *testing.T) {
	bad := []byte{0xAB, 0x01, 'd', 'a', 't', 'a'}
	_, _, _, err := compression.ReadEnvelope(bad)
	if err == nil {
		t.Fatal("expected ErrCorruptEnvelope")
	}
	if !strings.Contains(err.Error(), "version byte") {
		t.Errorf("error should name the version byte; got %v", err)
	}
}

func TestReadEnvelope_RejectsShortInput(t *testing.T) {
	_, _, _, err := compression.ReadEnvelope([]byte{})
	if err == nil {
		t.Fatal("expected error on empty input")
	}
	// v1 needs >= 2 bytes
	_, _, _, err = compression.ReadEnvelope([]byte{0x01})
	if err == nil {
		t.Fatal("expected error on v1 single-byte input")
	}
	// v2 needs >= 15 bytes
	_, _, _, err = compression.ReadEnvelope([]byte{0x02, 0x00})
	if err == nil {
		t.Fatal("expected error on v2 short input")
	}
}

func TestNone_Identity(t *testing.T) {
	c := none.Compressor{}
	body := []byte("the quick brown fox")
	out, algo, err := c.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if algo != compression.AlgoNone {
		t.Errorf("algo = %d, want AlgoNone", algo)
	}
	if !bytes.Equal(out, body) {
		t.Error("none should round-trip plaintext verbatim")
	}
	back, err := c.Decompress(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, body) {
		t.Error("none.Decompress should round-trip")
	}
}

func TestNone_DecompressIsACopy(t *testing.T) {
	// The compressor MUST return a fresh slice (so callers can mutate
	// the input afterwards without corrupting the stored chunk).
	c := none.Compressor{}
	body := []byte("dont mutate me")
	out, _, _ := c.Compress(body)
	body[0] = 'X'
	if out[0] != 'd' {
		t.Errorf("Compress output aliases input; got out[0]=%c", out[0])
	}
}

func TestZstd_RoundTrip_RandomPayload(t *testing.T) {
	c := zstd.NewDefault()
	r := mathrand.New(mathrand.NewSource(0xBEEF))
	for _, sz := range []int{0, 1, 63, 64, 1024, 64 * 1024, 1024 * 1024} {
		body := make([]byte, sz)
		r.Read(body)
		payload, algo, err := c.Compress(body)
		if err != nil {
			t.Fatalf("size %d: Compress: %v", sz, err)
		}
		// Empty / very small inputs short-circuit to AlgoNone.
		if sz < 64 && algo != compression.AlgoNone {
			t.Errorf("size %d: short-circuit broken; algo=%d", sz, algo)
		}
		// Decompress through the right codec for the algo we got back.
		var got []byte
		switch algo {
		case compression.AlgoNone:
			got, err = (none.Compressor{}).Decompress(payload)
		case compression.AlgoZstd:
			got, err = c.Decompress(payload)
		}
		if err != nil {
			t.Fatalf("size %d: Decompress: %v", sz, err)
		}
		if sha256.Sum256(got) != sha256.Sum256(body) {
			t.Errorf("size %d: round-trip differs", sz)
		}
	}
}

func TestZstd_CompressesRedundantPayload(t *testing.T) {
	// Highly redundant input should compress significantly.
	body := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 10000)
	c := zstd.NewDefault()
	payload, algo, err := c.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if algo != compression.AlgoZstd {
		t.Fatalf("algo = %d; want zstd for a ~450 KiB input", algo)
	}
	if float64(len(payload))/float64(len(body)) > 0.05 {
		t.Errorf("redundant input compressed only to %d / %d (%.2fx); expected better",
			len(payload), len(body), float64(len(payload))/float64(len(body)))
	}
}

func TestRegistry_LookupAndHas(t *testing.T) {
	r := compression.NewRegistry()
	r.Register(compression.AlgoNone, none.Compressor{})
	if !r.Has(compression.AlgoNone) {
		t.Error("Has should be true after Register")
	}
	if r.Has(compression.AlgoZstd) {
		t.Error("Has should be false for unregistered algo")
	}
	c, err := r.Lookup(compression.AlgoNone)
	if err != nil || c == nil {
		t.Errorf("Lookup AlgoNone: c=%v err=%v", c, err)
	}
	_, err = r.Lookup(compression.AlgoZstd)
	if err == nil {
		t.Error("Lookup of unregistered algo should error")
	}
}

func TestRegistry_DoubleRegisterPanics(t *testing.T) {
	r := compression.NewRegistry()
	r.Register(compression.AlgoNone, none.Compressor{})
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("expected panic on double-register")
		}
	}()
	r.Register(compression.AlgoNone, none.Compressor{})
}

func TestAlgorithmID_String(t *testing.T) {
	cases := []struct {
		a    compression.AlgorithmID
		want string
	}{
		{compression.AlgoNone, "none"},
		{compression.AlgoZstd, "zstd"},
		{compression.AlgorithmID(99), "unknown-algo-99"},
	}
	for _, c := range cases {
		if got := c.a.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.a, got, c.want)
		}
	}
}

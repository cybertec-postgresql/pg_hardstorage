package zstd_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	upstreamzstd "github.com/klauspost/compress/zstd"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
)

func TestCompressor_RoundTrip(t *testing.T) {
	c := zstd.NewDefault()
	body := bytes.Repeat([]byte("compressible body "), 1024)
	payload, algo, err := c.Compress(body)
	if err != nil {
		t.Fatal(err)
	}
	if algo == 0 {
		t.Errorf("expected non-zero algo for >=64 byte input; got %d", algo)
	}
	got, err := c.Decompress(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("round-trip differs")
	}
}

func TestCompressor_BombGuard_RejectsOversizedDecode(t *testing.T) {
	// Build a payload that legitimately decompresses to N bytes;
	// configure the compressor with a much smaller MaxDecodedSize.
	// Decompress must refuse rather than allocating the whole thing.

	// Use upstream zstd to build a payload of known plaintext size
	// outside of our wrapper (so the wrapper's bomb guard is the only
	// thing protecting us).
	enc, err := upstreamzstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("A"), 10*1024) // 10 KiB
	payload := enc.EncodeAll(plaintext, nil)
	_ = enc.Close()

	c := &zstd.Compressor{
		Level:          upstreamzstd.SpeedDefault,
		MaxDecodedSize: 1024, // 1 KiB — well under our 10 KiB plaintext
	}
	_, err = c.Decompress(payload)
	if err == nil {
		t.Fatal("expected bomb-guard error; Decompress succeeded")
	}
	// klauspost/compress error messages include "max" or
	// "memory"; be tolerant about exact wording, just verify the
	// guard fired.
	if !strings.Contains(strings.ToLower(err.Error()), "memory") &&
		!strings.Contains(strings.ToLower(err.Error()), "exceed") &&
		!strings.Contains(strings.ToLower(err.Error()), "size") {
		t.Errorf("expected memory/size limit error; got %v", err)
	}
}

func TestCompressor_BombGuard_AcceptsLegitimatePayload(t *testing.T) {
	// Default 256 MiB is plenty for any FastCDC-shaped chunk.
	c := zstd.NewDefault()
	body := bytes.Repeat([]byte("A"), 256*1024) // 256 KiB
	payload, _, _ := c.Compress(body)
	got, err := c.Decompress(payload)
	if err != nil {
		t.Fatalf("default guard wrongly rejected legitimate payload: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("legitimate payload round-trip differs")
	}
}

func TestCompressor_DecompressGarbage(t *testing.T) {
	c := zstd.NewDefault()
	_, err := c.Decompress([]byte("not zstd"))
	if err == nil {
		t.Error("expected error on non-zstd input")
	}
	// Underlying error path varies; just ensure it's surfaced.
	_ = errors.Unwrap
}

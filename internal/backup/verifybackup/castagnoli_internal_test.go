package verifybackup

// castagnoliHash adapts CRC32C to hash.Hash so the verifier can treat
// every algorithm PG's backup_manifest names uniformly. Its byte order
// has already been wrong once: an earlier version emitted big-endian,
// producing "f1c7ab58" where the manifest said "58abc7f1", and every
// CRC32C-checksummed file failed verification. The source comment
// records the fix and adds the warning that matters —
//
//	the test helper must match
//
// — because a test that computes its expectation through the same
// code agrees with any byte order and proves nothing. So the digests
// here come from the CRC-32/ISCSI standard check value, not from
// castagnoliHash.
//
// Reset, Size and BlockSize were unwitnessed. Size and BlockSize are
// what hash.Hash callers size their buffers from, and Reset is what
// makes a hasher reusable across files: a Reset that did not clear
// state would fold file N-1's bytes into file N's digest, failing
// every file after the first with a mismatch pointing at the wrong
// file.

import (
	"bytes"
	"context"
	"encoding/hex"
	"hash"
	"os"
	"path/filepath"
	"testing"
)

// The CRC-32/ISCSI (Castagnoli) check value: the CRC of the ASCII
// string "123456789" is 0xE3069283. This is the standard's own
// self-test vector, independent of any code in this repository.
const (
	crcCheckInput = "123456789"
	crcCheckValue = uint32(0xE3069283)
)

// PG serialises the digest little-endian, so the low byte comes first.
var crcCheckLittleEndian = []byte{0x83, 0x92, 0x06, 0xE3}

func newCRC32C(t *testing.T) hash.Hash {
	t.Helper()
	h, err := newHasher("CRC32C")
	if err != nil {
		t.Fatalf("newHasher(CRC32C): %v", err)
	}
	return h
}

func TestCastagnoliHash_SumIsLittleEndianAgainstTheStandardCheckValue(t *testing.T) {
	h := newCRC32C(t)
	if _, err := h.Write([]byte(crcCheckInput)); err != nil {
		t.Fatal(err)
	}
	got := h.Sum(nil)

	if !bytes.Equal(got, crcCheckLittleEndian) {
		t.Fatalf("Sum = %x, want %x — CRC-32/ISCSI check value %#08x serialised the way PG's "+
			"backup_manifest does. Big-endian here (%x) is the regression that failed every "+
			"CRC32C-checksummed file in the backup.",
			got, crcCheckLittleEndian, crcCheckValue,
			[]byte{0xE3, 0x06, 0x92, 0x83})
	}
	// The manifest carries hex, which is what actually gets compared.
	if got, want := hex.EncodeToString(got), "839206e3"; got != want {
		t.Errorf("hex digest = %q, want %q", got, want)
	}
}

// Sum must append to its argument and must not disturb the running
// state — hash.Hash's documented contract, and the verifier relies on
// it when it digests and then keeps reading.
func TestCastagnoliHash_SumAppendsAndDoesNotConsumeState(t *testing.T) {
	h := newCRC32C(t)
	if _, err := h.Write([]byte(crcCheckInput)); err != nil {
		t.Fatal(err)
	}
	prefix := []byte{0xAA, 0xBB}
	got := h.Sum(prefix)
	if !bytes.Equal(got[:2], prefix) {
		t.Errorf("Sum did not append to its argument: %x", got)
	}
	if !bytes.Equal(got[2:], crcCheckLittleEndian) {
		t.Errorf("appended digest = %x, want %x", got[2:], crcCheckLittleEndian)
	}
	if second := h.Sum(nil); !bytes.Equal(second, crcCheckLittleEndian) {
		t.Errorf("Sum changed the running state: second call gave %x, want %x",
			second, crcCheckLittleEndian)
	}
}

// The reuse contract. The verifier walks many files with one hasher.
func TestCastagnoliHash_ResetClearsState(t *testing.T) {
	h := newCRC32C(t)
	if _, err := h.Write([]byte("bytes from an earlier file")); err != nil {
		t.Fatal(err)
	}
	h.Reset()
	if _, err := h.Write([]byte(crcCheckInput)); err != nil {
		t.Fatal(err)
	}
	if got := h.Sum(nil); !bytes.Equal(got, crcCheckLittleEndian) {
		t.Fatalf("Sum after Reset = %x, want %x — a Reset that leaves state behind folds the "+
			"previous file's bytes into this file's digest, so every file after the first "+
			"fails verification and the mismatch names the wrong file", got, crcCheckLittleEndian)
	}
}

// Streaming must equal one-shot: the verifier feeds files in chunks.
func TestCastagnoliHash_ChunkedWritesMatchOneShot(t *testing.T) {
	for _, split := range []int{1, 2, 4, 8} {
		h := newCRC32C(t)
		for i := 0; i < len(crcCheckInput); i += split {
			end := i + split
			if end > len(crcCheckInput) {
				end = len(crcCheckInput)
			}
			if _, err := h.Write([]byte(crcCheckInput[i:end])); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.Sum(nil); !bytes.Equal(got, crcCheckLittleEndian) {
			t.Errorf("split=%d: Sum = %x, want %x", split, got, crcCheckLittleEndian)
		}
	}
}

func TestCastagnoliHash_SizeAndBlockSize(t *testing.T) {
	h := newCRC32C(t)
	if got := h.Size(); got != 4 {
		t.Errorf("Size() = %d, want 4 (a CRC32C digest is four bytes)", got)
	}
	if got := h.BlockSize(); got != 1 {
		t.Errorf("BlockSize() = %d, want 1 (CRC32C has no preferred block boundary)", got)
	}
	// Size must actually describe Sum's output, not be an independent
	// constant that drifted from it.
	if got := len(h.Sum(nil)); got != h.Size() {
		t.Errorf("Sum produced %d bytes but Size() says %d", got, h.Size())
	}
}

// The digest widths of every algorithm PG documents, so a mis-wired
// case in newHasher is caught rather than silently verifying against
// the wrong hash.
func TestNewHasher_DigestWidths(t *testing.T) {
	for _, tc := range []struct {
		alg  string
		size int
	}{
		{"CRC32C", 4}, {"SHA224", 28}, {"SHA256", 32}, {"SHA384", 48}, {"SHA512", 64},
	} {
		h, err := newHasher(tc.alg)
		if err != nil {
			t.Errorf("newHasher(%s): %v", tc.alg, err)
			continue
		}
		if got := h.Size(); got != tc.size {
			t.Errorf("%s digest is %d bytes, want %d", tc.alg, got, tc.size)
		}
	}
	if _, err := newHasher("MD5"); err == nil {
		t.Error("an algorithm PG does not document must hard-fail, not verify as opaque-ok")
	}
}

// Result.Algorithm is JSON output and part of a restore's recorded
// evidence. A backup whose files carry mixed checksum algorithms —
// rare, but possible when pg_basebackup's --manifest-checksums changes
// mid-run — rendered its algorithm set in map order, so the same
// verification produced "mixed:CRC32C,SHA256" on one run and
// "mixed:SHA256,CRC32C" on the next. Evidence that cannot be diffed
// against itself is not evidence.
func TestVerify_MixedAlgorithmStringIsStable(t *testing.T) {
	manifest := []byte(`{
	  "PostgreSQL-Backup-Manifest-Version": 1,
	  "Files": [
	    {"Path": "a", "Size": 0, "Checksum-Algorithm": "SHA256",
	     "Checksum": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	    {"Path": "b", "Size": 0, "Checksum-Algorithm": "CRC32C", "Checksum": "00000000"},
	    {"Path": "c", "Size": 0, "Checksum-Algorithm": "SHA512",
	     "Checksum": "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"}
	  ]
	}`)
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first := ""
	for i := 0; i < 100; i++ {
		res, err := Verify(context.Background(), manifest, dir)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if i == 0 {
			first = res.Algorithm
			continue
		}
		if res.Algorithm != first {
			t.Fatalf("run %d reported %q, the first run reported %q — the mixed-algorithm "+
				"string depends on map iteration order, so the same verification cannot be "+
				"diffed against itself", i, res.Algorithm, first)
		}
	}
	if want := "mixed:CRC32C,SHA256,SHA512"; first != want {
		t.Errorf("Algorithm = %q, want %q (sorted)", first, want)
	}
}

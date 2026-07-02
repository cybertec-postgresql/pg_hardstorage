package verifybackup_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/verifybackup"
)

// writeFiles materialises a name→body map under root and
// returns root.  Used to set up a fake "restored datadir"
// the tests can verify against.
func writeFiles(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// crc32cHex matches PG's serialisation: little-endian
// 4-byte hex.  Stays in lockstep with verifybackup.go's
// castagnoliHash.Sum.
func crc32cHex(b []byte) string {
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	h.Write(b)
	v := h.Sum32()
	return hex.EncodeToString([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestVerify_CRC32C_LittleEndianMatchesPG locks in the
// byte-order convention against a well-known reference
// vector ("123456789" → 0xe3069283).  PG's backup_manifest
// emits CRC32C as little-endian hex; if a future change
// re-introduces big-endian by mistake, this test fails
// loudly and points at the regression history in
// verifybackup.go's castagnoliHash docstring.
func TestVerify_CRC32C_LittleEndianMatchesPG(t *testing.T) {
	body := []byte("123456789")
	root := writeFiles(t, map[string][]byte{"x": body})
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "x", "Size": 9,
			 "Checksum-Algorithm": "CRC32C", "Checksum": "839206e3"}
		]
	}`)
	if _, err := verifybackup.Verify(context.Background(), manifest, root); err != nil {
		t.Fatalf("expected pass with little-endian hex, got %v", err)
	}
	// And the inverse: big-endian-format hex must FAIL,
	// proving we're not silently accepting both.
	manifestBE := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "x", "Size": 9,
			 "Checksum-Algorithm": "CRC32C", "Checksum": "e3069283"}
		]
	}`)
	if _, err := verifybackup.Verify(context.Background(), manifestBE, root); err == nil {
		t.Error("expected mismatch on big-endian hex (we accept only PG's little-endian)")
	}
}

func TestVerify_Happy_CRC32C(t *testing.T) {
	body := []byte("hello world\n")
	root := writeFiles(t, map[string][]byte{"PG_VERSION": body})
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "PG_VERSION", "Size": ` + itoa(len(body)) + `,
			 "Last-Modified": "2026-05-06 12:00:00 GMT",
			 "Checksum-Algorithm": "CRC32C", "Checksum": "` + crc32cHex(body) + `"}
		]
	}`)
	res, err := verifybackup.Verify(context.Background(), manifest, root)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.FilesChecked != 1 {
		t.Errorf("FilesChecked = %d, want 1", res.FilesChecked)
	}
	if res.Algorithm != "CRC32C" {
		t.Errorf("Algorithm = %q, want CRC32C", res.Algorithm)
	}
}

func TestVerify_Happy_SHA256(t *testing.T) {
	body := []byte("a different file\n")
	root := writeFiles(t, map[string][]byte{"global/pg_control": body})
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "global/pg_control", "Size": ` + itoa(len(body)) + `,
			 "Last-Modified": "2026-05-06 12:00:00 GMT",
			 "Checksum-Algorithm": "SHA256", "Checksum": "` + sha256Hex(body) + `"}
		]
	}`)
	if _, err := verifybackup.Verify(context.Background(), manifest, root); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerify_DetectsMissingFile(t *testing.T) {
	root := writeFiles(t, map[string][]byte{}) // nothing on disk
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "PG_VERSION", "Size": 3, "Checksum-Algorithm": "NONE", "Checksum": ""}
		]
	}`)
	_, err := verifybackup.Verify(context.Background(), manifest, root)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q does not mention missing", err.Error())
	}
}

func TestVerify_DetectsSizeMismatch(t *testing.T) {
	body := []byte("hello")
	root := writeFiles(t, map[string][]byte{"PG_VERSION": body})
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "PG_VERSION", "Size": 99, "Checksum-Algorithm": "NONE", "Checksum": ""}
		]
	}`)
	_, err := verifybackup.Verify(context.Background(), manifest, root)
	if err == nil {
		t.Fatal("expected size mismatch error")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("error %q does not mention size mismatch", err.Error())
	}
}

func TestVerify_DetectsContentCorruption(t *testing.T) {
	original := []byte("clean content\n")
	corrupted := []byte("BAD content!!\n") // same length
	if len(original) != len(corrupted) {
		t.Fatal("test data must have equal lengths")
	}
	root := writeFiles(t, map[string][]byte{"data": corrupted})
	// Manifest checksum is for the ORIGINAL — corruption
	// preserves size but breaks the checksum.
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "data", "Size": ` + itoa(len(original)) + `,
			 "Checksum-Algorithm": "CRC32C", "Checksum": "` + crc32cHex(original) + `"}
		]
	}`)
	_, err := verifybackup.Verify(context.Background(), manifest, root)
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error %q does not mention checksum mismatch", err.Error())
	}
}

func TestVerify_NoneAlgorithm_OnlyChecksSizeAndPresence(t *testing.T) {
	body := []byte("anything goes\n")
	root := writeFiles(t, map[string][]byte{"x": body})
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "x", "Size": ` + itoa(len(body)) + `,
			 "Checksum-Algorithm": "NONE", "Checksum": ""}
		]
	}`)
	res, err := verifybackup.Verify(context.Background(), manifest, root)
	if err != nil {
		t.Fatalf("NONE algorithm should pass when size matches: %v", err)
	}
	if res.FilesChecked != 1 {
		t.Errorf("FilesChecked = %d", res.FilesChecked)
	}
}

func TestVerify_EmptyManifest_ReturnsErrNoManifest(t *testing.T) {
	_, err := verifybackup.Verify(context.Background(), nil, t.TempDir())
	if !errors.Is(err, verifybackup.ErrNoManifest) {
		t.Errorf("expected ErrNoManifest, got %v", err)
	}
}

func TestVerify_RejectsUnsupportedAlgorithm(t *testing.T) {
	body := []byte("xx")
	root := writeFiles(t, map[string][]byte{"x": body})
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Path": "x", "Size": 2, "Checksum-Algorithm": "MD5", "Checksum": "00"}
		]
	}`)
	_, err := verifybackup.Verify(context.Background(), manifest, root)
	if err == nil {
		t.Fatal("expected error on unsupported algorithm")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error %q does not mention unsupported", err.Error())
	}
}

// TestVerify_EncodedPath_HexDecodesToRealFile is the bug-78
// regression: PG emits "Encoded-Path" (hex) instead of "Path"
// for filenames that aren't clean UTF-8.  Before the fix,
// Path=="" made filepath.Join(dataDir,"") resolve to the
// dataDir itself (a directory), so verifyOne false-failed with
// "expected regular file, got mode=…dir".  With the fix the hex
// is decoded and the real file is verified.
func TestVerify_EncodedPath_HexDecodesToRealFile(t *testing.T) {
	body := []byte("weirdly-named file body\n")
	name := "base/16384/odd\xff\x01name"
	root := writeFiles(t, map[string][]byte{name: body})
	encoded := hex.EncodeToString([]byte(name))
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Encoded-Path": "` + encoded + `", "Size": ` + itoa(len(body)) + `,
			 "Checksum-Algorithm": "CRC32C", "Checksum": "` + crc32cHex(body) + `"}
		]
	}`)
	res, err := verifybackup.Verify(context.Background(), manifest, root)
	if err != nil {
		t.Fatalf("Encoded-Path entry should verify, got %v", err)
	}
	if res.FilesChecked != 1 {
		t.Errorf("FilesChecked = %d, want 1", res.FilesChecked)
	}
}

// TestVerify_EncodedPath_DetectsMissingFile ensures the decoded
// path is actually used for the on-disk lookup: a manifest
// entry whose Encoded-Path points at an absent file must fail
// as "missing", not silently pass by resolving to the dataDir.
func TestVerify_EncodedPath_DetectsMissingFile(t *testing.T) {
	root := writeFiles(t, map[string][]byte{}) // nothing on disk
	encoded := hex.EncodeToString([]byte("base/16384/gone\xfffile"))
	manifest := []byte(`{
		"PostgreSQL-Backup-Manifest-Version": 1,
		"Files": [
			{"Encoded-Path": "` + encoded + `", "Size": 3,
			 "Checksum-Algorithm": "NONE", "Checksum": ""}
		]
	}`)
	_, err := verifybackup.Verify(context.Background(), manifest, root)
	if err == nil {
		t.Fatal("expected error for missing Encoded-Path file")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q does not mention missing", err.Error())
	}
}

// itoa is a tiny helper to avoid pulling strconv into a string-
// concatenation embedded JSON literal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

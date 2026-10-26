package verifybackup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/verifybackup"
)

// The manifest is decoded as a token stream: unknown keys around Files are
// skipped, a failing entry reports its index, and structural damage is still
// refused even though earlier entries have already been checked.
func TestVerify_StreamedDecode(t *testing.T) {
	a, b := []byte("aaa"), []byte("bbbb")
	root := writeFiles(t, map[string][]byte{"a": a, "b": b})
	entry := func(name string, body []byte, sum string) string {
		return `{"Path":"` + name + `","Size":` + itoa(len(body)) + `,"Checksum-Algorithm":"SHA256","Checksum":"` + sum + `"}`
	}
	good := entry("a", a, sha256Hex(a)) + "," + entry("b", b, sha256Hex(b))
	doc := func(files, after string) []byte {
		return []byte(`{"PostgreSQL-Backup-Manifest-Version":2,"System-Identifier":7,"Files":[` + files + `]` + after + `}`)
	}
	ctx := context.Background()

	res, err := verifybackup.Verify(ctx, doc(good, `,"WAL-Ranges":[{"Timeline":1}],"Manifest-Checksum":"x"`), root)
	if err != nil || res.FilesChecked != 2 {
		t.Fatalf("good manifest: %v %+v", err, res)
	}
	if _, err := verifybackup.Verify(ctx, doc(good+","+entry("b", b, sha256Hex(a)), ""), root); err == nil || !strings.Contains(err.Error(), "file[2]") {
		t.Errorf("corrupt third entry: %v", err)
	}
	if _, err := verifybackup.Verify(ctx, append(doc(good, ""), []byte(` {"x":1}`)...), root); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := verifybackup.Verify(ctx, []byte(`{"PostgreSQL-Backup-Manifest-Version":2,"Files":[`+good), root); err == nil {
		t.Error("truncated document accepted")
	}
	if _, err := verifybackup.Verify(ctx, []byte(`[]`), root); err == nil {
		t.Error("non-object accepted")
	}
	if _, err := verifybackup.Verify(ctx, []byte(`{"PostgreSQL-Backup-Manifest-Version":2,"Files":null}`), root); !errors.Is(err, verifybackup.ErrEmptyManifest) {
		t.Errorf("null Files: %v", err)
	}
}

// VerifyReader takes the manifest as a stream (a merged 100 TB manifest is
// tens of GB) with Verify's results, and an empty stream is "no manifest".
func TestVerifyReader(t *testing.T) {
	a := []byte("aaa")
	root := writeFiles(t, map[string][]byte{"a": a})
	doc := `{"PostgreSQL-Backup-Manifest-Version":2,"Files":[{"Path":"a","Size":3,"Checksum-Algorithm":"SHA256","Checksum":"` + sha256Hex(a) + `"}]}`
	res, err := verifybackup.VerifyReader(context.Background(), strings.NewReader(doc), root)
	if err != nil || res.FilesChecked != 1 {
		t.Fatalf("VerifyReader: %v %+v", err, res)
	}
	if _, err := verifybackup.VerifyReader(context.Background(), strings.NewReader(""), root); !errors.Is(err, verifybackup.ErrNoManifest) {
		t.Fatalf("empty stream: %v", err)
	}
}

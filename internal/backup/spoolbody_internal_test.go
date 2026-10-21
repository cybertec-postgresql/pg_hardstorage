package backup

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestSpoolBody_ReadSeekMatchesConcatenation(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "head")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("0123456789", 50)
	tail := `,"attestation":{"x":1}}`
	f.WriteString(head)
	b := &spoolBody{head: io.NewSectionReader(f, 0, int64(len(head))), headLen: int64(len(head)), tail: strings.NewReader(tail)}
	whole := head + tail
	got, err := io.ReadAll(b)
	if err != nil || string(got) != whole {
		t.Fatalf("sequential read: err=%v len=%d want %d", err, len(got), len(whole))
	}
	for _, off := range []int64{0, 1, 499, 500, 501, int64(len(whole)) - 3, int64(len(whole))} {
		if _, err := b.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		rest, _ := io.ReadAll(b)
		if string(rest) != whole[off:] {
			t.Fatalf("seek %d: got %q want %q", off, rest, whole[off:])
		}
	}
	if n, _ := b.Seek(0, io.SeekEnd); n != int64(len(whole)) {
		t.Fatalf("SeekEnd = %d", n)
	}
	b.Seek(-5, io.SeekCurrent)
	if rest, _ := io.ReadAll(b); string(rest) != whole[len(whole)-5:] {
		t.Fatalf("SeekCurrent: %q", rest)
	}
}

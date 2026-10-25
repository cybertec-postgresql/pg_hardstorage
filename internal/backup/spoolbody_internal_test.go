package backup

import (
	"errors"
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

func TestDeliveryError_WrapsAndReports(t *testing.T) {
	inner := io.ErrUnexpectedEOF
	var err error = &deliveryError{err: inner}
	if err.Error() != inner.Error() {
		t.Fatalf("Error() = %q", err.Error())
	}
	if u, ok := err.(interface{ Unwrap() error }); !ok || u.Unwrap() != inner {
		t.Fatal("Unwrap does not return the wrapped error")
	}
}

func TestWALOutcomeName_AllOutcomesNamed(t *testing.T) {
	for o, want := range map[rotateOutcome]string{
		rotateOutcomeRotated:             "rotated",
		rotateOutcomeWouldRotate:         "would_rotate",
		rotateOutcomeAlreadyRotated:      "already_rotated",
		rotateOutcomeSkippedUnencrypted:  "skipped_unencrypted",
		rotateOutcomeSkippedDifferentKEK: "skipped_different_kek",
		rotateOutcomeFailed:              "failed",
	} {
		if got := walOutcomeName(o); got != want {
			t.Errorf("walOutcomeName(%v) = %q, want %q", o, got, want)
		}
	}
}

func TestHoldWeakenError_MessageNamesTheHolder(t *testing.T) {
	err := error(&HoldWeakenError{Deployment: "db1", BackupID: "b1", Existing: &Hold{Holder: "legal"}, Why: "shorter expiry"})
	if !errors.Is(err, ErrHoldWeaken) {
		t.Fatal("errors.Is(ErrHoldWeaken) must match")
	}
	for _, want := range []string{"db1/b1", "legal", "shorter expiry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
}

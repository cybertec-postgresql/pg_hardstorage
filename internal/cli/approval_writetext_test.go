package cli

// `approval status` renders whatever approval records are on disk — the
// Approvals slice is copied out of the request verbatim, with no
// signature filter and no field validation. The renderer therefore has
// to survive a malformed record: it is the command an operator runs to
// find out why a request is not approved, so dying on the bad record is
// the one thing it must not do.

import (
	"strings"
	"testing"
	"time"
)

func statusBodyWith(fps ...string) approvalStatusBody {
	b := approvalStatusBody{
		ID: "req-1", Op: "restore", Initiator: "alice",
		Threshold: 2, ApprovalCount: len(fps), Status: "pending",
		ExpiresAt: time.Unix(0, 0).UTC(),
	}
	for _, fp := range fps {
		b.Approvals = append(b.Approvals, approvalEntry{
			KeyFingerprint: fp, At: time.Unix(0, 0).UTC(),
		})
	}
	return b
}

// The regression: any fingerprint length must render, not panic.
func TestApprovalStatusWriteText_ShortFingerprintDoesNotPanic(t *testing.T) {
	for _, fp := range []string{"", "a", "abc", "0123456789", "0123456789ab", "0123456789abc",
		"0123456789abcdef", strings.Repeat("f", 64)} {
		t.Run("len"+string(rune('0'+len(fp)%10)), func(t *testing.T) {
			var sb strings.Builder
			if err := statusBodyWith(fp).WriteText(&sb); err != nil {
				t.Fatalf("WriteText(%q): %v", fp, err)
			}
			if sb.Len() == 0 {
				t.Fatalf("WriteText(%q) rendered nothing", fp)
			}
		})
	}
}

func TestShortFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "(no key fingerprint)"},
		{"ab", "ab"},
		{"0123456789ab", "0123456789ab"},   // exactly the cap: no ellipsis
		{"0123456789abc", "0123456789ab…"}, // one over: abbreviated
		{strings.Repeat("f", 64), strings.Repeat("f", 12) + "…"},
	}
	for _, c := range cases {
		if got := shortFingerprint(c.in); got != c.want {
			t.Errorf("shortFingerprint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An empty fingerprint must read as a stated absence, not a bare
// ellipsis an operator would mistake for a rendering artefact.
func TestApprovalStatusWriteText_EmptyFingerprintIsNamed(t *testing.T) {
	var sb strings.Builder
	if err := statusBodyWith("").WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "no key fingerprint") {
		t.Errorf("an approval with no fingerprint must say so:\n%s", out)
	}
}

// The status line is the answer to "why isn't this approved yet" — it
// must always carry the count against the threshold.
func TestApprovalStatusWriteText_StatesCountAgainstThreshold(t *testing.T) {
	var sb strings.Builder
	if err := statusBodyWith("0123456789abcdef", "fedcba9876543210").WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	if out := sb.String(); !strings.Contains(out, "(2/2 approvals)") {
		t.Errorf("status line must show approvals against the threshold:\n%s", out)
	}
}

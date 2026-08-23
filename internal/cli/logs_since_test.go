package cli

// logs_since_test.go — `logs --since 24h` must work, because the help
// text offers "24h" as its first example.
//
// journalctl rejects a bare duration ("Failed to parse timestamp: 24h")
// and the value used to reach it untouched, so the most obvious way to
// invoke the flag failed — reported as a generic `internal` error that
// named neither --since nor the fix. Caught by L1_misc_smoke_b, one of
// the 162 scenarios no make target runs.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestJournalSince_BareDurationBecomesRelativePast(t *testing.T) {
	for _, in := range []string{"24h", "90m", "1h30m", "1.5h", "300ms", "2s", "2h45m30s"} {
		if got, want := journalSince(in), "-"+in; got != want {
			t.Errorf("journalSince(%q) = %q, want %q — journalctl needs a sign on a relative time",
				in, got, want)
		}
	}
}

// Everything systemd already understands must survive untouched;
// negating "yesterday" would break a spelling that worked.
func TestJournalSince_PassesThroughTimestampSpellings(t *testing.T) {
	for _, in := range []string{
		"yesterday", "today", "now", "1 hour ago", "24h ago",
		"2026-08-01 10:00:00", "2026-08-01T10:00:00Z",
		"-24h", "+1h", "",
	} {
		if got := journalSince(in); got != in {
			t.Errorf("journalSince(%q) = %q, want it unchanged", in, got)
		}
	}
}

// The mapping is only correct if systemd agrees. Assert against the
// real binary rather than trusting the table above, so a systemd
// change in accepted syntax fails here instead of in production.
func TestJournalSince_AcceptedByRealJournalctl(t *testing.T) {
	bin, err := exec.LookPath("journalctl")
	if err != nil {
		t.Skip("journalctl not on PATH; the unit tests above still pin the mapping")
	}
	for _, in := range []string{"24h", "90m", "1h30m", "1.5h", "300ms", "2s", "2h45m30s", "yesterday"} {
		out := journalSince(in)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, bin, "--since", out, "-n", "0", "--no-pager")
		combined, rerr := cmd.CombinedOutput()
		cancel()
		// A journal we cannot read is not a syntax failure; only a
		// parse complaint disproves the mapping.
		if rerr != nil && strings.Contains(string(combined), "Failed to parse timestamp") {
			t.Errorf("journalctl rejected journalSince(%q) = %q: %s", in, out, combined)
		}
	}
}

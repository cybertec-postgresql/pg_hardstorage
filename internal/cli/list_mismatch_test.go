package cli

import (
	"strings"
	"testing"
)

// renderList is a tiny helper: render a listBody to text.
func renderList(t *testing.T, b listBody) string {
	t.Helper()
	var sb strings.Builder
	if err := b.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return sb.String()
}

// TestList_SignatureMismatch_NotMistakenForDataLoss is the #6/F7 regression:
// when every backup failed signature verification with a key mismatch, the
// bare "No backups" line must NOT be the message — the operator has to learn
// the backups EXIST and how to recover them.
func TestList_SignatureMismatch_NotMistakenForDataLoss(t *testing.T) {
	out := renderList(t, listBody{
		Deployment:          "db1",
		Count:               0,
		Skipped:             2,
		SignatureMismatches: 2,
		Backups:             nil,
	})
	if strings.Contains(out, "No backups for") {
		t.Errorf("must not print the bare 'No backups' line on a key mismatch:\n%s", out)
	}
	for _, want := range []string{
		"exist but FAILED signature verification",
		"DIFFERENT key",
		"NOT gone",
		"pg_hardstorage doctor",
		"repo check",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in mismatch notice:\n%s", want, out)
		}
	}
}

// TestList_MixedMismatch_FooterNotice: some backups list fine but others are
// orphaned — the table renders and a footer notice still appears.
func TestList_MixedMismatch_FooterNotice(t *testing.T) {
	out := renderList(t, listBody{
		Deployment:          "db1",
		Count:               1,
		Skipped:             1,
		SignatureMismatches: 1,
		Backups: []backupSummary{
			{BackupID: "db1.full.x", Type: "full"},
		},
	})
	if !strings.Contains(out, "Backups for db1 (1)") {
		t.Errorf("table header missing:\n%s", out)
	}
	if !strings.Contains(out, "exist but FAILED signature verification") {
		t.Errorf("mixed case should still surface the mismatch footer:\n%s", out)
	}
}

// TestList_NonMismatchSkip_KeepsGenericMessage: a plain (non-key) skip keeps
// the original generic wording — we only special-case key mismatches.
func TestList_NonMismatchSkip_KeepsGenericMessage(t *testing.T) {
	out := renderList(t, listBody{
		Deployment: "db1",
		Count:      0,
		Skipped:    1,
		// SignatureMismatches == 0
	})
	if !strings.Contains(out, "No backups for") {
		t.Errorf("plain empty list should still say 'No backups':\n%s", out)
	}
	if !strings.Contains(out, "failed verification and were skipped") {
		t.Errorf("generic skip message missing:\n%s", out)
	}
}

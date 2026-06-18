package chat

import (
	"strings"
	"testing"
)

// TestScanDryRunMislabels_F4 reproduces the live failure: the model labeled
// `rotate db1 --apply` as a "Dry-run first … without touching anything",
// when --apply is exactly what makes it delete. The scan must flag it.
func TestScanDryRunMislabels_F4(t *testing.T) {
	text := strings.Join([]string{
		"```bash",
		"# 1a. Dry-run first — see what rotate would delete without touching anything",
		"pg_hardstorage rotate db1 --apply --output json",
		"```",
	}, "\n")
	warns := scanDryRunMislabels(text)
	if len(warns) != 1 {
		t.Fatalf("expected 1 mislabel warning, got %d: %#v", len(warns), warns)
	}
	if !strings.Contains(warns[0].Issue, "DESTRUCTIVE") || !strings.Contains(warns[0].Command, "--apply") {
		t.Errorf("warning should name the destructive command: %+v", warns[0])
	}
}

// TestScanDryRunMislabels_CorrectlyLabeled: a real dry-run (no --apply) and a
// destructive command that is NOT called safe must both stay silent.
func TestScanDryRunMislabels_NoFalsePositives(t *testing.T) {
	// Real dry-run, correctly described — must not warn.
	ok1 := "```bash\n# Dry-run: preview only, nothing is deleted\npg_hardstorage rotate db1\n```"
	if w := scanDryRunMislabels(ok1); len(w) != 0 {
		t.Errorf("a genuine dry-run (no --apply) must not warn: %#v", w)
	}
	// Destructive but honestly described — must not warn.
	ok2 := "```bash\n# This WILL permanently delete aged backups\npg_hardstorage rotate db1 --apply\n```"
	if w := scanDryRunMislabels(ok2); len(w) != 0 {
		t.Errorf("an honestly-labeled destructive command must not warn: %#v", w)
	}
}

// TestScanDryRunMislabels_DestructiveVerbs: shred/wipe are destructive
// regardless of flags; a "safe to run" label over them is flagged.
func TestScanDryRunMislabels_DestructiveVerbs(t *testing.T) {
	text := "```bash\n# safe to run, this is read-only\npg_hardstorage kms shred --reason x\n```"
	w := scanDryRunMislabels(text)
	if len(w) != 1 || !strings.Contains(w[0].Issue, "shred") {
		t.Errorf("shred-under-safe-label should warn: %#v", w)
	}
}

// TestClassifyDestructive: flag- and verb-based classification, ignoring
// flags that appear only inside a trailing comment.
func TestClassifyDestructive(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"pg_hardstorage rotate db1 --apply", true},
		{"pg_hardstorage repo wipe --yes", true},
		{"pg_hardstorage kms shred --reason x", true},
		{"pg_hardstorage restore db1 latest --force-foreign", false}, // not an exact destructive token
		{"pg_hardstorage rotate db1", false},
		{"pg_hardstorage list db1 # could add --apply later", false}, // flag only in comment
	}
	for _, c := range cases {
		got, _ := classifyDestructive(c.cmd)
		if got != c.want {
			t.Errorf("classifyDestructive(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

package pgbackrest

import (
	"testing"
)

// TestBoolIgnoredFlagsParse is the regression test for bug #48:
// start-fast / stop-auto / backup-standby are BOOLEAN pgBackRest knobs.
// Operators write them as bare switches (`--start-fast`, no value). If
// they were registered as STRING flags, cobra would fail parsing with
// "flag needs an argument: --start-fast". They must parse as bare bool
// switches (their values are ignored by the shim).
func TestBoolIgnoredFlagsParse(t *testing.T) {
	got := captureDispatch(t)

	root := NewRoot()
	root.SetArgs([]string{
		"backup",
		"--stanza", "db1",
		"--pg1-host", "h",
		"--repo1-path", "/r",
		"--start-fast",
		"--stop-auto",
		"--backup-standby",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("bare boolean pgBackRest knobs must parse; got error: %v", err)
	}
	// The dispatch still fired (backup verb), and none of the ignored
	// bool knobs leaked into the native argv.
	for _, a := range *got {
		switch a {
		case "--start-fast", "--stop-auto", "--backup-standby":
			t.Errorf("ignored bool knob %q leaked into native argv: %v", a, *got)
		}
	}
	if len(*got) == 0 || (*got)[0] != "backup" {
		t.Errorf("expected a `backup` dispatch; got %v", *got)
	}
}

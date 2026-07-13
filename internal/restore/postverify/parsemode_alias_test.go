package postverify

import "testing"

func TestParseModeAliases(t *testing.T) {
	for s, want := range map[string]Mode{"skip": ModeOff, "require": ModeRequired, "off": ModeOff, "required": ModeRequired} {
		got, err := ParseMode(s)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q)=%v,%v want %v", s, got, err, want)
		}
	}
}

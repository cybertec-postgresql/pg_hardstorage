package pitrtime

import "testing"

func TestRequireExplicitZone(t *testing.T) {
	refused := []string{
		"2026-04-27",
		"2026-04-27 09:42",
		"2026-04-27 09:42:00",
		"2026-04-27 09:42:00.123456",
		"2026-04-27T09:42:00",
		" 2026-04-27 09:42:00 ",
	}
	for _, v := range refused {
		if err := RequireExplicitZone("Barman", "--target-time", v); err == nil {
			t.Errorf("%q: offset-less timestamp accepted", v)
		}
	}
	accepted := []string{
		"2026-04-27 09:42:00+02",
		"2026-04-27 09:42:00+02:00",
		"2026-04-27 09:42:00 -0500",
		"2026-04-27 09:42:00 UTC",
		"2026-04-27T09:42:00Z",
		"5 minutes ago",
		"now",
	}
	for _, v := range accepted {
		if err := RequireExplicitZone("Barman", "--target-time", v); err != nil {
			t.Errorf("%q: refused: %v", v, err)
		}
	}
}

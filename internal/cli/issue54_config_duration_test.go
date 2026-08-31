package cli

// Issue #54: `pg_hardstorage agent` refused every pg_hardstorage.yaml
// the documentation told operators to write.
//
//	retention:
//	  policy: simple
//	  keep_for: 7w
//
//	{"op":"task.add_failed","body":{"error":
//	  "retention.keep_for \"7w\": time: unknown unit \"w\" in duration \"7w\""}}
//
// The CLI grew a d/w-aware parser for issue #52 — because its own help
// text advertised `--keep-for 30d` and the flag rejected it — but the
// YAML loader kept calling time.ParseDuration. So `rotate --keep-for 3d`
// worked while `keep_for: 3d` in the config did not, and four
// operator-facing documents show exactly the spelling that failed:
//
//	docs/operations/operator-guide.md:166        keep_for: 30d
//	docs/operations/operator-guide.md:648        keep_for: 30d
//	docs/how-to/operating/configuration-file.md  keep_for: 30d
//	docs/how-to/operating/set-retention.md       keep_for: 30d
//
// The agent skipped the rotate task for the affected deployment
// entirely — a WARNING, not a failure — so retention silently stopped
// running for anyone following the docs.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestBuildRetentionPolicy_KeepForAcceptsDayAndWeekUnits(t *testing.T) {
	cases := []struct {
		keepFor string
		want    time.Duration
	}{
		{"3d", 3 * 24 * time.Hour},     // issue #54's reproduction
		{"7w", 7 * 7 * 24 * time.Hour}, // issue #54's reproduction
		{"30d", 30 * 24 * time.Hour},   // the spelling in four docs
		{"1w2d", 9 * 24 * time.Hour},
		{"720h", 720 * time.Hour}, // the stdlib spelling still works
	}
	for _, c := range cases {
		p, err := buildRetentionPolicy(config.RetentionConfig{
			Policy: "simple", KeepFor: c.keepFor,
		})
		if err != nil {
			t.Errorf("keep_for %q: %v — this is a value the documentation tells operators "+
				"to write, and the agent skips the rotate task when it will not parse",
				c.keepFor, err)
			continue
		}
		sp, ok := p.(retention.SimplePolicy)
		if !ok {
			t.Fatalf("keep_for %q: got %T, want SimplePolicy", c.keepFor, p)
		}
		if sp.KeepFor != c.want {
			t.Errorf("keep_for %q → %v, want %v", c.keepFor, sp.KeepFor, c.want)
		}
	}
}

// Garbage must still be refused: widening the accepted units must not
// turn a typo into a silently-wrong retention window.
func TestBuildRetentionPolicy_KeepForStillRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"3x", "abc", "d", "30 d", "-"} {
		if _, err := buildRetentionPolicy(config.RetentionConfig{
			Policy: "simple", KeepFor: bad,
		}); err == nil {
			t.Errorf("keep_for %q was accepted — a typo must not become a retention window", bad)
		}
	}
}

// The empty case keeps its documented 30-day default.
func TestBuildRetentionPolicy_KeepForDefaultUnchanged(t *testing.T) {
	p, err := buildRetentionPolicy(config.RetentionConfig{Policy: "simple"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(retention.SimplePolicy).KeepFor; got != 30*24*time.Hour {
		t.Errorf("default keep_for = %v, want 720h", got)
	}
}

// patroni.interval shares the parser. A poll cadence in days is not a
// sensible value, but "unknown unit d" is not a sensible way to say so,
// and an operator who has learned that keep_for: 30d works will try it.
func TestParsePatroniInterval_AcceptsDayUnits(t *testing.T) {
	got, err := parsePatroniInterval("1d")
	if err != nil {
		t.Fatalf("patroni interval 1d: %v", err)
	}
	if got != 24*time.Hour {
		t.Errorf("1d = %v, want 24h", got)
	}
	// Existing spellings unchanged.
	if d, err := parsePatroniInterval("5s"); err != nil || d != 5*time.Second {
		t.Errorf("5s = %v (%v), want 5s", d, err)
	}
	if d, err := parsePatroniInterval(""); err != nil || d != 0 {
		t.Errorf("empty = %v (%v), want 0", d, err)
	}
}

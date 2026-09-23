package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// docs/reference/error-codes.md: "Every error a CLI command can surface
// is a structured *output.Error". Usage errors broke that. Flag and
// argument parsing fail before the pre-run hook installs the output
// dispatcher, so an unknown flag or a missing argument — the commonest
// failures there are — printed a plain text line even under -o json.
// A script parsing the envelope got nothing to parse.
func TestUsageErrorsHonourJSONOutput(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_ROOT", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "json") // format request that parsing cannot lose
	for _, args := range [][]string{
		{"backup", "db1", "--bogus-flag"},
		{"restore", "db1"},
	} {
		stdout, stderr, exit := rotateCLI(t, args...)
		if exit != 2 {
			t.Errorf("%v: exit %d, want 2 (usage)", args, exit)
		}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		raw := strings.TrimSpace(stdout)
		if raw == "" {
			raw = strings.TrimSpace(stderr)
		}
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Errorf("%v: -o json produced no JSON envelope:\n%s", args, raw)
			continue
		}
		if !strings.HasPrefix(env.Error.Code, "usage.") {
			t.Errorf("%v: code %q, want a usage.* code", args, env.Error.Code)
		}
	}
}

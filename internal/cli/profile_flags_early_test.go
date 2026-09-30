package cli

import "testing"

// Root flags must be found wherever they sit on the command line. The
// early parse used to stop at the first subcommand flag, so every root
// flag after it was silently ignored.
func TestParseRootFlagsEarly_FindsProfileFlagsAfterSubcommandFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--cpu-profile", "/tmp/p", "repo", "gc", "--repo", "file:///r", "--apply"},
		{"repo", "gc", "--repo", "file:///r", "--apply", "--cpu-profile", "/tmp/p"},
		{"repo", "gc", "--repo", "file:///r", "--min-chunk-age", "0", "--cpu-profile=/tmp/p", "-o", "json"},
		{"backup", "db1", "--fast", "--include-wal", "--cpu-profile", "/tmp/p"},
	} {
		root := NewRoot()
		parseRootFlagsEarly(root, args)
		if got, _ := root.Flags().GetString("cpu-profile"); got != "/tmp/p" {
			t.Errorf("%v: cpu-profile = %q, want /tmp/p", args, got)
		}
	}
}

// The lenient early parse must not leak into the real one: unknown
// flags are still rejected when the command executes.
func TestParseRootFlagsEarly_RestoresStrictParsing(t *testing.T) {
	root := NewRoot()
	parseRootFlagsEarly(root, []string{"repo", "gc", "--no-such-flag"})
	if root.FParseErrWhitelist.UnknownFlags {
		t.Fatal("parseRootFlagsEarly left unknown flags allowed on the root")
	}
}

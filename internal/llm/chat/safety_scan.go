// safety_scan.go — post-response scan that flags destructive commands the
// model described as harmless. Complements the structural command-validator:
// the validator checks that a command WOULD PARSE; this checks that a
// command's described intent matches what it actually does.
package chat

import "strings"

// destructiveFlagTokens mark a command as a real mutation rather than a
// preview. gc/prune/rotate/delete are destructive only WITH one of these.
var destructiveFlagTokens = []string{"--apply", "--force", "--yes"}

// destructiveVerbs are irreversible regardless of flags.
var destructiveVerbs = []string{"shred", "wipe"}

// classifyDestructive reports whether a pg_hardstorage command line performs
// a real mutation (vs a dry-run/preview) and the token responsible. A
// trailing "# comment" is stripped first so a flag mentioned only in a
// comment doesn't count.
func classifyDestructive(cmd string) (bool, string) {
	line := cmd
	if h := strings.Index(line, "#"); h >= 0 {
		line = line[:h]
	}
	fields := strings.Fields(line)
	for _, f := range fields {
		for _, d := range destructiveFlagTokens {
			if f == d {
				return true, d
			}
		}
		for _, v := range destructiveVerbs {
			if f == v {
				return true, v
			}
		}
	}
	return false, ""
}

// safeLabelPhrases claim a command is harmless / preview-only.
var safeLabelPhrases = []string{
	"dry-run", "dry run", "without touching", "without writing",
	"doesn't touch", "does not touch", "won't delete", "will not delete",
	"won't change", "will not change", "no-op", "nothing is deleted",
	"nothing will be deleted", "preview only", "just shows", "safe to run",
	"harmless", "read-only", "read only",
}

func hasSafeLabel(s string) bool {
	low := strings.ToLower(s)
	for _, p := range safeLabelPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// scanDryRunMislabels finds destructive commands inside fenced code blocks
// that the surrounding text labels as a dry-run / harmless preview — the F4
// failure where the model labeled `rotate db1 --apply` as "Dry-run first,
// without touching anything". Following such a mislabeled "dry run" actually
// deletes. Each hit becomes a CommandWarning.
//
// Detection window: the destructive command line plus up to four preceding
// lines, which covers an in-fence comment directly above the command and the
// prose item introducing the block.
func scanDryRunMislabels(text string) []CommandWarning {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	var out []CommandWarning
	seen := map[string]bool{}
	inFence := false
	for i, ln := range lines {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		clean := strings.TrimSpace(strings.TrimLeft(ln, " \t>$"))
		if !strings.HasPrefix(clean, "pg_hardstorage ") {
			continue
		}
		dest, marker := classifyDestructive(clean)
		if !dest {
			continue
		}
		lo := i - 4
		if lo < 0 {
			lo = 0
		}
		window := strings.Join(lines[lo:i+1], "\n")
		if !hasSafeLabel(window) {
			continue
		}
		cmd := strings.Trim(clean, "`")
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		out = append(out, CommandWarning{
			Command: cmd,
			Issue: "DESTRUCTIVE (" + marker + ") but described as a dry-run/preview — running it WILL " +
				"execute/delete. A real dry-run omits --apply/--force/--yes.",
		})
	}
	return out
}

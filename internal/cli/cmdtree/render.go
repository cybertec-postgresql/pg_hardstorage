// render.go — compact depth-limited command-tree catalog for LLM system-prompt injection.
package cmdtree

import (
	"fmt"
	"strings"
)

// Catalog renders a compact, depth-limited view of the
// command tree for injection into the LLM's system prompt.
// The shape is optimised for "give the model the right
// verbs in the smallest possible token footprint":
//
//	backup            Take a backup of a deployment
//	deployment        Manage deployments
//	  add             Add a new deployment to the config
//	  edit            Edit an existing deployment
//	  list            List deployments
//	  remove          Remove a deployment
//	  test            Test deployment connectivity
//	repo              Manage repository
//	  check           Verify signatures + metadata
//	  init            Initialise a new repository
//	  ...
//
// Width is the intended terminal-style padding for the
// command column — 18 reads well in monospace and leaves
// room for the description.  Depth caps how many levels
// deep we render: 2 covers nearly every real command in
// pg_hardstorage and keeps the catalog under 100 lines.
//
// The catalog deliberately omits flags.  Flags live in
// `read_command_help <command>` because there are 200+ of
// them across the tree and inlining them would dwarf the
// rest of the system prompt.  The catalog plus the lookup
// tool together give the model "knows the verbs / can
// look up the flags".
func Catalog(root *Node, depth int) string {
	if root == nil {
		return ""
	}
	var b strings.Builder
	for _, top := range root.VisibleChildren() {
		writeCatalogNode(&b, top, 0, depth)
	}
	return b.String()
}

func writeCatalogNode(b *strings.Builder, n *Node, level, depth int) {
	if level > depth {
		return
	}
	indent := strings.Repeat("  ", level)
	const colWidth = 18
	left := indent + n.Name
	pad := colWidth - len(left)
	if pad < 1 {
		pad = 1
	}
	short := n.Short
	if short == "" {
		short = strings.TrimSpace(strings.SplitN(n.Long, "\n", 2)[0])
	}
	fmt.Fprintf(b, "%s%s%s\n", left, strings.Repeat(" ", pad), short)
	if level >= depth {
		return
	}
	for _, c := range n.VisibleChildren() {
		writeCatalogNode(b, c, level+1, depth)
	}
}

// Help renders a synopsis + flag list for a single
// command, mimicking the shape of `--help` output but
// without the cobra-injected examples / footers that
// don't add value for the LLM.  Returns the empty string
// when path doesn't resolve.
//
// The path is the segments after the binary name —
// {"deployment", "add"} for `pg_hardstorage deployment
// add`.  Aliases are accepted, hidden commands are
// rendered (the model may legitimately need to know about
// them when triaging a transcript that already mentions
// one), but flags marked hidden are omitted.
func Help(root *Node, path []string) string {
	if root == nil {
		return ""
	}
	n := root.Find(path)
	if n == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", n.Path)
	if n.Use != "" && n.Use != n.Name {
		fmt.Fprintf(&b, "  Usage: %s\n", n.Use)
	}
	if n.Short != "" {
		fmt.Fprintf(&b, "  %s\n", n.Short)
	}
	if n.Long != "" && n.Long != n.Short {
		// Long descriptions can be multi-paragraph; keep
		// the first paragraph (until the first blank line)
		// since that's where the high-signal summary sits.
		first := firstParagraph(n.Long)
		if first != "" && first != n.Short {
			fmt.Fprintf(&b, "\n  %s\n", strings.ReplaceAll(first, "\n", "\n  "))
		}
	}
	if len(n.Aliases) > 0 {
		fmt.Fprintf(&b, "\n  Aliases: %s\n", strings.Join(n.Aliases, ", "))
	}
	if len(n.Children) > 0 {
		fmt.Fprintf(&b, "\n  Subcommands:\n")
		for _, c := range n.VisibleChildren() {
			fmt.Fprintf(&b, "    %-16s %s\n", c.Name, c.Short)
		}
	}
	if len(n.Flags) > 0 {
		fmt.Fprintf(&b, "\n  Flags:\n")
		for _, f := range n.Flags {
			fmt.Fprintf(&b, "    %s\n", formatFlag(f))
		}
	}
	return b.String()
}

func formatFlag(f Flag) string {
	left := "--" + f.Name
	if f.Shorthand != "" {
		left = "-" + f.Shorthand + ", " + left
	}
	if f.Type != "" && f.Type != "bool" {
		left += " " + f.Type
	}
	usage := f.Usage
	tags := []string{}
	if f.Required {
		tags = append(tags, "required")
	}
	if f.Default != "" && f.Default != "false" && f.Default != "0" && f.Default != "[]" {
		tags = append(tags, "default: "+f.Default)
	}
	if len(tags) > 0 {
		usage = strings.TrimRight(usage, " ") + " (" + strings.Join(tags, "; ") + ")"
	}
	return fmt.Sprintf("%-32s %s", left, usage)
}

func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

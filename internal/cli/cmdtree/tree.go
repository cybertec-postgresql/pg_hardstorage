// Package cmdtree extracts a serialisable, language-model-
// friendly view of the cobra command tree.  The same tree
// drives three consumers:
//
//   - The LLM session's system prompt (rendered as a compact
//     "command catalog" so the model has ground truth about
//     valid verbs / subcommands and stops hallucinating
//     `deployment create --name X` when the real shape is
//     `deployment add <name> --connection ... --repo ...`).
//   - The `read_command_help` LLM tool, which expands a
//     command path to a synopsis + flag list on demand.
//   - The `suggest_command` validation gate, which parses
//     the model's proposed command and rejects unknown
//     subcommands / flags with did-you-mean hints before
//     they reach the operator.
//
// Keeping the introspection logic in its own package lets
// us unit-test it without booting the CLI and lets the
// docsgen binary (which already walks the tree) consolidate
// onto the same code if we ever want to.  The package
// imports cobra; consumers do not need to.
package cmdtree

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Node is a frozen snapshot of one cobra command.  We copy
// the fields we need rather than holding a pointer to the
// live *cobra.Command so the snapshot can be safely shared
// across goroutines (the LLM session may render the catalog
// while another command is mutating its own flag set during
// PreRun).
type Node struct {
	Name   string // bare verb, e.g. "add"
	Path   string // space-separated path, e.g. "pg_hardstorage deployment add"
	Use    string // raw cobra Use line, e.g. "add <name>"
	Short  string // one-line summary
	Long   string // full description (may be empty)
	Hidden bool   // hidden commands are excluded from the catalog
	// Runnable is true when the command itself has a Run/RunE — i.e.
	// it can execute with positional args, not merely route to
	// subcommands.  A command can be BOTH runnable AND have children
	// (e.g. `backup <deployment>` also hosts `backup delete`); the
	// validator needs this to tell a valid positional (`backup db1`)
	// from an unknown subcommand (`backup delte`).
	Runnable bool
	// MinArgs / MaxArgs are the positional-argument bounds derived from the
	// Use line's placeholders (`<required>` / `[optional]` / `...` variadic).
	// MaxArgs == -1 means unbounded (variadic or no parseable spec). The
	// validator enforces these on RUNNABLE commands so a hallucinated
	// subcommand on a runnable parent (`backup full db1` — two positionals
	// where one is allowed) is caught instead of silently treated as a
	// stray positional.
	MinArgs  int
	MaxArgs  int
	Aliases  []string
	Flags    []Flag  // local + inherited persistent flags
	Children []*Node // sorted alphabetically
}

// parseArgSpec derives positional-argument bounds from a cobra Use line.
//
//	"backup <deployment>"                  -> (1, 1)
//	"restore <deployment> <backup|latest>" -> (2, 2)
//	"doctor [<deployment>]"                -> (0, 1)
//	"undelete <deployment> <id> [id...]"   -> (2, -1)  variadic
//	"version" / no placeholders            -> (0, -1)  don't enforce a max
//
// A bare Use (just the command name) or one with no `<>`/`[]` placeholders
// yields (0, -1) so we never flag a too-many on a command whose Use simply
// under-specifies its args — conservative, to avoid false positives.
func parseArgSpec(use string) (minArgs, maxArgs int) {
	req, opt, variadic := usePositionals(use)
	if len(req) == 0 && len(opt) == 0 && !variadic {
		return 0, -1 // no positionals specified → don't enforce a maximum
	}
	if variadic {
		return len(req), -1
	}
	return len(req), len(req) + len(opt)
}

// usePositionals extracts the POSITIONAL placeholders from a cobra Use line
// (command name stripped), returning the mandatory ones, the optional ones,
// and whether the tail is variadic.  It is shared by parseArgSpec (which
// only needs the counts) and usePlaceholders (which renders the names in a
// message hint), so both agree on what counts as a positional.
//
// Three things it must get right, each a past false positive:
//   - Optional FLAG groups ("[--name <id>]", "[flags]") are NOT positionals.
//   - A flag's VALUE placeholder ("--repo <url>") is NOT a positional — the
//     <url> belongs to --repo, not the arg list (this is why a fully-correct
//     `audit export-bundle --repo X --out Y` must not be flagged for missing
//     positionals).
//   - Optional groups NEST: "[<deployment> [<expression>]]" carries TWO
//     optional positionals, not one — so `schedule db1 "daily_at 02:00"`
//     (two positionals) is valid.
func usePositionals(use string) (req, opt []string, variadic bool) {
	use = strings.TrimSpace(use)
	if i := strings.IndexAny(use, " \t"); i >= 0 {
		use = use[i+1:]
	} else {
		return nil, nil, false // bare name → no positionals
	}
	collectPositionals(use, false, &req, &opt, &variadic)
	return req, opt, variadic
}

// collectPositionals walks the Use-arg tokens.  optional=true means we are
// recursing inside an optional "[...]" group, so every positional found is
// optional regardless of its own brackets.
func collectPositionals(s string, optional bool, req, opt *[]string, variadic *bool) {
	toks := argTokens(s)
	for k := 0; k < len(toks); k++ {
		t := toks[k]
		switch {
		case strings.HasPrefix(t, "<"):
			if optional {
				*opt = append(*opt, t)
			} else {
				*req = append(*req, t)
			}
			if strings.Contains(t, "...") {
				*variadic = true
			}
		case strings.HasPrefix(t, "["):
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "["), "]"))
			// Optional FLAG group or the generic "[flags]" — not positional.
			if strings.HasPrefix(inner, "-") || inner == "flags" {
				continue
			}
			// Bare variadic repeat like "[id...]" (no angle brackets): it
			// just marks the previous positional as repeatable.
			if strings.Contains(inner, "...") && !strings.Contains(inner, "<") {
				*opt = append(*opt, t)
				*variadic = true
				continue
			}
			// Recurse so a NESTED optional ("[<deployment> [<expression>]]")
			// contributes each of its positionals.  But if the group holds no
			// <...>/nested positional of its own — an alternation of literals
			// like "[latest|<backup-id>]" (one token, no whitespace, so the
			// inner <backup-id> is part of a single alternation slot) or a bare
			// literal like "[force]" — it is STILL one optional positional
			// slot, so count it as one.
			before := len(*req) + len(*opt)
			collectPositionals(inner, true, req, opt, variadic)
			if len(*req)+len(*opt) == before {
				*opt = append(*opt, t)
			}
		case strings.HasPrefix(t, "-"):
			// A flag written into the Use line ("--repo <url>"): its value
			// placeholder is the NEXT token and is not a positional — skip it.
			if k+1 < len(toks) && strings.HasPrefix(toks[k+1], "<") {
				k++
			}
		default:
			// A bare literal word (e.g. a verb in "<init|check>" lives inside
			// <>, so this is rare) — not a positional.  Honour a trailing
			// "..." variadic marker just in case.
			if strings.Contains(t, "...") {
				*variadic = true
			}
		}
	}
}

// argTokens splits a Use-arg string into top-level tokens: a balanced "[...]"
// group or a "<...>" placeholder is ONE token (it may contain spaces), and
// everything else splits on whitespace.
func argTokens(s string) []string {
	var toks []string
	for i := 0; i < len(s); {
		switch {
		case s[i] == ' ' || s[i] == '\t':
			i++
		case s[i] == '[':
			j := matchCloseBracket(s[i:])
			if j < 0 {
				toks = append(toks, s[i:])
				return toks
			}
			toks = append(toks, s[i:i+j+1])
			i += j + 1
		case s[i] == '<':
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				toks = append(toks, s[i:])
				return toks
			}
			toks = append(toks, s[i:i+j+1])
			i += j + 1
		default:
			st := i
			for i < len(s) && s[i] != ' ' && s[i] != '\t' {
				i++
			}
			toks = append(toks, s[st:i])
		}
	}
	return toks
}

// matchCloseBracket returns the index, within s (which must start with '['),
// of the ']' that closes it — accounting for nesting; -1 if unbalanced.
func matchCloseBracket(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// Flag is a frozen snapshot of one pflag.Flag.  Captures
// just what the LLM needs to know: the long name, optional
// short, type, default value, and the help blurb.
type Flag struct {
	Name      string // long name without leading "--"
	Shorthand string // empty if not set
	Type      string // pflag's "type" string ("string", "bool", "int", ...)
	Default   string // pflag's default-value string
	Usage     string // help blurb
	Required  bool   // honoured via `cobra_annotations[BashCompOneRequiredFlag]`
}

// Walk freezes the cobra tree rooted at root.  Hidden
// commands are kept (the validator may need to recognise
// them) but the catalog renderer skips them.  The
// special-case "help" + "completion" + "__complete" trees
// — which cobra adds automatically — are dropped at this
// layer because suggesting them to an operator is never
// useful.
func Walk(root *cobra.Command) *Node {
	if root == nil {
		return nil
	}
	return walk(root, "")
}

func walk(c *cobra.Command, parentPath string) *Node {
	path := c.Name()
	if parentPath != "" {
		path = parentPath + " " + c.Name()
	}
	n := &Node{
		Name:   c.Name(),
		Path:   path,
		Use:    c.Use,
		Short:  c.Short,
		Long:   c.Long,
		Hidden: c.Hidden,
		// A group whose RunE was synthesised purely to reject unknown
		// subcommands (hardenGroupCommands) is NOT runnable for
		// validation purposes — `deployment create` must still be
		// classified as unknown_command, not as positional args.
		Runnable: c.Runnable() && c.Annotations["pg_hardstorage.group_guard"] != "1",
		Aliases:  append([]string(nil), c.Aliases...),
		Flags:    collectFlags(c),
	}
	n.MinArgs, n.MaxArgs = parseArgSpec(c.Use)
	for _, child := range c.Commands() {
		if isCobraInternal(child.Name()) {
			continue
		}
		n.Children = append(n.Children, walk(child, path))
	}
	sort.Slice(n.Children, func(i, j int) bool {
		return n.Children[i].Name < n.Children[j].Name
	})
	return n
}

// isCobraInternal returns true for the housekeeping commands
// cobra injects automatically (`help`, `completion`, the
// hidden `__complete` / `__completeNoDesc` pair).  These
// are never useful to suggest to an operator and just
// noise up the catalog.
func isCobraInternal(name string) bool {
	switch name {
	case "help", "completion", "__complete", "__completeNoDesc":
		return true
	}
	return false
}

// collectFlags merges the local flag set with inherited
// persistent flags, deduping on long name (a child's flag
// shadows the parent's).  Both surfaces matter: a typo'd
// `--no-color` (a persistent flag on root) and a typo'd
// `--connection` (a local flag on `deployment add`) should
// both be catchable by the validator.
func collectFlags(c *cobra.Command) []Flag {
	seen := map[string]bool{}
	var out []Flag
	add := func(f *pflag.Flag) {
		if seen[f.Name] {
			return
		}
		seen[f.Name] = true
		// A flag is required if cobra's MarkFlagRequired annotation is
		// set OR the help text declares it. Many pg_hardstorage commands
		// enforce --repo / --pg-connection MANUALLY in RunE (returning a
		// "X is required" error) rather than via MarkFlagRequired, so the
		// annotation is absent — but their usage string ends with
		// "(required)". Honouring that lets the LLM command-validator
		// flag a dropped required flag on those commands too (F2).
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		if !required && strings.Contains(f.Usage, "(required)") {
			required = true
		}
		out = append(out, Flag{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      f.Value.Type(),
			Default:   f.DefValue,
			Usage:     f.Usage,
			Required:  required,
		})
	}
	c.LocalFlags().VisitAll(add)
	c.InheritedFlags().VisitAll(add)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find walks the tree to the node at the given path.  Path
// segments are matched against Name OR any alias (so
// `pg_hardstorage deployment add` and `pg_hardstorage
// deployment ad` both resolve when "ad" is an alias).  The
// root's own name is NOT consumed — callers that want to
// look up "deployment add" pass []string{"deployment", "add"}.
// Returns nil when the path does not resolve.
func (n *Node) Find(path []string) *Node {
	cur := n
	for _, seg := range path {
		next := cur.findChild(seg)
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

func (n *Node) findChild(name string) *Node {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

// FlagByName returns the flag with the given long name on
// this node, or nil.  Used by the validator to decide
// whether `--unknown` is a real flag at the suggested
// command's depth.
func (n *Node) FlagByName(name string) *Flag {
	name = strings.TrimPrefix(name, "--")
	name = strings.TrimPrefix(name, "-")
	for i := range n.Flags {
		if n.Flags[i].Name == name || n.Flags[i].Shorthand == name {
			return &n.Flags[i]
		}
	}
	return nil
}

// VisibleChildren returns the non-hidden children, in the
// order Walk stored them (alphabetical).  Used by the
// catalog renderer.
func (n *Node) VisibleChildren() []*Node {
	out := make([]*Node, 0, len(n.Children))
	for _, c := range n.Children {
		if c.Hidden {
			continue
		}
		out = append(out, c)
	}
	return out
}

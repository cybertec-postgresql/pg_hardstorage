package cli_test

// docs_flags_test.go — a `--flag` shown in the prose docs must exist.
//
// The generated CLI reference under docs/reference/cli/ comes from
// cobra and is kept current by the docs-regen drift gate, so it cannot
// lie. The how-tos, tutorials and runbooks are hand-written, mention
// 160-odd distinct flags between them, and nothing checked any of them.
//
// That is the same gap that produced `use_path_style=true` in the
// Kubernetes tutorial — a parameter no code has ever read, sitting in a
// copy-pasteable command line. A flag is worse than a URL parameter,
// because an unknown flag makes the whole command exit 2: the operator
// following the page does not get a subtly wrong result, they get a
// failure with the docs as their only guide.
//
// Attribution is per command where the prose gives one. `pg_hardstorage
// wal stream --once` is checked against `wal stream`'s own flag set, so
// a flag that exists somewhere else in the CLI is still caught when
// shown on a command that does not accept it.

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// cmdLineRe matches a documented invocation and captures everything
// after the binary name.
var cmdLineRe = regexp.MustCompile(`(?m)pg_hardstorage((?:\s+[a-z][a-z0-9-]*)*)((?:\s+--?[a-z][a-z0-9-]*(?:[= ][^\s\\]*)?)*)`)

// flagRe pulls the long flags out of that tail.
var flagRe = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)

// docsFlagsExempt are tokens that look like flags but are not ours.
var docsFlagsExempt = map[string]bool{
	// Passed through to other programs in documented pipelines.
	"help": true, "version": true,
	// psql / pg_basebackup / docker / kubectl / gh flags appear in the
	// same code blocks as ours.
	"dbname": true, "username": true, "host": true, "port": true,
	"namespace": true, "context": true, "from-literal": true,
	"filename": true, "output-format": true, "rm": true, "name": true,
	"env": true, "volume": true, "publish": true, "detach": true,
	"repo-url": true, "set": true, "values": true, "create-namespace": true,

	// Build-flavour gated, and the docs say so. --fips-strict only
	// exists in a GOEXPERIMENT=boringcrypto build, which is not the
	// binary this test introspects. docs/compliance/fedramp.md states
	// plainly that "the FIPS build is not yet shipped; --fips-strict is
	// not present in the default binary" — that documentation is honest,
	// and the guard cannot see build flavours. Flagging it would be the
	// guard calling correct docs wrong.
	"fips-strict": true,
}

// TestDocumentedFlagsExist is the guard.
func TestDocumentedFlagsExist(t *testing.T) {
	nodes := dumpTree(t)

	// path → flags valid there, plus the union for un-attributable
	// mentions.
	byPath := make(map[string]map[string]bool, len(nodes))
	union := map[string]bool{}
	for _, n := range nodes {
		set := make(map[string]bool, len(n.Flags))
		for _, f := range n.Flags {
			set[f] = true
			union[f] = true
		}
		byPath[n.Path] = set
	}
	if len(union) == 0 {
		t.Fatal("__dump-cmd-tree emitted no flags — the guard would pass vacuously")
	}

	root := repoRootFromTest(t)
	corpus := docsCorpus(t, root)

	type badFlag struct{ flag, cmd, line string }
	var bad []badFlag
	seen := map[string]bool{}

	for _, m := range cmdLineRe.FindAllStringSubmatch(corpus, -1) {
		words := strings.Fields(m[1])
		tail := m[2]
		if tail == "" {
			continue
		}
		// Longest command path that actually exists; the remaining
		// words are positional arguments.
		cmdPath := ""
		for i := len(words); i > 0; i-- {
			cand := strings.Join(words[:i], " ")
			if _, ok := byPath[cand]; ok {
				cmdPath = cand
				break
			}
		}
		// Accept a flag valid anywhere in the matched command's SUBTREE,
		// not only on the node itself.
		//
		// `pg_hardstorage llm --skill restore` really works — llm is
		// runnable and is chat — yet the dump reports `skill` on
		// `llm chat`, because that is where cobra registers it. A
		// stricter rule flagged a documented invocation that the binary
		// accepts, and a guard that calls correct docs wrong is one
		// nobody will keep.
		//
		// It still catches the real cases: --fix on doctor, --tenant on
		// list, --scenario on gameday run exist nowhere in their
		// subtrees.
		valid := union
		if cmdPath != "" {
			valid = subtreeFlags(byPath, cmdPath)
		}
		for _, fm := range flagRe.FindAllStringSubmatch(tail, -1) {
			name := fm[1]
			if docsFlagsExempt[name] || valid[name] {
				continue
			}
			key := cmdPath + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			where := cmdPath
			if where == "" {
				where = "(no command matched)"
			}
			bad = append(bad, badFlag{flag: name, cmd: where,
				line: strings.TrimSpace(m[0])})
		}
	}

	if len(bad) > 0 {
		sort.Slice(bad, func(i, j int) bool {
			if bad[i].cmd != bad[j].cmd {
				return bad[i].cmd < bad[j].cmd
			}
			return bad[i].flag < bad[j].flag
		})
		var lines []string
		for _, b := range bad {
			lines = append(lines, "--"+b.flag+" on `"+b.cmd+"`\n      "+truncURL(b.line))
		}
		t.Errorf("%d documented flag(s) do not exist on the command they are shown with:"+
			"\n  %s\n\nAn unknown flag makes cobra exit 2, so an operator following the "+
			"page gets a hard failure with the docs as their only guide. The generated "+
			"reference under docs/reference/cli/ cannot drift — it comes from cobra — but "+
			"prose is hand-written and nothing checked it until now.",
			len(bad), strings.Join(lines, "\n  "))
	}
}

// subtreeFlags is every flag valid on cmdPath or any descendant.
func subtreeFlags(byPath map[string]map[string]bool, cmdPath string) map[string]bool {
	out := map[string]bool{}
	for p, set := range byPath {
		if p != cmdPath && !strings.HasPrefix(p, cmdPath+" ") {
			continue
		}
		for f := range set {
			out[f] = true
		}
	}
	return out
}

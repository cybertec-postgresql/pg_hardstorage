package cmdtree_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// arityRoot mirrors the real shapes the arity check must handle: a runnable
// parent that also hosts a subcommand (`backup <deployment>` + `backup
// delete`), an optional positional (`doctor [<deployment>]`), and a variadic
// (`undelete <deployment> <id> [id...]`).
func arityRoot() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}

	backup := &cobra.Command{Use: "backup <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	backup.Flags().String("repo", "", "repository URL") // a value flag, to test value-vs-positional
	backup.AddCommand(&cobra.Command{Use: "delete <deployment> <id>", Run: func(_ *cobra.Command, _ []string) {}})
	root.AddCommand(backup)

	root.AddCommand(&cobra.Command{Use: "doctor [<deployment>]", Run: func(_ *cobra.Command, _ []string) {}})
	root.AddCommand(&cobra.Command{Use: "undelete <deployment> <id> [id...]", Run: func(_ *cobra.Command, _ []string) {}})

	// A pure parent (no Run): its `<a|b>` Use is a verb list, NOT a positional.
	repo := &cobra.Command{Use: "repo <init|check>"}
	repo.AddCommand(&cobra.Command{Use: "init <url>", Run: func(_ *cobra.Command, _ []string) {}})
	root.AddCommand(repo)

	// `notify add <plugin> [--name <id>] [--set k=v ...]`: the bracketed
	// FLAGS must NOT be counted as positionals (the round-4 false positive).
	notify := &cobra.Command{Use: "notify"}
	add := &cobra.Command{Use: "add <plugin> [--name <id>] [--set key=value ...]", Run: func(_ *cobra.Command, _ []string) {}}
	add.Flags().String("name", "", "")
	add.Flags().StringArray("set", nil, "")
	notify.AddCommand(add)
	root.AddCommand(notify)
	return root
}

// TestArity_BracketedFlagsNotCountedAsPositionals is the round-4 regression:
// a Use line with optional flags in brackets (`[--name <id>] [--set k=v ...]`)
// must read as ONE positional (<plugin>), not 2–4 — so a valid
// `notify add webhook --name x --set k=v` is no longer wrongly flagged.
func TestArity_BracketedFlagsNotCountedAsPositionals(t *testing.T) {
	valid := "pg_hardstorage notify add webhook --name prom --set url=http://x --set min_severity=info"
	if err := validate(t, valid); err != nil {
		t.Errorf("bracketed flags wrongly counted as positionals; got: %v", err)
	}
	// Genuine too-few / too-many still caught, with a clean placeholder hint.
	tooFew := validate(t, "pg_hardstorage notify add")
	if ve, ok := tooFew.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Fatalf("notify add (no plugin) should be arg_count, got: %v", tooFew)
	} else if !strings.Contains(ve.Message, "(<plugin>)") {
		t.Errorf("hint should show only the real positional, got: %v", ve.Message)
	}
	tooMany := validate(t, "pg_hardstorage notify add slack extra")
	if ve, ok := tooMany.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Errorf("notify add slack extra should be arg_count, got: %v", tooMany)
	}
}

func validate(t *testing.T, cmd string) error {
	t.Helper()
	return cmdtree.Validate(cmdtree.Walk(arityRoot()), cmd, "pg_hardstorage")
}

// TestArity_HallucinatedSubcommandOnRunnableParent is the round-2 regression:
// `backup full db1` — `full` is read as a positional (backup is runnable), so
// the old validator missed it; arity now catches the two-positional command
// against `backup <deployment>`'s one.
func TestArity_HallucinatedSubcommandOnRunnableParent(t *testing.T) {
	err := validate(t, "pg_hardstorage backup full db1")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok || ve.Kind != "arg_count" {
		t.Fatalf("want arg_count, got: %v", err)
	}
	if !strings.Contains(ve.Message, "accepts 1 positional argument") || !strings.Contains(ve.Message, "got 2") {
		t.Errorf("message should name the bound + actual: %v", ve)
	}
	if !strings.Contains(ve.Message, "<deployment>") {
		t.Errorf("message should show the Use placeholder: %v", ve)
	}
}

func TestArity_Cases(t *testing.T) {
	cases := []struct {
		cmd  string
		kind string // "" = valid
	}{
		{"pg_hardstorage backup db1", ""},                  // exactly one
		{"pg_hardstorage backup", "arg_count"},             // too few
		{"pg_hardstorage backup a b", "arg_count"},         // too many
		{"pg_hardstorage backup delete db1 id1", ""},       // real subcommand, 2 positionals
		{"pg_hardstorage backup delete db1", "arg_count"},  // subcommand too few
		{"pg_hardstorage doctor", ""},                      // optional positional, 0 ok
		{"pg_hardstorage doctor db1", ""},                  // optional positional, 1 ok
		{"pg_hardstorage doctor a b", "arg_count"},         // optional positional, 2 too many
		{"pg_hardstorage undelete db1 i1", ""},             // variadic min met
		{"pg_hardstorage undelete db1 i1 i2 i3", ""},       // variadic extra ok
		{"pg_hardstorage undelete db1", "arg_count"},       // variadic min not met
		{"pg_hardstorage backup db1 --repo file:///r", ""}, // flag VALUE is not a positional
		{"pg_hardstorage backup --repo file:///r db1", ""}, // flag-before-positional, value not counted
	}
	for _, c := range cases {
		err := cmdtree.Validate(cmdtree.Walk(arityRoot()), c.cmd, "pg_hardstorage")
		got := ""
		if ve, ok := err.(*cmdtree.ValidationError); ok {
			got = ve.Kind
		} else if err != nil {
			got = "other:" + err.Error()
		}
		if got != c.kind {
			t.Errorf("%q: kind = %q, want %q (err=%v)", c.cmd, got, c.kind, err)
		}
	}
}

// TestArity_PureParentNotEnforced: a non-runnable parent's `<init|check>` Use
// is a verb list, not positional args — arity must not fire on it (the
// unknown-subcommand check owns that path).
func TestArity_PureParentNotEnforced(t *testing.T) {
	// `repo init <url>` is valid (1 positional).
	if err := validate(t, "pg_hardstorage repo init file:///r"); err != nil {
		t.Errorf("valid subcommand should pass: %v", err)
	}
	// `repo bogus` is an unknown subcommand, NOT an arg_count error.
	err := validate(t, "pg_hardstorage repo bogus")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "unknown_command" {
		t.Errorf("pure-parent typo should be unknown_command, got: %v", err)
	}
}

// TestArity_ShellOperatorsNotCounted is the round-5 regression: a command
// that is backgrounded / piped / chained / redirected must validate as just
// the command itself — the shell operators and everything after them are not
// positional arguments. (The model emitted `wal stream db1 ... &`.)
func TestArity_ShellOperatorsNotCounted(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	root.AddCommand(&cobra.Command{Use: "list <deployment>", Run: func(_ *cobra.Command, _ []string) {}})
	init := &cobra.Command{Use: "init <url>", Run: func(_ *cobra.Command, _ []string) {}}
	root.AddCommand(init)
	tree := cmdtree.Walk(root)

	for _, c := range []string{
		"pg_hardstorage list db1 &",               // backgrounded
		"pg_hardstorage list db1 | jq .",          // piped
		"pg_hardstorage list db1 ; echo done",     // chained
		"pg_hardstorage list db1 > /tmp/out.json", // redirected
		"pg_hardstorage list db1 2> /tmp/err",     // stderr redirect
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid (shell op stripped), got: %v", c, err)
		}
	}

	// An `&` INSIDE a quoted URL is part of the argument, not an operator —
	// the command is still valid (one positional).
	if err := cmdtree.Validate(tree, "pg_hardstorage init 's3://b/?a=1&b=2'", "pg_hardstorage"); err != nil {
		t.Errorf("quoted-URL ampersand must not truncate: %v", err)
	}
	// A genuine too-many before a shell op is still caught.
	err := cmdtree.Validate(tree, "pg_hardstorage list a b | jq", "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Errorf("real too-many before a pipe should still be arg_count, got: %v", err)
	}
}

// TestArity_NestedOptionalPositionals is the round-7 regression: a Use line
// with NESTED optional positionals (`schedule [<deployment> [<expression>]]`)
// carries TWO optional args, not one.  Before the fix the outer bracket was
// counted as a single optional (max 1), so the valid two-arg form
// `schedule db1 "daily_at 02:00"` was wrongly flagged.
func TestArity_NestedOptionalPositionals(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	root.AddCommand(&cobra.Command{Use: "schedule [<deployment> [<expression>]]", Run: func(_ *cobra.Command, _ []string) {}})
	tree := cmdtree.Walk(root)

	for _, ok := range []string{
		"pg_hardstorage schedule",                      // 0 — fine
		"pg_hardstorage schedule db1",                  // 1 — fine
		`pg_hardstorage schedule db1 "daily_at 02:00"`, // 2 — fine (was flagged)
	} {
		if err := cmdtree.Validate(tree, ok, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid, got: %v", ok, err)
		}
	}
	// 3 positionals is genuinely too many.
	err := cmdtree.Validate(tree, "pg_hardstorage schedule a b c", "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Fatalf("3 positionals should be arg_count, got: %v", err)
	} else if !strings.Contains(ve.Message, "0–2") {
		t.Errorf("bound should read 0–2: %v", ve.Message)
	}
}

// TestArity_FlagValuePlaceholdersNotPositionals is the round-7 regression: a
// Use line that embeds flags with value placeholders
// (`export-bundle --repo <url> --out <path>`) must read as ZERO positionals —
// the <url>/<path> belong to the flags.  Before the fix they were counted as
// two required positionals, so the fully-correct flag form was rejected.
func TestArity_FlagValuePlaceholdersNotPositionals(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	exp := &cobra.Command{Use: "export-bundle --repo <url> --out <path>", Run: func(_ *cobra.Command, _ []string) {}}
	exp.Flags().String("repo", "", "repository URL")
	exp.Flags().String("out", "", "output path")
	root.AddCommand(exp)
	tree := cmdtree.Walk(root)

	if err := cmdtree.Validate(tree, "pg_hardstorage export-bundle --repo file:///r --out /tmp/b.json", "pg_hardstorage"); err != nil {
		t.Errorf("flag-only form must be valid (no phantom positionals): %v", err)
	}
	// A Use line that declares only flags carries no positional bound, so the
	// arity check stays conservative (unbounded) rather than risk a false
	// too-many — the point of the fix is that the correct flag form is NOT
	// rejected, which the assertion above pins.
	if err := cmdtree.Validate(tree, "pg_hardstorage export-bundle --repo file:///r --out /tmp/b.json --repo x", "pg_hardstorage"); err != nil {
		t.Errorf("repeated flag form must still be valid: %v", err)
	}
}

// TestArity_RepoFlagSatisfiesPositional is the round-7 regression for the
// positional-OR-flag commands (repo audit/gc, compliance report, capacity
// preflight): the repo URL may come via the <url>/<repo> positional OR the
// --repo flag.  When --repo is given, the positional must count as satisfied.
func TestArity_RepoFlagSatisfiesPositional(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	audit := &cobra.Command{Use: "audit <url>", Run: func(_ *cobra.Command, _ []string) {}}
	audit.Flags().String("repo", "", "repository URL — positional <url> also accepted")
	root.AddCommand(audit)
	tree := cmdtree.Walk(root)

	// --repo flag form — positional satisfied.
	if err := cmdtree.Validate(tree, "pg_hardstorage audit --repo file:///r", "pg_hardstorage"); err != nil {
		t.Errorf("--repo should satisfy the <url> positional: %v", err)
	}
	// positional form — also valid.
	if err := cmdtree.Validate(tree, "pg_hardstorage audit file:///r", "pg_hardstorage"); err != nil {
		t.Errorf("positional <url> should be valid: %v", err)
	}
	// neither — genuinely missing.
	err := cmdtree.Validate(tree, "pg_hardstorage audit", "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Errorf("no url and no --repo should be arg_count, got: %v", err)
	}
}

// TestArity_InlineCommentNotCounted is the round-8 regression: a trailing
// inline shell comment (`... --set k=v   # silence if last backup < 2h old`)
// must not have its words counted as positional arguments. The model emitted
// exactly this on `notify add`, and the comment's tokens inflated the count
// to "got 6" against the one allowed positional.
func TestArity_InlineCommentNotCounted(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	add := &cobra.Command{Use: "add <plugin> [--set key=value ...]", Run: func(_ *cobra.Command, _ []string) {}}
	add.Flags().StringArray("set", nil, "")
	add.Flags().String("name", "", "")
	notify := &cobra.Command{Use: "notify"}
	notify.AddCommand(add)
	root.AddCommand(notify)
	list := &cobra.Command{Use: "list <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	root.AddCommand(list)
	tree := cmdtree.Walk(root)

	for _, c := range []string{
		`pg_hardstorage notify add pagerduty --name pd --set k=v --set j=w   # silence if last backup < 2h old`,
		`pg_hardstorage list db1 # show all backups`, // space before #
		`pg_hardstorage list db1 #inline-no-space`,   // no space before #
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid (inline comment stripped), got: %v", c, err)
		}
	}
	// A genuine too-many BEFORE the comment is still caught.
	err := cmdtree.Validate(tree, "pg_hardstorage list a b # note", "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Errorf("real too-many before a comment should still be arg_count, got: %v", err)
	}
}

// TestValidate_HelpFlagAlwaysValid is the round-9 regression: `--help` / `-h`
// is valid on every cobra command and short-circuits execution, but the help
// flag is added lazily by cobra so it's absent from the frozen snapshot.
// Without the guard, `agent --help`, `verify --help`, even `--help` itself —
// all common steps in LLM answers — were wrongly flagged "unknown flag" or
// "missing required --repo".
func TestValidate_HelpFlagAlwaysValid(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	agent := &cobra.Command{Use: "agent", Run: func(_ *cobra.Command, _ []string) {}}
	root.AddCommand(agent)
	verify := &cobra.Command{Use: "verify <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	verify.Flags().String("repo", "", "repository URL (required)")
	root.AddCommand(verify)
	tree := cmdtree.Walk(root)

	for _, c := range []string{
		"pg_hardstorage agent --help",
		"pg_hardstorage agent -h",
		"pg_hardstorage --help",
		"pg_hardstorage verify --help", // would otherwise be missing --repo
		"pg_hardstorage verify db1 --help",
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid (help request), got: %v", c, err)
		}
	}
	// A help request on a TYPO'd subcommand is still caught — path resolves
	// before the help short-circuit.
	err := cmdtree.Validate(tree, "pg_hardstorage bogus --help", "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "unknown_command" {
		t.Errorf("bogus --help should still be unknown_command, got: %v", err)
	}
	// Without --help, the missing required flag is still caught.
	if err := cmdtree.Validate(tree, "pg_hardstorage verify db1", "pg_hardstorage"); err == nil {
		t.Error("verify db1 (no --help) should still report missing --repo")
	}
}

// TestArity_OptionalAlternationPositional is a round-10 regression: an
// optional positional written as an alternation of literals —
// `verify <deployment> [latest|<backup-id>]` — is ONE optional slot. The
// round-7 recursion found no bare <...> inside the single `latest|<backup-id>`
// token and counted zero, so `verify db1 latest` (a valid 2-positional
// command) was wrongly flagged "accepts 1, got 2".
func TestArity_OptionalAlternationPositional(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	v := &cobra.Command{Use: "verify <deployment> [latest|<backup-id>]", Run: func(_ *cobra.Command, _ []string) {}}
	v.Flags().String("repo", "", "repository URL (required)")
	root.AddCommand(v)
	tree := cmdtree.Walk(root)

	for _, ok := range []string{
		"pg_hardstorage verify db1 --repo r",        // 1 positional
		"pg_hardstorage verify db1 latest --repo r", // 2 — the literal arm
		"pg_hardstorage verify db1 abc123 --repo r", // 2 — the <backup-id> arm
	} {
		if err := cmdtree.Validate(tree, ok, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid, got: %v", ok, err)
		}
	}
	// 3 is genuinely too many; bound reads 1–2.
	err := cmdtree.Validate(tree, "pg_hardstorage verify db1 a b --repo r", "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "arg_count" {
		t.Fatalf("3 positionals should be arg_count, got: %v", err)
	} else if !strings.Contains(ve.Message, "1–2") {
		t.Errorf("bound should read 1–2: %v", ve.Message)
	}
}

// TestValidate_InlineCommentWithQuote is a round-10 regression: a trailing
// inline comment may contain an apostrophe ("use only when you're sure").
// tokenise must drop the comment at the '#' word boundary — earlier the
// comment was stripped only AFTER tokenising, so the apostrophe was mistaken
// for an opening quote and the whole command failed "unbalanced quote". A
// QUOTED '#' is still a literal argument, never a comment.
func TestValidate_InlineCommentWithQuote(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	list := &cobra.Command{Use: "list <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	list.Flags().String("repo", "", "repository URL")
	root.AddCommand(list)
	root.AddCommand(&cobra.Command{Use: "init <url>", Run: func(_ *cobra.Command, _ []string) {}})
	tree := cmdtree.Walk(root)

	for _, c := range []string{
		`pg_hardstorage list db1 --repo r   # override; use only when you're sure`,
		`pg_hardstorage list db1 --repo r  # don't worry about it`,
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid (comment dropped, no parse error), got: %v", c, err)
		}
	}
	// A QUOTED '#' is a real argument, not a comment — must survive.
	if err := cmdtree.Validate(tree, `pg_hardstorage init '#literal-hash'`, "pg_hardstorage"); err != nil {
		t.Errorf("quoted '#' must be kept as a literal arg, got: %v", err)
	}
	// Mid-word '#' (no preceding space) is a literal too — one positional.
	if err := cmdtree.Validate(tree, "pg_hardstorage init a#b", "pg_hardstorage"); err != nil {
		t.Errorf("mid-word '#' is literal, got: %v", err)
	}
}

// TestValidate_CommandSubstitutionIsOpaque is a round-14 regression: a
// `$(...)` command substitution is an opaque VALUE to the outer command. Its
// internals — an inner `|`, the inner command's own `--flags`, its
// positionals — must not be parsed as the outer command's. Before the fix the
// inner `|` triggered the shell-op truncation and dropped the outer command's
// trailing `--repo`, so a valid one-liner was wrongly flagged "missing --repo".
func TestValidate_CommandSubstitutionIsOpaque(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	verify := &cobra.Command{Use: "verify <deployment> [latest|<backup-id>]", Run: func(_ *cobra.Command, _ []string) {}}
	verify.Flags().String("repo", "", "repository URL (required)")
	root.AddCommand(verify)
	tree := cmdtree.Walk(root)

	// VALID: backup-id comes from a subshell, --repo follows it. The inner
	// `|` and the inner `-o`/`jq` flags belong to the substitution, not verify.
	for _, c := range []string{
		`pg_hardstorage verify mydb $(pg_hardstorage list mydb -o json | jq -r '.[0].id') --repo r`,
		`pg_hardstorage verify mydb $(echo latest) --repo r`,
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid (substitution is opaque), got: %v", c, err)
		}
	}
	// A genuinely-absent --repo (none after the substitution) is still caught.
	err := cmdtree.Validate(tree, `pg_hardstorage verify mydb $(pg_hardstorage list mydb -o json | jq -r .id)`, "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "missing_required" {
		t.Errorf("missing --repo (none outside the substitution) should still be caught, got: %v", err)
	}
	// An unknown OUTER command is still caught even with a substitution arg.
	err = cmdtree.Validate(tree, `pg_hardstorage bogus $(echo x) --repo r`, "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "unknown_command" {
		t.Errorf("unknown outer command should still be caught, got: %v", err)
	}
}

// TestValidate_UnterminatedQuoteAfterPipe is a round-15 regression: a model
// answer often pipes a pg_hardstorage command into a MULTI-LINE quoted jq
// filter; the scrub extractor grabs only the first line, leaving a dangling
// quote in the (discarded) jq tail. tokenise must not fail the whole command
// with "unbalanced quote" — the pg_hardstorage part before the `|` is
// complete and is what we validate.
func TestValidate_UnterminatedQuoteAfterPipe(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	list := &cobra.Command{Use: "list <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	list.Flags().String("repo", "", "repository URL (required)")
	list.Flags().StringP("output", "o", "", "output format")
	root.AddCommand(list)
	status := &cobra.Command{Use: "status [<deployment>]", Run: func(_ *cobra.Command, _ []string) {}}
	status.Flags().String("repo", "", "repository URL (required)")
	status.Flags().StringP("output", "o", "", "output format")
	root.AddCommand(status)
	tree := cmdtree.Walk(root)

	// VALID pre-pipe command, dangling quote only in the discarded jq tail.
	for _, c := range []string{
		`pg_hardstorage list db1 --repo r -o json | jq -r '`,
		`pg_hardstorage list db1 --repo r | jq '.backups[] | select(.x`,
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q valid pre-pipe should pass (tail discarded), got: %v", c, err)
		}
	}
	// The pre-pipe part's OWN problem (missing --repo) is still reported —
	// and as that, not a generic "parse" error.
	err := cmdtree.Validate(tree, `pg_hardstorage status --output json | jq -r '`, "pg_hardstorage")
	if ve, ok := err.(*cmdtree.ValidationError); !ok || ve.Kind != "missing_required" {
		t.Errorf("status (missing --repo) before the pipe should be missing_required, got: %v", err)
	}
}

// TestArity_FdDuplicationRedirectNotCounted is a round-18 regression: a
// file-descriptor duplication redirect (`2>&1`, `1>&2`, `>&2`) renders as one
// token the tokeniser doesn't split, and `2>` alone didn't match it — so
// `backup prod --verbose 2>&1 | grep …` counted `2>&1` as a 2nd positional
// and tripped a false arg_count. fd-dup redirects are now recognised.
func TestArity_FdDuplicationRedirectNotCounted(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	b := &cobra.Command{Use: "backup <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	b.Flags().String("repo", "", "")
	b.Flags().String("pg-connection", "", "")
	root.AddCommand(b)
	init := &cobra.Command{Use: "init <url>", Run: func(_ *cobra.Command, _ []string) {}}
	root.AddCommand(init)
	tree := cmdtree.Walk(root)

	for _, c := range []string{
		`pg_hardstorage backup prod --repo r --pg-connection "host=h" 2>&1 | grep X`,
		`pg_hardstorage backup prod --repo r 1>&2`,
		`pg_hardstorage backup prod --repo r >&2`,
		`pg_hardstorage backup prod --repo r 2> /tmp/err`, // space form, already worked
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should be valid (redirect stripped), got: %v", c, err)
		}
	}
	// A `&` inside a quoted URL is still NOT an operator (regression guard).
	if err := cmdtree.Validate(tree, `pg_hardstorage init 's3://b/?a=1&b=2'`, "pg_hardstorage"); err != nil {
		t.Errorf("quoted-URL ampersand must stay literal: %v", err)
	}
}

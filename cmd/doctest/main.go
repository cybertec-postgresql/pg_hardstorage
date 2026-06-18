// doctest is the markdown-test-runner that executes
// `# RUNNABLE` code blocks from the documentation site
// against a real `pg_hardstorage` binary + a real
// PostgreSQL.  CI uses it to catch tutorial bit-rot — when
// a CLI flag is renamed, the tutorial that uses it fails
// here before a release ships.
//
// # Markup convention
//
// A code block is runnable if its FIRST line (after the
// opening fence) is a comment matching:
//
//	# RUNNABLE [expect-exit=N] [expect-match="..."] [skip-in-ci="reason"] [skip="reason"]
//
// All directives are optional.  Defaults: bash, expect
// exit 0, no skip, no stdout regex.  See
// docs/CONTRIBUTING-DOCS.md for the authoring spec.
//
// # Execution model
//
// Every RUNNABLE block in one markdown file shares ONE
// bash session — `cd`, env vars, and shell state propagate
// across blocks.  Each tutorial gets a fresh per-run
// tempdir and a curated env (REPO_URL, PG_CONNECTION,
// HOME) injected.  Blocks across DIFFERENT tutorials are
// fully isolated.
//
// # Assertions
//
//   - `expect-exit=N` — block must exit with that code.
//     Default: 0.
//   - `expect-match="re"` — block stdout must match the
//     regex (Go syntax).  Default: no match check.
//   - `skip-in-ci="..."` — block is skipped when the
//     PG_HARDSTORAGE_DOCTEST_CI env var is set ("1" /
//     "true").  Use for blocks that need external services
//     CI doesn't have.
//   - `skip="..."` — block is always skipped.  Use for
//     code that's purely illustrative (Go interface defs,
//     YAML config snippets that aren't shell commands,
//     etc.) but happens to live in a bash fence.
//
// # CI vs local
//
// In CI: set `PG_HARDSTORAGE_DOCTEST_CI=1`.  Blocks marked
// `skip-in-ci=...` are skipped; everything else must pass.
//
// Local: omit the env var.  Every block runs.
//
// # Exit codes
//
//   0 — every executed block met its expectations
//   1 — at least one block failed
//   2 — runner-internal error (bad CLI flags, dir not
//       found, etc.)

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	root := flag.String("root", "docs/tutorials", "directory to walk for *.md files")
	pgConn := flag.String("pg-connection", "", "PG connection URL (overrides $PG_HARDSTORAGE_DOCTEST_PG)")
	bin := flag.String("pg-hardstorage-bin", "", "absolute path to pg_hardstorage binary (overrides PATH lookup)")
	timeout := flag.Duration("per-block-timeout", 90*time.Second, "kill a block after this long")
	verbose := flag.Bool("v", false, "print stdout/stderr of every block, even passing ones")
	listOnly := flag.Bool("list", false, "list runnable blocks but don't execute them")
	flag.Parse()

	if err := run(*root, *pgConn, *bin, *timeout, *verbose, *listOnly); err != nil {
		log.Fatalf("doctest: %v", err)
	}
}

func run(root, pgConn, binPath string, timeout time.Duration, verbose, listOnly bool) error {
	if pgConn == "" {
		pgConn = os.Getenv("PG_HARDSTORAGE_DOCTEST_PG")
	}
	ci := truthy(os.Getenv("PG_HARDSTORAGE_DOCTEST_CI"))

	if binPath == "" {
		binPath = os.Getenv("PG_HARDSTORAGE_DOCTEST_BIN")
	}
	if binPath == "" {
		bp, err := exec.LookPath("pg_hardstorage")
		if err != nil {
			// Fall back to ./bin/pg_hardstorage in repo root —
			// the most common dev layout.
			cwd, _ := os.Getwd()
			candidate := filepath.Join(cwd, "bin", "pg_hardstorage")
			if _, e := os.Stat(candidate); e == nil {
				binPath = candidate
			} else {
				return fmt.Errorf("pg_hardstorage not on PATH; pass --pg-hardstorage-bin or run `make build` first")
			}
		} else {
			binPath = bp
		}
	}

	files, err := walk(root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no markdown files under %q", root)
	}

	totalRunnable := 0
	totalRun := 0
	totalSkip := 0
	totalFail := 0
	failures := []failure{}
	t0 := time.Now()

	for _, f := range files {
		blocks, err := extractBlocks(f)
		if err != nil {
			return fmt.Errorf("parse %s: %w", f, err)
		}
		if len(blocks) == 0 {
			continue
		}
		totalRunnable += len(blocks)

		fmt.Printf("==> %s (%d runnable blocks)\n", relPath(f), len(blocks))

		if listOnly {
			for _, b := range blocks {
				fmt.Printf("    L%-4d  exit=%d match=%q skip=%q skip-in-ci=%q\n",
					b.line, b.expectExit, b.expectMatch, b.skip, b.skipInCI)
			}
			continue
		}

		fileFails, ran, skipped := runFile(f, blocks, fileEnv{
			Bin:     binPath,
			PgConn:  pgConn,
			CI:      ci,
			Timeout: timeout,
			Verbose: verbose,
		})
		totalRun += ran
		totalSkip += skipped
		totalFail += len(fileFails)
		failures = append(failures, fileFails...)
	}

	fmt.Println()
	fmt.Printf("doctest summary: %d runnable, %d ran, %d skipped, %d failed in %s\n",
		totalRunnable, totalRun, totalSkip, totalFail, time.Since(t0).Round(time.Millisecond))

	if totalFail > 0 {
		fmt.Println()
		fmt.Println("Failures:")
		for _, f := range failures {
			fmt.Printf("  %s:%d  %s\n", relPath(f.file), f.line, f.reason)
		}
		return fmt.Errorf("%d block(s) failed", totalFail)
	}
	return nil
}

// walk returns every *.md under root, sorted.
func walk(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// relPath returns p relative to cwd if possible, falling
// back to the absolute path otherwise.  Pure cosmetic.
func relPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil {
		return p
	}
	return rel
}

// block is one extracted RUNNABLE code block.
type block struct {
	file        string
	line        int    // 1-based line number of the directive
	body        string // the script body (without the directive line)
	expectExit  int
	expectMatch string // empty = no match check
	skip        string // empty = don't skip
	skipInCI    string // empty = don't skip in CI
}

var (
	fenceRE     = regexp.MustCompile("^```(bash|sh|console)$")
	directiveRE = regexp.MustCompile(`^# RUNNABLE\b(.*)$`)
)

// extractBlocks returns every RUNNABLE block from path.
// Lightweight markdown scan — doesn't try to be CommonMark
// compliant; tutorials use plain triple-backtick fences and
// that's all we need to recognise.
func extractBlocks(path string) ([]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	var (
		out        []block
		inFence    bool
		fenceLang  string
		bodyLines  []string
		firstLine  = true
		currentBlk block
		startLine  int
	)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if !inFence {
			m := fenceRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			inFence = true
			fenceLang = m[1]
			bodyLines = nil
			firstLine = true
			startLine = lineNum
			continue
		}
		// inFence
		if line == "```" {
			// Block close.  If it was RUNNABLE, emit it.
			if currentBlk.line != 0 {
				currentBlk.body = strings.Join(bodyLines, "\n")
				out = append(out, currentBlk)
			}
			inFence = false
			fenceLang = ""
			currentBlk = block{}
			bodyLines = nil
			continue
		}
		if firstLine {
			firstLine = false
			// Only `bash` / `sh` blocks are candidates;
			// `console` is output-only.
			if fenceLang == "console" {
				bodyLines = append(bodyLines, line)
				continue
			}
			m := directiveRE.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				bodyLines = append(bodyLines, line)
				continue
			}
			// We have a RUNNABLE directive.
			b, err := parseDirective(path, startLine+1, m[1])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNum, err)
			}
			currentBlk = b
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseDirective parses the trailing directive args from
// the `# RUNNABLE ...` comment line.
func parseDirective(file string, line int, args string) (block, error) {
	b := block{file: file, line: line, expectExit: 0}
	args = strings.TrimSpace(args)
	if args == "" {
		return b, nil
	}
	// Tokenise on whitespace, but keep quoted strings intact.
	tokens, err := tokenise(args)
	if err != nil {
		return block{}, fmt.Errorf("RUNNABLE directive: %w", err)
	}
	for _, tok := range tokens {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			return block{}, fmt.Errorf("RUNNABLE directive %q: expected key=value", tok)
		}
		val = strings.Trim(val, `"`)
		switch key {
		case "expect-exit":
			n, err := strconv.Atoi(val)
			if err != nil {
				return block{}, fmt.Errorf("expect-exit=%q: %w", val, err)
			}
			b.expectExit = n
		case "expect-match":
			b.expectMatch = val
		case "skip":
			b.skip = val
			if b.skip == "" {
				b.skip = "no reason given"
			}
		case "skip-in-ci":
			b.skipInCI = val
			if b.skipInCI == "" {
				b.skipInCI = "no reason given"
			}
		default:
			return block{}, fmt.Errorf("RUNNABLE directive: unknown key %q (want expect-exit | expect-match | skip | skip-in-ci)", key)
		}
	}
	return b, nil
}

// tokenise splits s on whitespace, honouring "..."-quoted
// segments.  No backslash escapes; if you need a literal "
// in a value, file an issue.
func tokenise(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			cur.WriteByte(c)
			inQuote = !inQuote
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unclosed quote in %q", s)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// fileEnv carries the per-run knobs into runFile.
type fileEnv struct {
	Bin     string
	PgConn  string
	CI      bool
	Timeout time.Duration
	Verbose bool
}

// failure is one block that didn't meet expectations.
type failure struct {
	file   string
	line   int
	reason string
}

// runFile executes every RUNNABLE block in one markdown
// file.  All blocks share a single bash invocation so
// `cd`, env vars, and shell state propagate across them
// (mimics how an operator would copy-paste the tutorial in
// sequence).  Returns failures + ran/skipped counts.
func runFile(file string, blocks []block, env fileEnv) ([]failure, int, int) {
	tmpdir, err := os.MkdirTemp("", "doctest-*")
	if err != nil {
		fmt.Printf("    ERR  %v\n", err)
		return []failure{{file: file, line: 0, reason: err.Error()}}, 0, 0
	}
	defer os.RemoveAll(tmpdir)

	// Write a single shell script per file.  Each block is
	// run as a group command `{ … }` (not a subshell) so env
	// vars and shell state set in one block (e.g.
	// `BACKUP_ID=$(...)`) propagate to later blocks — the
	// header doc promises a shared bash session and tutorials
	// rely on it.  set -e is forced off at script top so a
	// block-internal failure can't abort the whole file; per-
	// block exit codes go to block.<key>.exit.
	script, blockKeys := buildScript(blocks, tmpdir, env)
	scriptPath := filepath.Join(tmpdir, "doctest.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return []failure{{file: file, line: 0, reason: err.Error()}}, 0, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), env.Timeout*time.Duration(len(blocks)+1))
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"DOCTEST_TMPDIR="+tmpdir,
		"PG_HARDSTORAGE_BIN="+env.Bin,
		"PG_CONNECTION="+env.PgConn,
		"HOME="+tmpdir,
		"XDG_CONFIG_HOME="+tmpdir+"/.config",
		"XDG_STATE_HOME="+tmpdir+"/.state",
		"XDG_DATA_HOME="+tmpdir+"/.data",
	)
	combined := &bytesWriter{}
	cmd.Stdout = combined
	cmd.Stderr = combined
	_ = cmd.Run()

	// Read per-block result files.
	var fails []failure
	ran, skipped := 0, 0
	for _, b := range blocks {
		key := blockKeys[b.line]
		if reason := shouldSkip(b, env.CI); reason != "" {
			fmt.Printf("    L%-4d  SKIP  %s\n", b.line, reason)
			skipped++
			continue
		}
		exitPath := filepath.Join(tmpdir, "block."+key+".exit")
		stdoutPath := filepath.Join(tmpdir, "block."+key+".stdout")
		stderrPath := filepath.Join(tmpdir, "block."+key+".stderr")
		// v29 audit fix: detect missing result files explicitly.
		// Pre-fix, a bash crash (script syntax error, fork
		// failure, OOM) left no per-block files; os.ReadFile
		// returned (nil, err); strconv.Atoi("") returned (0,
		// err) which we discarded; exit==0 matched the default
		// expectExit==0; every block falsely reported PASS.
		exitBytes, exitReadErr := os.ReadFile(exitPath)
		stdoutBytes, _ := os.ReadFile(stdoutPath)
		stderrBytes, _ := os.ReadFile(stderrPath)

		ran++
		var reason string
		switch {
		case exitReadErr != nil:
			reason = fmt.Sprintf("no result file (bash itself failed?): %v", exitReadErr)
		case strings.TrimSpace(string(exitBytes)) == "skipped":
			// Block was deliberately skipped by buildScript
			// (skip / skip-in-ci honoured at script-emit time).
			// shouldSkip() above will already have logged + bumped
			// the skipped counter, but this branch can be reached
			// if a future buildScript change emits "skipped"
			// without runFile noticing — surface it explicitly.
			reason = "" // treat as already-handled
		default:
			exit, atoiErr := strconv.Atoi(strings.TrimSpace(string(exitBytes)))
			if atoiErr != nil {
				reason = fmt.Sprintf("malformed exit code in result file: %q", string(exitBytes))
			} else if exit != b.expectExit {
				reason = fmt.Sprintf("exit=%d, want %d", exit, b.expectExit)
			}
		}

		if reason == "" && b.expectMatch != "" {
			re, err := regexp.Compile(b.expectMatch)
			if err != nil {
				reason = fmt.Sprintf("expect-match regex invalid: %v", err)
			} else if !re.Match(stdoutBytes) {
				reason = fmt.Sprintf("stdout did not match %q", b.expectMatch)
			}
		}
		if reason == "" {
			fmt.Printf("    L%-4d  ok\n", b.line)
			if env.Verbose && len(stdoutBytes)+len(stderrBytes) > 0 {
				fmt.Printf("        stdout: %s\n", oneline(string(stdoutBytes)))
				if len(stderrBytes) > 0 {
					fmt.Printf("        stderr: %s\n", oneline(string(stderrBytes)))
				}
			}
			continue
		}
		fmt.Printf("    L%-4d  FAIL  %s\n", b.line, reason)
		// On failure, dump full stdout/stderr (indented for
		// scannability) — truncating to oneline() hid the
		// actual cause and forced re-running locally to
		// diagnose every CI failure.
		if len(stdoutBytes) > 0 {
			fmt.Printf("        stdout:\n%s\n", indent(string(stdoutBytes), "          "))
		}
		if len(stderrBytes) > 0 {
			fmt.Printf("        stderr:\n%s\n", indent(string(stderrBytes), "          "))
		}
		fails = append(fails, failure{file: file, line: b.line, reason: reason})
	}
	return fails, ran, skipped
}

func shouldSkip(b block, ci bool) string {
	if b.skip != "" {
		return "skip: " + b.skip
	}
	if ci && b.skipInCI != "" {
		return "skip-in-ci: " + b.skipInCI
	}
	return ""
}

// buildScript produces the per-file bash script that runs
// every block, recording per-block exit code + stdout +
// stderr into files named by block key.  Returns the
// script body and a line→key map for result lookup.
func buildScript(blocks []block, tmpdir string, env fileEnv) (string, map[int]string) {
	var b strings.Builder
	keys := map[int]string{}
	b.WriteString("#!/usr/bin/env bash\n")
	// Don't `set -e` at the top; we want every block to run
	// regardless, and we capture each block's exit
	// individually.  Subshells per-block contain failures.
	b.WriteString("set +e\n")
	// Make the binary discoverable on PATH.  Tutorials say
	// `pg_hardstorage backup …` not `${PG_HARDSTORAGE_BIN}
	// backup …`; the runner injects the binary's directory
	// at the front of PATH so the bare name resolves.
	b.WriteString("export PATH=\"$(dirname \"$PG_HARDSTORAGE_BIN\"):$PATH\"\n")
	b.WriteString("cd \"$DOCTEST_TMPDIR\"\n")
	b.WriteString("mkdir -p .config .state .data\n")
	// Tutorials commonly use literal /tmp/hs-<name>-repo
	// paths so an operator copy-pastes a self-contained
	// command.  Before each tutorial we wipe those prefixes
	// so consecutive runs are deterministic — exit 7
	// (conflict.repo_exists) is exactly what `repo init`
	// returns when the repo already exists, and we don't
	// want that to be a CI flake from a leftover tmp dir.
	b.WriteString("rm -rf /tmp/hs-tutorial-* /tmp/hs-encrypt-* /tmp/hs-pitr-* /tmp/hs-restored-* 2>/dev/null || true\n")
	b.WriteString("\n")
	for i, blk := range blocks {
		key := fmt.Sprintf("%03d", i)
		keys[blk.line] = key
		// v29 audit fix: skipped blocks must not execute.  The
		// previous loop emitted shell for every block regardless
		// of skip-in-ci, then ignored their result files in
		// runFile — but the shell still ran the body, burning CI
		// minutes and triggering side effects (e.g. an indefinite
		// `wal stream` would block the runner).  Now we emit a
		// no-op marker for skipped blocks so the runFile code can
		// still find the result files but they correctly indicate
		// "this was skipped, did not execute."
		if reason := shouldSkip(blk, env.CI); reason != "" {
			fmt.Fprintf(&b, "# === block %d (line %d) — SKIPPED: %s ===\n", i, blk.line, reason)
			fmt.Fprintf(&b, ": >block.%s.stdout\n", key)
			fmt.Fprintf(&b, ": >block.%s.stderr\n", key)
			fmt.Fprintf(&b, "echo skipped > block.%s.exit\n\n", key)
			continue
		}
		fmt.Fprintf(&b, "# === block %d (line %d) ===\n", i, blk.line)
		fmt.Fprintf(&b, "{\n%s\n} >block.%s.stdout 2>block.%s.stderr\n", blk.body, key, key)
		fmt.Fprintf(&b, "echo $? > block.%s.exit\n\n", key)
	}
	return b.String(), keys
}

// truthy interprets common boolean-ish env-var values.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// bytesWriter discards everything; we read per-block
// stdout/stderr from the on-disk files instead.
type bytesWriter struct{}

// Write implements io.Writer by discarding p and reporting full success.
func (bytesWriter) Write(p []byte) (int, error) { return len(p), nil }

// indent prefixes every line of s with prefix.  Used on the
// failure path so the dumped stdout/stderr stays visually
// distinct from the doctest log lines around it.
func indent(s, prefix string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

// oneline truncates s to the first line + a length cap so
// terminal output stays scannable on big outputs.
func oneline(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 200 {
		s = s[:200] + " …"
	}
	return s
}

// silence "imported and not used" while developing.
var _ = io.Copy

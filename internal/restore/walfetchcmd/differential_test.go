package walfetchcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuild_DifferentialAgainstRealShell is the adversarial quoting test:
// Build's output IS a shell script (PG runs it via system() → /bin/sh -c).
// We run it through a REAL /bin/sh with a stub "agent" that records its argv,
// and assert the agent receives `wal fetch <dep> <%f> <%p> --repo <url>` with
// every argument byte-for-byte intact — even when the repo URL carries shell
// metacharacters (& ; | $ ` ' space) that, if mis-quoted, would background a
// process, run a subcommand, expand a variable, or split the URL into pieces.
// This is the exact failure mode the package docstring warns about (an `&` in
// an S3 query string turning the wrapper into a no-op).
func TestBuild_DifferentialAgainstRealShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh available")
	}

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	// Neutral name (NOT pg_hardstorage_simple) so normalizeAgentBin leaves it.
	stub := filepath.Join(dir, "agent-stub")
	// Record each received arg on its own line; the args under test contain
	// no newlines. ${STUB_EXIT} lets the exit-code test reuse the same stub.
	script := "#!/bin/sh\n: > \"$ARGV_OUT\"\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$ARGV_OUT\"; done\nexit ${STUB_EXIT:-0}\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		dep     = "noplay"
		walName = "000000010000000000000048"
		dest    = "/var/lib/postgresql/pg_wal/RECOVERYXLOG"
	)
	cases := []struct{ name, repoURL string }{
		{"s3 query ampersands", "s3://bucket/prefix?region=us-east-1&endpoint=http://h:9000&path_style=true"},
		{"dollar must not expand", "s3://b/p?token=$SECRET_DO_NOT_EXPAND"},
		{"backtick must not execute", "s3://b/p?x=`whoami`"},
		{"command sub must not run", "s3://b/p?x=$(whoami)"},
		{"single quote in url", "s3://b/p?filter=name='x'"},
		{"space in file path", "file:///tmp/my repo/data"},
		{"semicolon injection", "s3://b/p?x=a;touch /tmp/pwn_should_not_exist"},
		{"pipe injection", "s3://b/p?x=a|touch /tmp/pwn2_should_not_exist"},
		{"plain file url", "file:///srv/repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := Build(stub, dep, c.repoURL)
			// PG substitutes %f / %p literally before calling system().
			rc = strings.ReplaceAll(rc, "%f", walName)
			rc = strings.ReplaceAll(rc, "%p", dest)

			cmd := exec.Command(sh, "-c", rc)
			cmd.Env = append(os.Environ(),
				"ARGV_OUT="+argvFile,
				"STUB_EXIT=0",
				"SECRET_DO_NOT_EXPAND=LEAKED", // proves $-expansion didn't happen
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("shell run failed: %v\noutput=%s\nrestore_command=%s", err, out, rc)
			}

			raw, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			want := []string{"wal", "fetch", dep, walName, dest, "--repo", c.repoURL}
			if len(argv) != len(want) {
				t.Fatalf("agent saw %d args, want %d\n got=%q\nwant=%q\nrestore_command=%s",
					len(argv), len(want), argv, want, rc)
			}
			for i := range want {
				if argv[i] != want[i] {
					t.Errorf("argv[%d] = %q, want %q\nrestore_command=%s", i, argv[i], want[i], rc)
				}
			}
		})
	}

	// Injection guard: the metacharacter cases must NOT have spawned side
	// effects (a backgrounded `touch`, an executed subcommand).
	for _, p := range []string{"/tmp/pwn_should_not_exist", "/tmp/pwn2_should_not_exist"} {
		if _, err := os.Stat(p); err == nil {
			os.Remove(p)
			t.Errorf("INJECTION: %s was created — the repo URL was not safely quoted", p)
		}
	}
}

// TestBuild_ExitCodeMappingThroughRealShell verifies the v1 exit-code contract
// end-to-end through a real shell: the agent's exit 6 (ExitNotFound, normal
// end-of-archive) is remapped to 1 (PG's "stop & promote"), and every other
// code passes through unchanged so a real failure still surfaces as a crash.
func TestBuild_ExitCodeMappingThroughRealShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh available")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "agent-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit ${STUB_EXIT:-0}\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := Build(stub, "dep", "file:///srv/repo")
	rc = strings.ReplaceAll(rc, "%f", "seg")
	rc = strings.ReplaceAll(rc, "%p", "/dest")

	cases := []struct{ agentExit, wantExit int }{
		{0, 0}, // archived segment found → success
		{6, 1}, // ExitNotFound (end-of-archive) → PG "stop & promote"
		{8, 8}, // storage.unreachable → real crash, pass through
		{1, 1}, // misc failure → pass through
		{2, 2}, // misuse → pass through
	}
	for _, c := range cases {
		cmd := exec.Command(sh, "-c", rc)
		cmd.Env = append(os.Environ(), "STUB_EXIT="+itoa(c.agentExit))
		err := cmd.Run()
		got := 0
		if cmd.ProcessState != nil {
			got = cmd.ProcessState.ExitCode()
		} else if err != nil {
			t.Fatalf("agentExit=%d: run error with no ProcessState: %v", c.agentExit, err)
		}
		if got != c.wantExit {
			t.Errorf("agent exit %d → restore_command exit %d, want %d", c.agentExit, got, c.wantExit)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

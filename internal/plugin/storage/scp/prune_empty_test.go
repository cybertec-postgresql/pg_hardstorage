//go:build !windows

package scp

// prune_empty_test.go — PruneEmptyDirs and the writer-side retries that
// make it safe, against the in-process exec server (the local /bin/sh on
// a real filesystem; no docker). The containerised binding runs the same
// contract against BusyBox ash + GNU find.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
)

func openInproc(t *testing.T) *Plugin {
	t.Helper()
	srv := startExecServer(t)
	return openExecPlugin(t, srv.addr, srv.hostPub)
}

func TestPruneEmptyDirs_Contract(t *testing.T) {
	p := openInproc(t)
	contract.PruneEmptyDirs(t, p)
	if _, err := os.Stat(p.root); err != nil {
		t.Fatalf("repo root after pruning it: %v", err)
	}
}

// Writers racing the prune must never fail, or lose an object, because
// their directory vanished (issue #89).
func TestPruneEmptyDirs_RacesWriters(t *testing.T) {
	iters := 100
	if testing.Short() {
		iters = 25
	}
	contract.PruneRacesWriters(t, openInproc(t), 4, iters)
}

// racyMkdir puts a `mkdir` first on PATH that loses the race against a
// prune on purpose: each of its first `vanish` calls creates the
// directory and removes it again before returning — the window the
// retry loop in stageCommand / linkCommand exists for, held open. It
// returns the environment to run a command under and a counter of
// mkdir calls.
func racyMkdir(t *testing.T, vanish int) (env []string, calls func() int) {
	t.Helper()
	realMkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skip("no mkdir on PATH")
	}
	realRmdir, err := exec.LookPath("rmdir")
	if err != nil {
		t.Skip("no rmdir on PATH")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"n=$(cat " + shellQuote(count) + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1))\n" +
		"echo \"$n\" > " + shellQuote(count) + "\n" +
		shellQuote(realMkdir) + " \"$@\" || exit $?\n" +
		"if [ \"$n\" -le " + strconv.Itoa(vanish) + " ]; then\n" +
		"  for last; do :; done\n" +
		"  " + shellQuote(realRmdir) + " \"$last\"\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(dir, "mkdir"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	calls = func() int {
		b, err := os.ReadFile(count)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	return env, calls
}

// runRemote runs a remote command line the way the exec server does.
func runRemote(env []string, command, stdin string) (stderr string, err error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var eb bytes.Buffer
	cmd.Stderr = &eb
	err = cmd.Run()
	return eb.String(), err
}

// The staging file's directory vanishes between the mkdir and the
// create, three times over; the upload still lands, complete.
func TestStageCommand_RetriesVanishedDir(t *testing.T) {
	env, calls := racyMkdir(t, 3)
	tmp := filepath.Join(t.TempDir(), "chunks/sha256/a b/it's/obj.hstmp-0011223344556677")
	if stderr, err := runRemote(env, stageCommand(tmp), "payload"); err != nil {
		t.Fatalf("stage: %v (stderr: %s)", err, stderr)
	}
	if b, err := os.ReadFile(tmp); err != nil || string(b) != "payload" {
		t.Fatalf("staged file: %q, %v", b, err)
	}
	if got := calls(); got != 4 {
		t.Errorf("mkdir ran %d times, want 4 (three lost races, then success)", got)
	}
}

// A failure that is not a lost race ends the loop — bounded — and comes
// back with the command's own words, without touching stdin's consumer.
func TestStageCommand_RealFailureSurfaces(t *testing.T) {
	env, calls := racyMkdir(t, 0)
	dir := t.TempDir()
	// A regular file where the directory should be.
	if err := os.WriteFile(filepath.Join(dir, "chunks"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, err := runRemote(env, stageCommand(filepath.Join(dir, "chunks/aa/obj.hstmp-0")), "payload")
	if err == nil {
		t.Fatal("staging under a regular file succeeded")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("failure carries no error text from the remote command")
	}
	if got := calls(); got != pruneRaceRetries {
		t.Errorf("mkdir ran %d times, want exactly pruneRaceRetries=%d", got, pruneRaceRetries)
	}
}

func TestLinkCommand_RetriesVanishedDir(t *testing.T) {
	env, calls := racyMkdir(t, 3)
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "manifests/other dir/dst")
	if stderr, err := runRemote(env, linkCommand(src, dst), ""); err != nil {
		t.Fatalf("link: %v (stderr: %s)", err, stderr)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "m" {
		t.Fatalf("dst: %q, %v", b, err)
	}
	if got := calls(); got != 4 {
		t.Errorf("mkdir ran %d times, want 4 (three lost races, then success)", got)
	}
}

// "dst exists" and "src is gone" are answers, not lost races: one
// attempt, and the single-winner signal ("File exists") intact.
func TestLinkCommand_RealAnswersAreNotRetried(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, f := range []string{src, dst} {
		if err := os.WriteFile(f, []byte(filepath.Base(f)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	env, calls := racyMkdir(t, 0)
	env = append(env, "LC_ALL=C")
	stderr, err := runRemote(env, linkCommand(src, dst), "")
	if err == nil || !strings.Contains(stderr, "File exists") {
		t.Fatalf("link onto an existing dst: err=%v stderr=%q, want a File exists failure", err, stderr)
	}
	if got := calls(); got != 1 {
		t.Errorf("existing dst: %d attempts, want 1", got)
	}
	if b, _ := os.ReadFile(dst); string(b) != "dst" {
		t.Fatalf("existing dst was replaced: %q", b)
	}

	env, calls = racyMkdir(t, 0)
	if _, err := runRemote(env, linkCommand(filepath.Join(dir, "nosrc"), filepath.Join(dir, "new")), ""); err == nil {
		t.Fatal("link of a missing src succeeded")
	}
	if got := calls(); got != 1 {
		t.Errorf("missing src: %d attempts, want 1", got)
	}
}

// The prune removes with rmdir(1) only, and hands paths over as
// arguments, quoted exactly once.
func TestPruneEmptyDirsCommand_Shape(t *testing.T) {
	const hostile = "/srv/re po/it's $(touch pwned)"
	for _, self := range []bool{false, true} {
		cmd := pruneEmptyDirsCommand(hostile, self)
		if !strings.Contains(cmd, `-exec rmdir -- {} \;`) {
			t.Errorf("includeSelf=%v: directories are not removed by rmdir: %s", self, cmd)
		}
		for _, bad := range []string{"rm -", "-delete", "xargs", "sh -c"} {
			if strings.Contains(cmd, bad) {
				t.Errorf("includeSelf=%v: command contains %q: %s", self, bad, cmd)
			}
		}
		if got := strings.Count(cmd, shellQuote(hostile)); got != 2 {
			t.Errorf("includeSelf=%v: path quoted %d times, want 2 (test + find): %s", self, got, cmd)
		}
		if strings.Contains(strings.ReplaceAll(cmd, shellQuote(hostile), ""), "pwned") {
			t.Errorf("includeSelf=%v: path appears unquoted: %s", self, cmd)
		}
		if self == strings.Contains(cmd, "-mindepth 1") {
			t.Errorf("includeSelf=%v: wrong -mindepth: %s", self, cmd)
		}
	}
}

// Symlinks below the start point are content, never followed — the
// target may lie outside the repo. A symlinked start point (chunks/ on
// its own volume) is followed, as the fs backend does.
func TestPruneEmptyDirs_Symlinks(t *testing.T) {
	p := openInproc(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(outside, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.root, "chunks/linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.root, "chunks/linked/out")); err != nil {
		t.Fatal(err)
	}
	n, err := p.PruneEmptyDirs(ctx, "chunks", true)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0 and no error", n, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "empty")); err != nil {
		t.Fatalf("directory outside the repo was pruned through a symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(p.root, "chunks/linked/out")); err != nil {
		t.Fatalf("symlink removed: %v", err)
	}

	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(filepath.Join(volume, "sha256/aa/bb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(volume, filepath.Join(p.root, "wal")); err != nil {
		t.Fatal(err)
	}
	n, err = p.PruneEmptyDirs(ctx, "wal", true)
	if err != nil || n != 3 {
		t.Fatalf("symlinked start point: n=%d err=%v, want 3 and no error", n, err)
	}
	if _, err := os.Stat(volume); err != nil {
		t.Fatalf("the start point's target itself must stay (it is reached through a symlink): %v", err)
	}
}

// What find cannot read is an error for the caller (gc reports it as a
// warning) — only lost races are swallowed.
func TestPruneEmptyDirs_RealErrorSurfaces(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	p := openInproc(t)
	locked := filepath.Join(p.root, "chunks/locked")
	if err := os.MkdirAll(filepath.Join(locked, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.root, "chunks/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	n, err := p.PruneEmptyDirs(context.Background(), "chunks", false)
	if err == nil {
		t.Fatal("an unreadable directory went unreported")
	}
	if n != 1 {
		t.Errorf("removed %d directories, want 1: the count still reports what was pruned before the error", n)
	}
}

func TestIsPruneRace(t *testing.T) {
	exit := &ssh.ExitError{}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"vanished mid-walk", &remoteCmdError{err: exit,
			stderr: "find: '/r/chunks/sha256/aa': No such file or directory"}, true},
		{"filled before rmdir, and vanished", &remoteCmdError{err: exit,
			stderr: "rmdir: failed to remove '/r/a': Directory not empty\nfind: '/r/b': No such file or directory\n"}, true},
		{"wrapped", fmt.Errorf("outer: %w", &remoteCmdError{err: exit,
			stderr: "find: '/r/b': No such file or directory"}), true},
		{"permission denied", &remoteCmdError{err: exit,
			stderr: "find: '/r/locked': Permission denied"}, false},
		{"race beside a real error", &remoteCmdError{err: exit,
			stderr: "find: '/r/b': No such file or directory\nfind: '/r/locked': Permission denied"}, false},
		{"no find at all", &remoteCmdError{err: exit, stderr: "sh: 1: find: not found"}, false},
		{"silent failure", &remoteCmdError{err: exit, stderr: ""}, false},
		{"not an exit status", &remoteCmdError{err: errors.New("EOF"),
			stderr: "find: '/r/b': No such file or directory"}, false},
		{"transport", errors.New("scp: new session: EOF"), false},
		{"nil", nil, false},
	} {
		if got := isPruneRace(c.err); got != c.want {
			t.Errorf("%s: isPruneRace = %v, want %v", c.name, got, c.want)
		}
	}
}

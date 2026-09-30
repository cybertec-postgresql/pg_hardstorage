package cli_test

// json_contract_test.go — the `-o json` stdout contract, enforced across
// the whole command tree.
//
// The contract (docs/reference/output-event-schema.md, "Stdout
// contract"): under `-o json` — also the default whenever stdout is not a
// terminal — stdout carries at most ONE JSON document, the command's
// Result. Streaming events (start-up warnings, progress, per-file
// verbose lines) go to stderr as one JSON object per line. A second
// document on stdout breaks `cmd | jq`, and every command can emit
// start-up events, so this was never a per-command concern: one command
// that renders an event to stdout breaks every script built on it.
//
// The guard is deliberately structural. It walks the live cobra tree, so
// a command added tomorrow is covered without anyone remembering to list
// it, and it plants a config whose sink cannot be built — guaranteeing
// every invocation emits a warning event during start-up, the exact
// traffic that used to leak onto stdout.

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// contractSkip lists leaf commands the no-argument sweep does not run,
// each with the reason. Keep it short: every entry is a command whose
// JSON contract is unguarded.
var contractSkip = map[string]string{
	// Long-running servers / followers: with a valid config they block
	// until signalled (their error paths are covered by the curated
	// invocations below where reachable offline).
	"pg_hardstorage agent":                 "long-running daemon",
	"pg_hardstorage llm":                   "interactive chat on stdin; MCP mode has its own test",
	"pg_hardstorage wal stream":            "long-running replication follower",
	"pg_hardstorage wal receive":           "long-running replication follower",
	"pg_hardstorage control-plane":         "long-running server",
	"pg_hardstorage serve":                 "long-running server",
	"pg_hardstorage verify-runner":         "long-running worker",
	"pg_hardstorage demo":                  "starts a PostgreSQL container",
	"pg_hardstorage gameday run":           "starts containers",
	"pg_hardstorage completion":            "shell script by design, not a Result",
	"pg_hardstorage completion bash":       "shell script by design, not a Result",
	"pg_hardstorage completion zsh":        "shell script by design, not a Result",
	"pg_hardstorage completion fish":       "shell script by design, not a Result",
	"pg_hardstorage completion powershell": "shell script by design, not a Result",
	"pg_hardstorage help":                  "cobra help text",
	"pg_hardstorage __dump-cmd-tree":       "hidden developer dump of the command tree",
}

// contractWorld is an offline sandbox: HOME under a temp dir, a config
// with one deployment pointing at an initialised file:// repo, and a sink
// that cannot be built so start-up always emits an event.
type contractWorld struct {
	*readWorld
	configFile string
}

func newContractWorld(t *testing.T) *contractWorld {
	t.Helper()
	w := newReadWorld(t)
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "")
	t.Setenv("HSPLUGIN_PATH", "")
	if err := os.MkdirAll(w.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: "host=127.0.0.1 port=1 dbname=postgres user=backup password=hunter2 connect_timeout=1"
    repo: %s
sinks:
  - name: broken
    plugin: no-such-sink-plugin
`, w.repoURL)
	path := filepath.Join(w.configDir, "pg_hardstorage.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &contractWorld{readWorld: w, configFile: path}
}

// runBounded runs args with an empty stdin and a deadline; ok=false when
// the command did not return in time.
func runBounded(t *testing.T, stdin string, timeout time.Duration, args ...string) (stdout, stderr string, exit int, ok bool) {
	t.Helper()
	root := cli.NewRoot()
	var out, errb syncBuffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	// Run pre-parses os.Args (as production does) to find -o for errors
	// raised before the dispatcher exists; mirror the real process.
	savedArgs := os.Args
	os.Args = append([]string{"pg_hardstorage"}, args...)
	defer func() { os.Args = savedArgs }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	root.SetContext(ctx)
	done := make(chan int, 1)
	go func() { done <- cli.Run(root) }()
	select {
	case exit = <-done:
		return out.String(), errb.String(), exit, true
	case <-time.After(timeout + 10*time.Second):
		return out.String(), errb.String(), -1, false
	}
}

// jsonValues decodes a stream of concatenated JSON values. It returns the
// number decoded and an error if anything other than JSON (and
// whitespace) is present.
func jsonValues(s string) (int, error) {
	dec := stdjson.NewDecoder(strings.NewReader(s))
	n := 0
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("after %d JSON value(s): %w", n, err)
		}
		n++
	}
}

// assertJSONContract checks one invocation against the contract:
// stdout is at most one JSON document (exactly one on success), and
// stderr — events and the error Result — is a stream of JSON values.
func assertJSONContract(t *testing.T, label, stdout, stderr string, exit int) {
	t.Helper()
	n, err := jsonValues(stdout)
	switch {
	case err != nil:
		t.Errorf("%s: stdout is not JSON (exit %d): %v\nstdout:\n%s", label, exit, err, clip(stdout))
	case n > 1:
		t.Errorf("%s: stdout carries %d JSON documents, want at most 1 (exit %d)\nstdout:\n%s", label, n, exit, clip(stdout))
	case exit == 0 && n == 0:
		t.Errorf("%s: exit 0 but stdout carries no Result document\nstderr:\n%s", label, clip(stderr))
	}
	if _, err := jsonValues(stderr); err != nil {
		t.Errorf("%s: stderr is not a JSON stream under -o json (exit %d): %v\nstderr:\n%s", label, exit, err, clip(stderr))
	}
}

func clip(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "…"
	}
	return s
}

// leafCommands returns every runnable command path in the tree.
func leafCommands(c *cobra.Command) [][]string {
	var out [][]string
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		if c.Runnable() && len(path) > 0 {
			out = append(out, append([]string(nil), path...))
		}
		for _, sub := range c.Commands() {
			walk(sub, append(path, sub.Name()))
		}
	}
	walk(c, nil)
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], " ") < strings.Join(out[j], " ") })
	return out
}

// isPureGroup reports a group command whose only RunE is the synthetic
// unknown-subcommand guard (root.go hardenGroupCommands).
func isPureGroup(c *cobra.Command) bool {
	return c != nil && c.Annotations["pg_hardstorage.group_guard"] == "1"
}

func cmdAt(root *cobra.Command, path []string) *cobra.Command {
	c, _, err := root.Find(path)
	if err != nil {
		return nil
	}
	return c
}

// TestJSONContract_EveryCommandNoArgs runs every leaf command with no
// positional arguments under -o json. Most land on a usage or not-found
// error; all of them go through start-up, which emits an event.
func TestJSONContract_EveryCommandNoArgs(t *testing.T) {
	if testing.Short() {
		t.Skip("sweeps the whole command tree")
	}
	newContractWorld(t)
	root := cli.NewRoot()
	for _, path := range leafCommands(root) {
		key := "pg_hardstorage " + strings.Join(path, " ")
		if _, skip := contractSkip[key]; skip {
			continue
		}
		args := append([]string(nil), path...)
		if isPureGroup(cmdAt(root, path)) {
			// A bare group prints its help (exit 0) by convention —
			// help is not a Result. Drive its error path instead.
			args = append(args, "no-such-subcommand")
		}
		args = append(args, "-o", "json")
		stdout, stderr, exit, ok := runBounded(t, "", 5*time.Second, args...)
		if !ok {
			t.Errorf("%s: did not return within its deadline", key)
			continue
		}
		assertJSONContract(t, key, stdout, stderr, exit)
	}
}

// TestJSONContract_CuratedInvocations runs commands that succeed offline
// (or fail past argument parsing) against the sandbox repo, so the
// success-path Result and the command-body events are covered too.
func TestJSONContract_CuratedInvocations(t *testing.T) {
	w := newContractWorld(t)
	w.commitManifest(t, "db1", 1)
	repo := w.repoURL
	cases := [][]string{
		{"version"},
		{"status"},
		{"list", "db1", "--repo", repo},
		{"list", "nosuch", "--repo", repo},
		{"show", "db1", "--repo", repo},
		{"deployment", "list"},
		{"lint"},
		{"glossary"},
		{"changelog"},
		{"doctor"},
		{"plugin", "list"},
		{"repo", "usage", "--repo", repo},
		{"repo", "check", "--repo", repo},
		{"repo", "gc", "--repo", repo},
		{"wal", "list", "db1", "--repo", repo},
		{"hold", "list", "--repo", repo},
		{"verify", "db1", "--repo", repo},
		{"backup", "db1"},
		{"backup", "db1", "--verbose"},
		{"restore", "db1", "--repo", repo, "--target", filepath.Join(t.TempDir(), "r")},
		{"logs", "-n", "5"},
		{"notify", "list"},
		{"schedule"},
		{"init", "--yes", "--pg-connection", "host=127.0.0.1 port=1 dbname=x user=u password=p connect_timeout=1", "--repo", repo, "--deployment", "db2", "--skip-backup"},
		{"no-such-command"},
	}
	for _, c := range cases {
		args := append(append([]string(nil), c...), "-o", "json")
		label := strings.Join(c, " ")
		stdout, stderr, exit, ok := runBounded(t, "", 20*time.Second, args...)
		if !ok {
			t.Errorf("%s: did not return within its deadline", label)
			continue
		}
		t.Logf("%s: exit %d", label, exit)
		assertJSONContract(t, label, stdout, stderr, exit)
	}
}

// TestJSONContract_MCPServerStdoutIsJSONRPC: `llm --mcp-server` owns
// stdout for JSON-RPC; start-up events must never land there, whatever
// the output format.
func TestJSONContract_MCPServerStdoutIsJSONRPC(t *testing.T) {
	newContractWorld(t)
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
	for _, format := range []string{"json", "text", "ndjson"} {
		stdout, _, _, ok := runBounded(t, req, 10*time.Second, "llm", "--mcp-server", "-o", format)
		if !ok {
			t.Fatalf("-o %s: MCP server did not exit at stdin EOF", format)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Fatalf("-o %s: no JSON-RPC response on stdout", format)
		}
		for i, line := range lines {
			var msg map[string]any
			if err := stdjson.Unmarshal([]byte(line), &msg); err != nil || msg["jsonrpc"] != "2.0" {
				t.Errorf("-o %s: stdout line %d is not a JSON-RPC frame: %q", format, i, line)
			}
		}
	}
}

// syncBuffer is a bytes.Buffer safe for a command that writes from
// several goroutines while the test reads it after a timeout.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

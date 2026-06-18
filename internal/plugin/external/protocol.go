// Package external is the Tier-2 plugin host.
//
// Tier-1 plugins live in this repo, statically linked into
// the binary, registered via init().  Tier-2 plugins are
// shipped as separate executables that pg_hardstorage
// discovers at startup and speaks to over a stdio JSON-RPC
// channel.  Crash-isolated, language-agnostic, no shared
// library ABI to break across Go versions.
//
// # wire contract: stdio JSON-RPC (this file)
// # v1.x target: gRPC (see proto/plugin/v1/plugin.proto)
//
// SPEC.md and `proto/plugin/v1/plugin.proto` describe the
// **forward-looking** wire contract: hashicorp/go-plugin
// over gRPC, with streaming RPCs and rich error types.  In
// the actually-shipped wire format is the stdio
// JSON-RPC defined below — simpler to author against (no
// protoc toolchain), simpler to debug (every line is
// human-readable), and language-agnostic.
//
// The proto file is the v1.x target; the bytes on the wire
// today are JSON.  Tier-2 plugin authors targeting
// implement the stdio JSON-RPC handshake; the gRPC variant
// will land alongside the public registry at
// registry.pghardstorage.org and will be additive (the
// stdio path stays for the 24-month back-compat window).
//
// # Wire protocol
//
// The host launches the plugin executable with the env var
// `PG_HARDSTORAGE_PLUGIN=1` and (initially) the arg
// `--probe`.  The plugin writes one JSON object on stdout:
//
//	{"protocol":"pg_hardstorage.plugin.v1",
//	 "name":"my-storage",
//	 "kind":"storage",
//	 "schemes":["myproto"]}
//
// Then exits.  Probe is the discovery handshake — every
// plugin must respond to `--probe` even if it doesn't
// implement any other RPC.
//
// On a per-operation invocation, the host launches the
// plugin again (no `--probe`), this time speaking JSON-RPC
// over stdio.  Each request is a single line of JSON on
// stdin; each response is a single line of JSON on stdout.
// Both sides terminate the conversation by closing stdin
// (host -> plugin) or exiting (plugin -> host).
//
// # Why one-shot processes (not long-lived daemons)
//
// Long-lived plugin daemons need:
//   - a supervisor (lifecycle, restart-on-crash)
//   - a concurrency model (request multiplexing)
//   - a shutdown protocol
//
// One-shot exec-per-call avoids all three.  Cost: TLS
// handshake / SDK init runs once per call rather than once
// per process.  Acceptable for the operations Tier-2
// plugins do (init repo, refresh state, probe credentials);
// the hot path (chunk I/O during a backup) stays Tier-1.
//
// Future v1.1+ work may layer a long-lived mode on top of
// this protocol; the v1 contract is the one-shot shape.
package external

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the wire-format identifier carried in
// every probe response.  24-month back-compat for v1; v2
// will run alongside v1 during the transition.
const ProtocolVersion = "pg_hardstorage.plugin.v1"

// MaxPluginMessageBytes caps a single probe/RPC message read from a
// plugin process so a buggy or runaway plugin can't exhaust host memory
// with an unbounded stdout write. Probe responses and RPC results are
// small JSON documents; 8 MiB is far above any legitimate message.
const MaxPluginMessageBytes = 8 << 20 // 8 MiB

// EnvIsPlugin is the env var the host sets when launching a
// plugin process.  Plugins MAY check this to refuse running
// if invoked by a user (vs. by the host).
const EnvIsPlugin = "PG_HARDSTORAGE_PLUGIN"

// EnvPluginPath is the colon-separated list of directories
// the host walks for plugin executables.  Defaults to
// `/usr/local/lib/pg_hardstorage/plugins:/usr/lib/pg_hardstorage/plugins`
// when unset.
const EnvPluginPath = "HSPLUGIN_PATH"

// PluginPrefix is the filename prefix every plugin
// executable must use.  Discovery walks the plugin path
// and treats any executable starting with this string as a
// candidate.  We deliberately don't allow arbitrary
// filenames — a typo'd ls.binary could otherwise be
// invoked by accident.
const PluginPrefix = "pg-hardstorage-plugin-"

// ProbeResponse is the discovery payload every plugin must
// emit when invoked with `--probe`.
type ProbeResponse struct {
	Protocol string   `json:"protocol"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`              // "storage"|"sink"|"kms"|"compression"|"renderer"
	Schemes  []string `json:"schemes,omitempty"` // for storage/kms: URL schemes the plugin claims
	Version  string   `json:"version,omitempty"` // plugin's own version (informational)
}

// Plugin describes one discovered plugin executable.
type Plugin struct {
	ProbeResponse

	// Path is the absolute filesystem path to the
	// executable.  Stored so the host can re-spawn it for
	// each RPC without re-walking $HSPLUGIN_PATH.
	Path string
}

// Request is one host-to-plugin call.  Method names mirror
// the Tier-1 plugin interface methods (e.g. "Storage.Put",
// "Sink.Emit").  Params is method-specific.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one plugin-to-host reply.  Exactly one of
// Result / Error is set.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is the structured error shape every plugin
// returns in lieu of throwing.  Code follows the v1 schema's
// `error.code` namespace (e.g. "storage.not_found",
// "auth.permission_denied").
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Discover walks every directory in $HSPLUGIN_PATH (or the
// default), invokes each executable with `--probe`, and
// returns the resulting Plugin list, sorted by Name.
//
// Probe failures are surfaced as warnings to the optional
// logger but never fatal — a bad plugin never blocks
// startup.
func Discover(ctx context.Context, logger func(format string, args ...any)) []Plugin {
	dirs := strings.Split(os.Getenv(EnvPluginPath), string(os.PathListSeparator))
	if len(dirs) == 0 || (len(dirs) == 1 && dirs[0] == "") {
		dirs = []string{
			"/usr/local/lib/pg_hardstorage/plugins",
			"/usr/lib/pg_hardstorage/plugins",
		}
	}
	if logger == nil {
		logger = func(format string, args ...any) {}
	}
	var found []Plugin
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				logger("plugin: read dir %s: %v", dir, err)
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if !strings.HasPrefix(n, PluginPrefix) {
				continue
			}
			full := filepath.Join(dir, n)
			info, err := os.Stat(full)
			if err != nil {
				logger("plugin: stat %s: %v", full, err)
				continue
			}
			if info.Mode()&0o111 == 0 {
				logger("plugin: %s is not executable; skipping", full)
				continue
			}
			pr, err := probe(ctx, full)
			if err != nil {
				logger("plugin: probe %s: %v", full, err)
				continue
			}
			found = append(found, Plugin{ProbeResponse: pr, Path: full})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// probe spawns the executable with --probe and parses one
// JSON response from stdout.  5-second timeout so a hung
// plugin can't stall startup.
func probe(parentCtx context.Context, path string) (ProbeResponse, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--probe")
	cmd.Env = append(os.Environ(), EnvIsPlugin+"=1")
	// Cap the probe stdout read: cmd.Output() buffers stdout without
	// bound, so a runaway plugin could OOM the host. Read through a
	// LimitReader and cancel the process if it overruns; the 5s context
	// (plus WaitDelay) bounds a plugin that then blocks on the pipe.
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ProbeResponse{}, fmt.Errorf("stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return ProbeResponse{}, fmt.Errorf("exec: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, MaxPluginMessageBytes+1))
	overSize := int64(len(out)) > MaxPluginMessageBytes
	if overSize {
		cancel() // stop a plugin spewing past the cap
	}
	waitErr := cmd.Wait()
	if overSize {
		return ProbeResponse{}, fmt.Errorf("probe output exceeds %d-byte cap (runaway plugin?)", MaxPluginMessageBytes)
	}
	if waitErr != nil {
		return ProbeResponse{}, fmt.Errorf("exec: %w", waitErr)
	}
	if readErr != nil {
		return ProbeResponse{}, fmt.Errorf("read probe output: %w", readErr)
	}
	var pr ProbeResponse
	if err := json.Unmarshal(out, &pr); err != nil {
		return ProbeResponse{}, fmt.Errorf("parse probe output: %w", err)
	}
	if pr.Protocol != ProtocolVersion {
		return ProbeResponse{}, fmt.Errorf("plugin protocol %q is not %q", pr.Protocol, ProtocolVersion)
	}
	if pr.Name == "" || pr.Kind == "" {
		return ProbeResponse{}, errors.New("probe response missing name or kind")
	}
	return pr, nil
}

// --- per-operation RPC --------------------------------------

// Client is a one-shot RPC client over stdio.  Each Call
// spawns a fresh plugin process, writes the request, reads
// the response, and waits for exit.  Re-spawning is the
// crash-isolation property of the protocol.
type Client struct {
	Path string

	// Timeout for one RPC (default 30s).  Slow plugins
	// (cloud SDK init, network round-trip) push this up.
	Timeout time.Duration
}

// Call performs one RPC.  method/params come from the
// caller; the client returns the parsed Result bytes (or a
// wrapped RPCError).
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	rpcCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rpcCtx, c.Path)
	cmd.Env = append(os.Environ(), EnvIsPlugin+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdout: %w", err)
	}
	cmd.Stderr = os.Stderr // mirror plugin diagnostics
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: start: %w", err)
	}

	var paramBytes json.RawMessage
	if params != nil {
		paramBytes, err = json.Marshal(params)
		if err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("plugin: marshal params: %w", err)
		}
	}
	req := Request{Method: method, Params: paramBytes}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("plugin: marshal request: %w", err)
	}
	if _, err := stdin.Write(append(reqBytes, '\n')); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("plugin: write request: %w", err)
	}
	stdin.Close()

	resp, err := readResponse(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		// A non-zero exit after we got a structured response
		// is unusual but not fatal — surface as a warning via
		// the wrapped error.
		if resp.Error == nil {
			return nil, fmt.Errorf("plugin: exited %v after returning result", waitErr)
		}
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("plugin: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// readBoundedLine reads one newline-terminated message from r, refusing
// anything larger than MaxPluginMessageBytes. An unbounded
// bufio.ReadString would buffer a runaway plugin's entire stdout (a
// gigabyte single line) into memory; the LimitReader stops it at the
// cap and we report it as an over-size error rather than OOMing.
func readBoundedLine(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxPluginMessageBytes+1))
	line, err := br.ReadString('\n')
	if int64(len(line)) > MaxPluginMessageBytes {
		return "", fmt.Errorf("message exceeds %d-byte cap (runaway plugin output?)", MaxPluginMessageBytes)
	}
	return line, err
}

func readResponse(r io.Reader) (Response, error) {
	line, err := readBoundedLine(r)
	if err != nil && line == "" {
		return Response{}, fmt.Errorf("plugin: read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		return Response{}, fmt.Errorf("plugin: parse response: %w", err)
	}
	if resp.Error == nil && len(resp.Result) == 0 {
		return Response{}, errors.New("plugin: response missing both result and error")
	}
	return resp, nil
}

// --- plugin-side helpers (used by external-plugin authors) ---

// IsPluginInvocation reports whether the current process was
// launched by the host (vs. by a curious operator).  Plugins
// can use this to refuse running interactively:
//
//	if !external.IsPluginInvocation() {
//	    fmt.Fprintln(os.Stderr, "this binary is a pg_hardstorage plugin; do not run directly")
//	    os.Exit(2)
//	}
func IsPluginInvocation() bool {
	return os.Getenv(EnvIsPlugin) == "1"
}

// EmitProbeResponse writes a probe response to stdout.
// Convenience for plugin authors.
func EmitProbeResponse(out io.Writer, name, kind string, schemes []string, version string) error {
	pr := ProbeResponse{
		Protocol: ProtocolVersion,
		Name:     name,
		Kind:     kind,
		Schemes:  schemes,
		Version:  version,
	}
	enc := json.NewEncoder(out)
	return enc.Encode(pr)
}

// ServeRPC is the plugin-side dispatcher.  Reads one request
// line from stdin, looks up handler, writes one response
// line to stdout, returns.  Plugin authors call this once
// in their main():
//
//	external.ServeRPC(map[string]external.Handler{
//	    "Sink.Emit": func(params json.RawMessage) (any, error) { ... },
//	})
type Handler func(params json.RawMessage) (any, error)

// ServeRPC dispatches one RPC and writes the response.
func ServeRPC(in io.Reader, out io.Writer, handlers map[string]Handler) error {
	line, err := readBoundedLine(in)
	if err != nil && line == "" {
		return fmt.Errorf("plugin: read request: %w", err)
	}
	var req Request
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
		return writeResponse(out, Response{Error: &RPCError{
			Code: "plugin.parse_request", Message: err.Error(),
		}})
	}
	h, ok := handlers[req.Method]
	if !ok {
		return writeResponse(out, Response{Error: &RPCError{
			Code:    "plugin.unknown_method",
			Message: fmt.Sprintf("no handler for %q", req.Method),
		}})
	}
	result, err := h(req.Params)
	if err != nil {
		return writeResponse(out, Response{Error: &RPCError{
			Code: "plugin.method_error", Message: err.Error(),
		}})
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return writeResponse(out, Response{Error: &RPCError{
			Code: "plugin.marshal_result", Message: err.Error(),
		}})
	}
	return writeResponse(out, Response{Result: resultBytes})
}

func writeResponse(out io.Writer, resp Response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = out.Write(append(body, '\n'))
	return err
}

// --- registry ----------------------------------------------

// Registry holds discovered plugins keyed by Name.  The
// host builds it once at startup; subsequent kind-specific
// dispatchers consult it.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]Plugin
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry { return &Registry{plugins: map[string]Plugin{}} }

// Register adds a plugin.  Re-registration overwrites — the
// idiom for an operator-overlay path being preferred over
// the system path.
func (r *Registry) Register(p Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[p.Name] = p
}

// Get returns the plugin with the given name, or false.
func (r *Registry) Get(name string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[name]
	return p, ok
}

// ByKind returns every plugin claiming the given kind,
// sorted by name.
func (r *Registry) ByKind(kind string) []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Plugin
	for _, p := range r.plugins {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All returns every registered plugin sorted by Name.
func (r *Registry) All() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

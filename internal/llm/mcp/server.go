// Package mcp implements a Model Context Protocol server over
// stdio, exposing pg_hardstorage's read-only tool surface and
// builtin skills to any MCP-aware client (Claude Desktop,
// Cursor, Zed, Goose, Cline, …).
//
// The plan calls this "the highest-leverage integration we can
// ship — we don't compete with Claude Desktop, we plug into it."
// Operators add `pg_hardstorage llm --mcp-server` to their MCP
// client config; the client speaks MCP over stdio; our tools
// surface in their UI alongside whatever else they have.
//
// Wire format: line-delimited JSON-RPC 2.0 over stdin/stdout.
// We support the protocol version 2024-11-05.  Methods
// implemented:
//
//	initialize             — handshake.
//	tools/list             — every read-only tool the operator's
//	                         skills expose.
//	tools/call             — invoke a tool, return its result.
//	prompts/list           — every loaded skill (builtin + override).
//	prompts/get            — return a skill's prompt template +
//	                         pre-loaded cluster context.
//	notifications/cancelled — no-op (we don't track in-flight
//	                          tool calls across requests today).
//
// Methods deliberately NOT implemented in this commit:
//
//   - resources/* — we don't expose the docs corpus as MCP
//     resources; clients fetch via the search_docs tool.  A
//     future commit can mirror the corpus into resources/list
//     for clients that prefer the explicit shape.
//   - sampling/* — the server doesn't request completions from
//     the client;+ skills run their own LLM via the
//     openai provider.
//   - logging/setLevel — accepted but no-op.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/docs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// ProtocolVersion is the MCP version we advertise.  Pinned to
// the 2024-11-05 spec; bumps require checking the changelog
// for breaking shape changes.
const ProtocolVersion = "2024-11-05"

// ServerInfo describes this server to MCP clients.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Server is a stdio MCP server.  Reads line-delimited JSON-RPC
// 2.0 requests from In, writes responses to Out.  Errors go to
// Err for visibility (clients see them via stderr forwarding).
type Server struct {
	// In is the JSON-RPC request stream.  Defaults to os.Stdin
	// when nil.
	In io.Reader
	// Out is where JSON-RPC responses are written.  Defaults
	// to os.Stdout when nil.
	Out io.Writer
	// Err is where non-fatal log lines go.  Defaults to
	// os.Stderr when nil.
	Err io.Writer

	// Tools is the read-only tool surface the server exposes.
	// Required.
	Tools *tools.Registry

	// Skills is the loaded skill set surfaced as MCP prompts.
	// Optional; nil disables prompts/* dispatch.
	Skills *skills.Set

	// Info is what we advertise on initialize.
	Info ServerInfo

	// initialized guards the strict ordering MCP requires:
	// `initialize` must be the first request, and every other
	// method must come after.
	mu          sync.Mutex
	initialized bool
}

// Run drives the server until In returns EOF or ctx is cancelled.
// One JSON-RPC message per line on stdin; one JSON-RPC message
// per line on stdout.  Errors during dispatch surface as
// JSON-RPC error responses; transport-level errors (read of
// stdin failed, etc.) are returned.
func (s *Server) Run(ctx context.Context) error {
	if s.In == nil || s.Out == nil {
		return errors.New("mcp: Server.In and Server.Out are required")
	}
	if s.Tools == nil {
		return errors.New("mcp: Server.Tools is required")
	}
	scanner := bufio.NewScanner(s.In)
	scanner.Buffer(make([]byte, 64*1024), 1<<20) // 1 MiB max line
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(nil, parseErrorCode, "parse error: "+err.Error())
			continue
		}
		s.dispatch(ctx, &req)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcp: read: %w", err)
	}
	return nil
}

// dispatch handles a single JSON-RPC request.  Notifications
// (no ID) get no response; requests get exactly one (success
// or error).
func (s *Server) dispatch(ctx context.Context, req *rpcRequest) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "tools/list":
		s.requireInit(req, s.handleToolsList)
	case "tools/call":
		s.requireInit(req, func(req *rpcRequest) { s.handleToolsCall(ctx, req) })
	case "prompts/list":
		s.requireInit(req, s.handlePromptsList)
	case "prompts/get":
		s.requireInit(req, s.handlePromptsGet)
	case "notifications/initialized":
		// Spec: client signals it has finished initialization.
		// No response (it's a notification).
		// We mark initialized here — strictly speaking,
		// `initialize` already did it, but the SDK
		// implementations vary.
		s.markInit()
	case "notifications/cancelled", "logging/setLevel":
		// No-op; respond OK if it was a request, ignore if
		// notification.
		if req.ID != nil {
			s.writeResult(req.ID, map[string]any{})
		}
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	default:
		if req.ID != nil {
			s.writeError(req.ID, methodNotFoundCode, "method not found: "+req.Method)
		}
	}
}

// requireInit gates a handler on the `initialize` handshake.
// Returns method-not-allowed when uninitialised.
func (s *Server) requireInit(req *rpcRequest, fn func(*rpcRequest)) {
	s.mu.Lock()
	ready := s.initialized
	s.mu.Unlock()
	if !ready {
		s.writeError(req.ID, invalidRequestCode, "server not initialised; call `initialize` first")
		return
	}
	fn(req)
}

func (s *Server) markInit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialized = true
}

// --- handlers --------------------------------------------------------

func (s *Server) handleInitialize(req *rpcRequest) {
	s.markInit()
	s.writeResult(req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"serverInfo":      s.Info,
		"capabilities": map[string]any{
			"tools":   map[string]any{},
			"prompts": map[string]any{},
		},
	})
}

func (s *Server) handleToolsList(req *rpcRequest) {
	all := s.Tools.All()
	out := make([]map[string]any, 0, len(all))
	for _, t := range all {
		if !t.ReadOnly() {
			continue // never advertise mutating tools to MCP clients
		}
		out = append(out, map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"inputSchema": t.Schema(),
		})
	}
	s.writeResult(req.ID, map[string]any{"tools": out})
}

func (s *Server) handleToolsCall(ctx context.Context, req *rpcRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, invalidParamsCode, "tools/call params: "+err.Error())
		return
	}
	if params.Name == "" {
		s.writeError(req.ID, invalidParamsCode, "tools/call: `name` is required")
		return
	}
	t, err := s.Tools.Get(params.Name)
	if err != nil {
		s.writeError(req.ID, invalidParamsCode, fmt.Sprintf("tools/call: %v", err))
		return
	}
	if !t.ReadOnly() {
		s.writeError(req.ID, invalidParamsCode,
			fmt.Sprintf("tools/call: %q is not read-only;+ MCP server refuses mutation tools", params.Name))
		return
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	res, err := t.Run(ctx, params.Arguments)
	if err != nil {
		// Surface as a tool-result with isError=true (per MCP
		// spec, tool errors are not JSON-RPC errors — they're
		// successful tool calls that returned an error).
		s.writeResult(req.ID, map[string]any{
			"isError": true,
			"content": []map[string]any{{
				"type": "text",
				"text": err.Error(),
			}},
		})
		return
	}
	body, _ := json.MarshalIndent(map[string]any{
		"summary": res.Summary,
		"body":    res.Body,
	}, "", "  ")
	s.writeResult(req.ID, map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(body),
		}},
	})
}

func (s *Server) handlePromptsList(req *rpcRequest) {
	if s.Skills == nil {
		s.writeResult(req.ID, map[string]any{"prompts": []any{}})
		return
	}
	all := s.Skills.All()
	out := make([]map[string]any, 0, len(all))
	for _, sk := range all {
		out = append(out, map[string]any{
			"name":        sk.Name,
			"description": firstLineOf(sk.Description),
		})
	}
	s.writeResult(req.ID, map[string]any{"prompts": out})
}

func (s *Server) handlePromptsGet(req *rpcRequest) {
	if s.Skills == nil {
		s.writeError(req.ID, methodNotFoundCode, "prompts/* not enabled (no skill set wired)")
		return
	}
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, invalidParamsCode, "prompts/get params: "+err.Error())
		return
	}
	skill, err := s.Skills.Get(params.Name)
	if err != nil {
		s.writeError(req.ID, invalidParamsCode, fmt.Sprintf("prompts/get: %v", err))
		return
	}
	// MCP prompts return a list of messages.  We return one
	// system message containing the skill's prompt template
	// plus a runbook-index appendix (so the client gets the
	// same starting context our own chat session does).
	content := skill.PromptTemplate
	if idx, err := docs.RunbookIndex(); err == nil && len(idx) > 0 {
		var b strings.Builder
		b.WriteString(content)
		b.WriteString("\n\n## Runbook index\n\n")
		for _, e := range idx {
			fmt.Fprintf(&b, "- **%s** — %s\n", e.ID, e.Title)
		}
		content = b.String()
	}
	s.writeResult(req.ID, map[string]any{
		"description": firstLineOf(skill.Description),
		"messages": []map[string]any{{
			"role": "system",
			"content": map[string]any{
				"type": "text",
				"text": content,
			},
		}},
	})
}

// --- JSON-RPC plumbing -----------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	parseErrorCode     = -32700
	invalidRequestCode = -32600
	methodNotFoundCode = -32601
	invalidParamsCode  = -32602
	internalErrorCode  = -32603
)

func (s *Server) writeResult(id json.RawMessage, result any) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeError(id json.RawMessage, code int, message string) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (s *Server) write(resp rpcResponse) {
	body, err := json.Marshal(resp)
	if err != nil {
		// Fall back to a minimal hand-rolled error envelope.
		body = []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"marshal failed"}}`)
	}
	body = append(body, '\n')
	if _, err := s.Out.Write(body); err != nil && s.Err != nil {
		fmt.Fprintf(s.Err, "mcp: write: %v\n", err)
	}
}

func firstLineOf(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// silence unused-import warnings on niche build paths.
var _ = internalErrorCode

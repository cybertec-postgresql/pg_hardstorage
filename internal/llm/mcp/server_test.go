package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/mcp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// fakeTool is a deterministic read-only tool for MCP tests.
type fakeTool struct {
	name     string
	desc     string
	readOnly bool
	body     any
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Description() string    { return f.desc }
func (f *fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f *fakeTool) ReadOnly() bool         { return f.readOnly }
func (f *fakeTool) Run(_ context.Context, _ map[string]any) (tools.Result, error) {
	if f.body == nil {
		return tools.Result{Summary: "ok"}, nil
	}
	return tools.Result{Summary: "ok", Body: f.body}, nil
}

// runServer drives an MCP server with the given JSON-RPC lines
// as input.  Returns the responses (one per line in the order
// the server produced them).
func runServer(t *testing.T, server *mcp.Server, requests ...string) []map[string]any {
	t.Helper()
	in := strings.Join(requests, "\n") + "\n"
	var out bytes.Buffer
	server.In = strings.NewReader(in)
	server.Out = &out
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("server: %v", err)
	}
	var responses []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("parse response %q: %v", line, err)
		}
		responses = append(responses, resp)
	}
	return responses
}

func TestServer_RequiresIn(t *testing.T) {
	s := &mcp.Server{Tools: tools.NewRegistry()}
	if err := s.Run(context.Background()); err == nil {
		t.Error("nil In should error")
	}
}

func TestServer_RequiresTools(t *testing.T) {
	s := &mcp.Server{
		In:  strings.NewReader(""),
		Out: &bytes.Buffer{},
	}
	if err := s.Run(context.Background()); err == nil {
		t.Error("nil Tools should error")
	}
}

func TestServer_Initialize(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{
		Tools: reg,
		Info:  mcp.ServerInfo{Name: "pg_hardstorage-test", Version: "1.0.0"},
	}
	resps := runServer(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response; got %d", len(resps))
	}
	r := resps[0]
	if r["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", r["jsonrpc"])
	}
	res, _ := r["result"].(map[string]any)
	if res["protocolVersion"] != mcp.ProtocolVersion {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, has := caps["tools"]; !has {
		t.Error("capabilities should include tools")
	}
	if _, has := caps["prompts"]; !has {
		t.Error("capabilities should include prompts")
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "pg_hardstorage-test" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
}

func TestServer_RequiresInitializeFirst(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response; got %d", len(resps))
	}
	errObj, _ := resps[0]["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error response; got %+v", resps[0])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "initialize") {
		t.Errorf("expected init-required message; got %v", msg)
	}
}

func TestServer_ToolsList(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "read_a", desc: "A", readOnly: true})
	reg.Register(&fakeTool{name: "read_b", desc: "B", readOnly: true})
	reg.Register(&fakeTool{name: "execute_x", desc: "X", readOnly: false})
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses; got %d", len(resps))
	}
	res, _ := resps[1]["result"].(map[string]any)
	list, _ := res["tools"].([]any)
	if len(list) != 2 {
		t.Errorf("tools count = %d, want 2 (mutation tool must be filtered)", len(list))
	}
	for _, raw := range list {
		t0 := raw.(map[string]any)
		if t0["name"] == "execute_x" {
			t.Error("mutation tool leaked into tools/list")
		}
	}
}

func TestServer_ToolsCall(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "read_doctor", desc: "check", readOnly: true,
		body: map[string]any{"healthy": true},
	})
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_doctor"}}`)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses; got %d", len(resps))
	}
	res, _ := resps[1]["result"].(map[string]any)
	if res == nil {
		t.Fatalf("missing result; got %+v", resps[1])
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block; got %d", len(content))
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Errorf("content type = %v, want text", first["type"])
	}
	if !strings.Contains(first["text"].(string), "healthy") {
		t.Errorf("content text lost the body: %v", first["text"])
	}
}

func TestServer_ToolsCall_RefusesMutationTool(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "execute_x", desc: "x", readOnly: false})
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"execute_x"}}`)
	errObj, _ := resps[1]["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error; got %+v", resps[1])
	}
}

func TestServer_ToolsCall_UnknownToolErrors(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"missing"}}`)
	if _, has := resps[1]["error"]; !has {
		t.Errorf("unknown tool should yield JSON-RPC error; got %+v", resps[1])
	}
}

func TestServer_PromptsList(t *testing.T) {
	reg := tools.NewRegistry()
	set, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcp.Server{Tools: reg, Skills: set}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/list"}`)
	res, _ := resps[1]["result"].(map[string]any)
	prompts, _ := res["prompts"].([]any)
	if len(prompts) < 4 {
		t.Errorf("expected ≥ 4 builtin skills; got %d", len(prompts))
	}
	names := map[string]bool{}
	for _, raw := range prompts {
		p := raw.(map[string]any)
		names[p["name"].(string)] = true
	}
	for _, want := range []string{"ask", "explain", "restore", "incident"} {
		if !names[want] {
			t.Errorf("expected builtin skill %s in prompts/list", want)
		}
	}
}

func TestServer_PromptsGet(t *testing.T) {
	reg := tools.NewRegistry()
	set, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcp.Server{Tools: reg, Skills: set}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"ask"}}`)
	res, _ := resps[1]["result"].(map[string]any)
	if res == nil {
		t.Fatalf("missing result; got %+v", resps[1])
	}
	msgs, _ := res["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message; got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("role = %v, want system", first["role"])
	}
	content := first["content"].(map[string]any)
	if !strings.Contains(content["text"].(string), "operator assistant") {
		t.Errorf("ask skill text missing; got %q", content["text"])
	}
	// The runbook-index appendix should be present.
	if !strings.Contains(content["text"].(string), "Runbook index") {
		t.Errorf("ask prompt should append runbook index; got %q", content["text"])
	}
}

func TestServer_PromptsGet_UnknownErrors(t *testing.T) {
	reg := tools.NewRegistry()
	set, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcp.Server{Tools: reg, Skills: set}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"nonexistent"}}`)
	if _, has := resps[1]["error"]; !has {
		t.Errorf("unknown prompt should yield error; got %+v", resps[1])
	}
}

func TestServer_UnknownMethod(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"made/up"}`)
	errObj, _ := resps[1]["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("unknown method should yield error; got %+v", resps[1])
	}
}

func TestServer_PingResponds(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if _, has := resps[1]["result"]; !has {
		t.Errorf("ping should yield empty result; got %+v", resps[1])
	}
}

func TestServer_NotificationsHaveNoResponse(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{Tools: reg}
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(resps) != 1 {
		t.Fatalf("notifications should produce no response; got %d total responses", len(resps))
	}
}

func TestServer_BadJSONRecoverable(t *testing.T) {
	reg := tools.NewRegistry()
	s := &mcp.Server{Tools: reg}
	// One garbage line, then a valid initialize.  The server
	// should emit a parse-error response for the garbage and
	// still handle the initialize.
	resps := runServer(t, s,
		`not-json`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses (parse error + initialize); got %d", len(resps))
	}
	errObj, _ := resps[0]["error"].(map[string]any)
	if errObj == nil {
		t.Errorf("first response should be parse error; got %+v", resps[0])
	}
}

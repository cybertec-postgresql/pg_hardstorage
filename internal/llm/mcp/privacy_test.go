package mcp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/mcp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/privacy"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

type erroringTool struct{ msg string }

func (erroringTool) Name() string           { return "read_status" }
func (erroringTool) Description() string    { return "status" }
func (erroringTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (erroringTool) ReadOnly() bool         { return true }
func (e erroringTool) Run(_ context.Context, _ map[string]any) (tools.Result, error) {
	return tools.Result{}, errors.New(e.msg)
}

func callText(t *testing.T, s *mcp.Server, tool string) (string, bool) {
	t.Helper()
	resps := runServer(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tool+`"}}`)
	res, _ := resps[1]["result"].(map[string]any)
	if res == nil {
		t.Fatalf("missing result: %+v", resps[1])
	}
	content, _ := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

// An MCP client forwards tool results to ITS model provider.  The chat
// path redacts tool results under the configured privacy mode; the MCP
// server served them raw — credentials included.
func TestServer_ToolsCall_RedactsPerPrivacyMode(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "read_doctor", readOnly: true, body: map[string]any{
		"pg_connection": "postgres://backup:hunter2@10.1.2.3/db",
		"owner":         "ops@example.com",
	}})
	reg.Register(erroringTool{msg: "exit 1: password=hunter2 for ops@example.com"})

	for _, mode := range []privacy.Mode{"", privacy.ModeStandard, privacy.ModeStrict} {
		for _, tool := range []string{"read_doctor", "read_status"} {
			text, _ := callText(t, &mcp.Server{Tools: reg, Privacy: mode}, tool)
			for _, leak := range []string{"hunter2", "ops@example.com", "10.1.2.3"} {
				if strings.Contains(text, leak) {
					t.Errorf("mode %q tool %s: leaked %q: %s", mode, tool, leak, text)
				}
			}
		}
	}
	// open still masks credentials.
	text, _ := callText(t, &mcp.Server{Tools: reg, Privacy: privacy.ModeOpen}, "read_doctor")
	if strings.Contains(text, "hunter2") {
		t.Errorf("open mode leaked credential: %s", text)
	}
}

// local-only means cluster data may only reach a loopback / RFC-1918
// model.  The MCP server cannot see which model its client forwards to,
// so it cannot vouch for the endpoint and must refuse tool calls.
func TestServer_ToolsCall_LocalOnlyRefuses(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "read_doctor", readOnly: true, body: map[string]any{"deployment": "db1"}})
	text, isErr := callText(t, &mcp.Server{Tools: reg, Privacy: privacy.ModeLocalOnly}, "read_doctor")
	if !isErr {
		t.Errorf("local-only: tool call served: %s", text)
	}
	if strings.Contains(text, "db1") {
		t.Errorf("local-only: cluster data leaked: %s", text)
	}
}

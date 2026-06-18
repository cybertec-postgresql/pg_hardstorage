package external_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
)

// fakePluginScript writes a tiny shell script that emulates
// a plugin's --probe handshake.  The body is a literal shell
// script body — we wrap it in #!/bin/sh and chmod +x it.
func fakePluginScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(full, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return full
}

// probeScript builds a `cat <<EOF` shell body that emits the
// supplied probe JSON when the script is run.  Avoids the
// quote-escaping pitfalls of `echo '...'` for JSON bodies.
func probeScript(probe string) string {
	return "cat <<'PROBE_EOF'\n" + probe + "\nPROBE_EOF"
}

func TestDiscover_FindsExecutables(t *testing.T) {
	dir := t.TempDir()
	probeJSON := fmt.Sprintf(`{"protocol":%q,"name":"my-sink","kind":"sink","schemes":["my"]}`, external.ProtocolVersion)
	fakePluginScript(t, dir, "pg-hardstorage-plugin-mysink", probeScript(probeJSON))

	t.Setenv("HSPLUGIN_PATH", dir)
	plugins := external.Discover(context.Background(), nil)
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d: %#v", len(plugins), plugins)
	}
	if plugins[0].Name != "my-sink" || plugins[0].Kind != "sink" {
		t.Errorf("plugin metadata wrong: %#v", plugins[0])
	}
}

func TestDiscover_SkipsNonPrefixed(t *testing.T) {
	dir := t.TempDir()
	probeJSON := fmt.Sprintf(`{"protocol":%q,"name":"x","kind":"y"}`, external.ProtocolVersion)
	// Right name → discovered.
	fakePluginScript(t, dir, "pg-hardstorage-plugin-good", probeScript(probeJSON))
	// Wrong prefix → ignored.
	fakePluginScript(t, dir, "random-binary", `echo bad`)

	t.Setenv("HSPLUGIN_PATH", dir)
	plugins := external.Discover(context.Background(), nil)
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
}

func TestDiscover_RejectsWrongProtocol(t *testing.T) {
	dir := t.TempDir()
	fakePluginScript(t, dir, "pg-hardstorage-plugin-bad",
		probeScript(`{"protocol":"some.other.protocol.v9","name":"x","kind":"y"}`))
	t.Setenv("HSPLUGIN_PATH", dir)
	plugins := external.Discover(context.Background(), nil)
	if len(plugins) != 0 {
		t.Errorf("plugin with mismatched protocol should be skipped; got %v", plugins)
	}
}

func TestDiscover_RejectsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "pg-hardstorage-plugin-not-exec")
	if err := os.WriteFile(full, []byte("not exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HSPLUGIN_PATH", dir)
	plugins := external.Discover(context.Background(), nil)
	if len(plugins) != 0 {
		t.Errorf("non-executable file should be skipped; got %v", plugins)
	}
}

func TestSchemeOf_AndKind(t *testing.T) {
	r := external.NewRegistry()
	r.Register(external.Plugin{
		ProbeResponse: external.ProbeResponse{Name: "alpha", Kind: "sink"},
	})
	r.Register(external.Plugin{
		ProbeResponse: external.ProbeResponse{Name: "beta", Kind: "storage"},
	})
	r.Register(external.Plugin{
		ProbeResponse: external.ProbeResponse{Name: "gamma", Kind: "sink"},
	})
	sinks := r.ByKind("sink")
	if len(sinks) != 2 || sinks[0].Name != "alpha" || sinks[1].Name != "gamma" {
		t.Errorf("ByKind returned %v", sinks)
	}
}

func TestEmitProbeResponse_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := external.EmitProbeResponse(&buf, "test", "sink", []string{"x"}, "1.0"); err != nil {
		t.Fatal(err)
	}
	var pr external.ProbeResponse
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Protocol != external.ProtocolVersion {
		t.Errorf("Protocol = %q", pr.Protocol)
	}
	if pr.Name != "test" || pr.Kind != "sink" {
		t.Errorf("metadata wrong: %#v", pr)
	}
}

func TestServeRPC_DispatchesAndMarshalsResult(t *testing.T) {
	in := strings.NewReader(`{"method":"Echo","params":{"value":"hello"}}` + "\n")
	var out bytes.Buffer
	handlers := map[string]external.Handler{
		"Echo": func(params json.RawMessage) (any, error) {
			var p struct{ Value string }
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			return map[string]any{"echoed": p.Value}, nil
		},
	}
	if err := external.ServeRPC(in, &out, handlers); err != nil {
		t.Fatal(err)
	}
	var resp external.Response
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if !bytes.Contains(resp.Result, []byte(`"echoed":"hello"`)) {
		t.Errorf("result = %s", resp.Result)
	}
}

func TestServeRPC_UnknownMethod(t *testing.T) {
	in := strings.NewReader(`{"method":"NoSuchMethod"}` + "\n")
	var out bytes.Buffer
	external.ServeRPC(in, &out, map[string]external.Handler{})
	if !bytes.Contains(out.Bytes(), []byte("plugin.unknown_method")) {
		t.Errorf("expected plugin.unknown_method; got %s", out.String())
	}
}

func TestClient_Call_Roundtrip(t *testing.T) {
	// Build a tiny plugin executable in a tempdir using
	// `sh` that round-trips one RPC.
	dir := t.TempDir()
	plugin := filepath.Join(dir, "pg-hardstorage-plugin-echo")
	body := `#!/bin/sh
read line
echo '{"result":{"echoed":"plugin-side"}}'
`
	if err := os.WriteFile(plugin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	cli := &external.Client{Path: plugin}
	res, err := cli.Call(context.Background(), "Echo", map[string]string{"value": "hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !bytes.Contains(res, []byte(`"echoed":"plugin-side"`)) {
		t.Errorf("result = %s", res)
	}
}

func TestClient_Call_StructuredError(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "pg-hardstorage-plugin-failer")
	body := `#!/bin/sh
read line
echo '{"error":{"code":"plugin.test_failure","message":"intentional"}}'
`
	if err := os.WriteFile(plugin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cli := &external.Client{Path: plugin}
	_, err := cli.Call(context.Background(), "Fail", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "plugin.test_failure") {
		t.Errorf("error = %v, want code in message", err)
	}
}

func TestIsPluginInvocation(t *testing.T) {
	t.Setenv(external.EnvIsPlugin, "1")
	if !external.IsPluginInvocation() {
		t.Error("IsPluginInvocation should return true when env set")
	}
	t.Setenv(external.EnvIsPlugin, "")
	if external.IsPluginInvocation() {
		t.Error("IsPluginInvocation should return false when env unset")
	}
}

// Confirm the helpers compile against exec.LookPath usage
// in the same way the host wires them up — sanity check
// that the public surface is sufficient.
var _ = exec.LookPath

package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestDoctor_TextMode_FreshUser(t *testing.T) {
	t.Setenv("HOME", "/home/alice")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	// PG_HARDSTORAGE_* env vars cleared so XDG defaults apply.
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("PG_HARDSTORAGE_STATE_DIR", "")

	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"doctor", "-o", "text"})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, stderr=%s", exit, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"PATHS",
		"CONFIG",
		"Status:",
		"not yet configured",
		"pg_hardstorage init",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("text doctor missing %q\n%s", want, s)
		}
	}
}

func TestDoctor_JSONMode_StableShape(t *testing.T) {
	t.Setenv("HOME", "/home/alice")
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"doctor", "-o", "json"})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, stderr=%s", exit, errb.String())
	}

	// Parse the wrapper Result.
	var res output.Result
	if err := stdjson.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if res.Schema != output.Schema {
		t.Errorf("schema = %q", res.Schema)
	}
	if res.IsError() {
		t.Fatal("doctor should not produce an error result on a fresh box")
	}

	// Re-marshal then unmarshal into a typed view that mirrors doctorReport.
	bodyBytes, err := stdjson.Marshal(res.Result)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Mode   string           `json:"mode"`
		Paths  []map[string]any `json:"paths"`
		Config struct {
			Configured  bool             `json:"configured"`
			SourceFiles []map[string]any `json:"source_files"`
		} `json:"config"`
		Healthy bool `json:"healthy"`
	}
	if err := stdjson.Unmarshal(bodyBytes, &view); err != nil {
		t.Fatal(err)
	}
	if view.Mode == "" {
		t.Errorf("mode should be populated")
	}
	if len(view.Paths) < 10 {
		t.Errorf("expected at least 10 path entries; got %d", len(view.Paths))
	}
	for i, p := range view.Paths {
		if _, ok := p["domain"]; !ok {
			t.Errorf("path[%d] missing domain", i)
		}
		if _, ok := p["source"]; !ok {
			t.Errorf("path[%d] missing source", i)
		}
		if _, ok := p["exists"]; !ok {
			t.Errorf("path[%d] missing exists", i)
		}
	}
	if !view.Healthy {
		t.Error("fresh user box should still be healthy (configured=false is not an error)")
	}
	if view.Config.Configured {
		t.Error("fresh box should not be configured")
	}
}

func TestDoctor_RootOverrideMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", tmp)

	root := cli.NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"doctor", "-o", "json"})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	// Verify at least one path reports source=root-override.
	if !strings.Contains(out.String(), `"source": "root-override"`) {
		t.Errorf("expected at least one root-override path:\n%s", out.String())
	}
}

package cli

import (
	"bytes"
	"os"
	"testing"
)

func TestResolveRenderer_FlagWins(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "ndjson")
	r, err := resolveRenderer("json", &bytes.Buffer{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "json" {
		t.Errorf("flag should win; got %q", r.Name())
	}
}

func TestResolveRenderer_EnvFallback(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "ndjson")
	r, err := resolveRenderer("", &bytes.Buffer{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "ndjson" {
		t.Errorf("env should be used when flag empty; got %q", r.Name())
	}
}

func TestResolveRenderer_AutoDetectPipe(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "")
	// A bytes.Buffer is not a *os.File, so isTerminal() must return false
	// and we should default to json.
	r, err := resolveRenderer("", &bytes.Buffer{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "json" {
		t.Errorf("piped stdout should default to json; got %q", r.Name())
	}
}

func TestResolveRenderer_AutoDetectTTY(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "")
	// Open /dev/null which is a character device on Unix.
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	r, err := resolveRenderer("", f, false, "")
	if err != nil {
		t.Fatal(err)
	}
	// /dev/null is a character device, so this should pick text.
	if r.Name() != "text" {
		t.Errorf("character device should pick text; got %q", r.Name())
	}
}

func TestResolveRenderer_UnknownReturnsUsageError(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "")
	if _, err := resolveRenderer("xml", &bytes.Buffer{}, false, ""); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestResolveRenderer_NoColorEnvHonored(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("PG_HARDSTORAGE_OUTPUT", "text")
	r, err := resolveRenderer("text", &bytes.Buffer{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "text" {
		t.Errorf("expected text; got %q", r.Name())
	}
	// We can't easily inspect NoColor without exposing it; smoke test:
	// the NO_COLOR env path executes and doesn't error.
}

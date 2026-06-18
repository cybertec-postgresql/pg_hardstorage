package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestResolveLLMAPIKey_OPENAI_API_KEY_Wins(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "from-env")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "secondary")
	got, err := resolveLLMAPIKey(config.LLMConfig{
		APIKey:     "inline",
		APIKeyFile: "/never-read",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Errorf("got %q, want from-env (OPENAI_API_KEY should win)", got)
	}
}

func TestResolveLLMAPIKey_PG_HARDSTORAGE_LLM_API_KEY_Fallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "from-secondary-env")
	got, err := resolveLLMAPIKey(config.LLMConfig{APIKey: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-secondary-env" {
		t.Errorf("got %q, want from-secondary-env", got)
	}
}

func TestResolveLLMAPIKey_FileWinsOverInline(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "openai.key")
	if err := os.WriteFile(keyFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLLMAPIKey(config.LLMConfig{
		APIKey:     "inline-loses",
		APIKeyFile: keyFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-file" {
		t.Errorf("got %q, want from-file (file should win over inline)", got)
	}
}

func TestResolveLLMAPIKey_FileTrimsWhitespace(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "k")
	if err := os.WriteFile(keyFile, []byte("  sk-padded  \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := resolveLLMAPIKey(config.LLMConfig{APIKeyFile: keyFile})
	if got != "sk-padded" {
		t.Errorf("got %q, want sk-padded (trim should strip whitespace)", got)
	}
}

func TestResolveLLMAPIKey_FileNotFoundReturnsError(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	_, err := resolveLLMAPIKey(config.LLMConfig{APIKeyFile: "/no/such/file"})
	if err == nil {
		t.Error("missing file should error (not silently fall through)")
	}
}

func TestResolveLLMAPIKey_InlineKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	got, err := resolveLLMAPIKey(config.LLMConfig{APIKey: "sk-inline"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-inline" {
		t.Errorf("got %q, want sk-inline", got)
	}
}

func TestResolveLLMAPIKey_NoKeyReturnsEmpty(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	got, err := resolveLLMAPIKey(config.LLMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (caller decides whether absence is fatal)", got)
	}
}

func TestHasOpenAIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PG_HARDSTORAGE_LLM_API_KEY", "")
	if hasOpenAIKey(config.LLMConfig{}) {
		t.Error("no key anywhere → hasOpenAIKey should be false")
	}
	if !hasOpenAIKey(config.LLMConfig{APIKey: "x"}) {
		t.Error("inline key → true")
	}
	if !hasOpenAIKey(config.LLMConfig{APIKeyFile: "/some/path"}) {
		t.Error("api_key_file → true (file existence not checked here)")
	}
	t.Setenv("OPENAI_API_KEY", "x")
	if !hasOpenAIKey(config.LLMConfig{}) {
		t.Error("env var → true")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q, want third", got)
	}
	if got := firstNonEmpty("first", "second", "third"); got != "first" {
		t.Errorf("got %q, want first (order matters)", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("got %q, want empty (no args)", got)
	}
}

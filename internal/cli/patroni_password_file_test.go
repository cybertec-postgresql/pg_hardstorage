package cli

// patroni.password_file was accepted and ignored.
//
// PatroniConfig has declared PasswordFile since the field landed, config
// merge carries it, and `deployment list` names it as one of the "auth
// secrets (password, password_file)" it deliberately does not render.
// Every signal an operator has says it is supported. Nothing read it.
//
// So an operator who kept the password out of the config file — which is
// the only reason the field exists — got cfg.Password == "" and
// therefore WithAuth(user, ""). Patroni answers 401 on exactly the
// mutating endpoints the coordinator needs (switchover, restart), and
// the config looks correct while it happens.
//
// The same pattern is implemented correctly one package over for
// llm.api_key_file (resolveLLMAPIKey), which is what makes this a
// wiring bug rather than an unimplemented convention.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

func TestResolvePatroniPassword_ReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "patroni.pw")
	// Trailing newline is what every editor and `echo` produces; it must
	// not become part of the secret.
	if err := os.WriteFile(p, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePatroniPassword(config.PatroniConfig{User: "operator", PasswordFile: p})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s3cret" {
		t.Errorf("password = %q, want %q (trailing newline must be trimmed)", got, "s3cret")
	}
}

// The bug in its original form: password_file set, nothing else, and the
// client is built with an empty password.
func TestPatroniClientOpts_PasswordFileReachesTheAuthOption(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "patroni.pw")
	if err := os.WriteFile(p, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := patroniClientOpts(config.PatroniConfig{User: "operator", PasswordFile: p})
	if err != nil {
		t.Fatalf("patroniClientOpts: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("got %d client options, want 1 (auth)", len(opts))
	}
	// Assert the observable, not the struct: the credentials must reach
	// the wire. A Patroni that requires basic-auth answers 401 otherwise,
	// which is exactly what an operator using password_file was getting.
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"members":[]}`))
	}))
	defer srv.Close()

	c, err := patroni.NewClient(srv.URL, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Cluster(context.Background()); err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if !gotOK || gotUser != "operator" || gotPass != "s3cret" {
		t.Errorf("request carried basic-auth (%q, %q, present=%v), want (operator, s3cret, true).\n\n"+
			"An empty password here is the whole bug: Patroni returns 401 on the "+
			"switchover and restart endpoints the coordinator relies on, while the "+
			"operator's config names a password_file that looks honoured.",
			gotUser, gotPass, gotOK)
	}
}

// The inline password keeps working, and the file wins when both are set
// (a file is the deliberate, rotatable source; an inline value left
// behind in the config must not silently override it).
func TestResolvePatroniPassword_FilePrecedesInline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "patroni.pw")
	if err := os.WriteFile(p, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePatroniPassword(config.PatroniConfig{
		Password: "from-inline", PasswordFile: p,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "from-file" {
		t.Errorf("password = %q, want the file's value", got)
	}

	got, err = resolvePatroniPassword(config.PatroniConfig{Password: "from-inline"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "from-inline" {
		t.Errorf("inline password = %q, want from-inline", got)
	}
}

// An unreadable password_file must be a hard error, not a fall-back to
// no auth. The operator asked for authentication and said where the
// secret lives; proceeding unauthenticated is how a coordinator fails
// every switchover with a 401 that points nowhere.
func TestResolvePatroniPassword_UnreadableFileIsAnError(t *testing.T) {
	_, err := resolvePatroniPassword(config.PatroniConfig{
		User: "operator", PasswordFile: "/no/such/patroni/secret",
	})
	if err == nil {
		t.Fatal("a missing password_file resolved silently; the client would then be " +
			"built with no password at all")
	}
	if !strings.Contains(err.Error(), "password_file") {
		t.Errorf("error does not name the setting at fault: %v", err)
	}
}

// No auth configured at all stays option-free.
func TestPatroniClientOpts_NoAuthConfigured(t *testing.T) {
	opts, err := patroniClientOpts(config.PatroniConfig{URL: "http://x:8008"})
	if err != nil {
		t.Fatalf("patroniClientOpts: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("got %d options, want 0 when neither user nor password is set", len(opts))
	}
}

package airgap_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

// TestModeOffPermitsEverything: the zero-value policy never
// refuses, regardless of how exotic the URL is. This is the
// safety net for the test suite — any production code that
// inadvertently calls Default() in a non-air-gapped context
// must not start refusing things.
func TestModeOffPermitsEverything(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeOff}
	for _, u := range []string{
		"",
		"https://api.openai.com/v1",
		"https://hooks.slack.com/services/T0/B0/secret",
		"http://10.0.0.5:8080",
		"file:///tmp/foo",
		"not a url at all",
	} {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("ModeOff should permit %q, refused with %v", u, err)
		}
	}
}

// TestStrictAllowsLocalhost: loopback in any of its common
// shapes is the most-common air-gap target (Ollama, vLLM,
// local OTLP collector).
func TestStrictAllowsLocalhost(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	for _, u := range []string{
		"http://localhost:11434/v1",
		"http://127.0.0.1:8080",
		"https://127.0.0.1:9000",
		"http://[::1]:8000",
		"grpc://localhost:4317",
	} {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("strict mode should allow loopback %q, refused: %v", u, err)
		}
	}
}

// TestStrictAllowsPrivate: RFC1918 + RFC4193 ULA + CGNAT.
// These are the addresses an in-perimeter network uses.
func TestStrictAllowsPrivate(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	for _, u := range []string{
		"https://10.0.0.5/v1",
		"http://10.255.255.254:9999",
		"https://172.16.0.1",
		"https://172.31.255.254",
		"http://192.168.1.50:8080",
		"https://[fc00::1]:443",
		"https://[fd12:3456:7890::1]:443",
		"https://100.64.0.1", // Tailscale CGNAT
		"https://100.127.255.254",
		"http://169.254.1.1", // link-local (rare but well-defined)
	} {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("strict mode should allow private IP %q, refused: %v", u, err)
		}
	}
}

// TestStrictRefusesPublicIP: the gate's headline contract.
func TestStrictRefusesPublicIP(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	cases := []string{
		"https://1.1.1.1",
		"https://8.8.8.8/dns",
		"https://[2001:4860:4860::8888]:443",
		"https://9.9.9.9",
	}
	for _, u := range cases {
		err := p.EndpointAllowed(u)
		if err == nil {
			t.Errorf("strict mode should refuse public IP %q, allowed", u)
			continue
		}
		if !errors.Is(err, airgap.ErrEndpointNotAllowed) {
			t.Errorf("strict mode should wrap ErrEndpointNotAllowed for %q, got %T %v", u, err, err)
		}
	}
}

// TestStrictRefusesPublicHostname: hostnames not in allowlist
// are refused without DNS lookup. Operators who want a
// hostname must add it deliberately.
func TestStrictRefusesPublicHostname(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	for _, u := range []string{
		"https://api.openai.com/v1",
		"https://hooks.slack.com/services/abc",
		"https://acme.atlassian.net/rest/api/2/issue",
		"https://example.com",
	} {
		if err := p.EndpointAllowed(u); err == nil {
			t.Errorf("strict mode should refuse public hostname %q, allowed", u)
		}
	}
}

// TestStrictAllowsLocalSchemes: file://, unix:, fd:, stdio:
// are inherently local — no host is involved.
func TestStrictAllowsLocalSchemes(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	for _, u := range []string{
		"file:///var/log/pg_hardstorage/audit.cef",
		"unix:///run/pg_hardstorage/archive.sock",
		"fd://3",
		"stdio:",
	} {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("strict mode should allow local scheme %q, refused: %v", u, err)
		}
	}
}

// TestAllowlistHostMatch: a hostname in the allowlist is
// permitted. host-only entries match any port.
func TestAllowlistHostMatch(t *testing.T) {
	p := airgap.Policy{
		Mode:      airgap.ModeStrict,
		Allowlist: []string{"siem.acme.example.com", "ops-jira.acme.example.com:8443"},
	}
	for _, u := range []string{
		"https://siem.acme.example.com",
		"https://siem.acme.example.com:6514",
		"https://siem.ACME.EXAMPLE.COM",                   // case-insensitive
		"https://ops-jira.acme.example.com:8443/rest/api", // host:port match
	} {
		if err := p.EndpointAllowed(u); err != nil {
			t.Errorf("allowlist should permit %q, refused: %v", u, err)
		}
	}
	// host:port allowlist entry doesn't match a different port.
	if err := p.EndpointAllowed("https://ops-jira.acme.example.com:443/rest/api"); err == nil {
		t.Error("host:port allowlist must not match a different port")
	}
}

// TestParseMode covers the YAML / flag string forms.
func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want airgap.Mode
		err  bool
	}{
		{"", airgap.ModeOff, false},
		{"off", airgap.ModeOff, false},
		{"false", airgap.ModeOff, false},
		{"0", airgap.ModeOff, false},
		{"no", airgap.ModeOff, false},
		{"none", airgap.ModeOff, false},
		{"1", airgap.ModeStrict, false},
		{"true", airgap.ModeStrict, false},
		{"strict", airgap.ModeStrict, false},
		{"yes", airgap.ModeStrict, false},
		{"on", airgap.ModeStrict, false},
		{"STRICT", airgap.ModeStrict, false},
		{"  Strict  ", airgap.ModeStrict, false},
		{"maybe", airgap.ModeOff, true},
	} {
		got, err := airgap.ParseMode(tc.in)
		if tc.err && err == nil {
			t.Errorf("ParseMode(%q): expected error, got %v", tc.in, got)
		}
		if !tc.err && err != nil {
			t.Errorf("ParseMode(%q): unexpected error %v", tc.in, err)
		}
		if !tc.err && got != tc.want {
			t.Errorf("ParseMode(%q): got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestRefusalCarriesURL: refusals must include the offending
// URL in the error message so operators can find the
// misconfiguration without a stack trace.
func TestRefusalCarriesURL(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	err := p.EndpointAllowed("https://api.openai.com/v1/chat/completions")
	if err == nil {
		t.Fatal("expected refusal")
	}
	msg := err.Error()
	for _, want := range []string{"api.openai.com", "airgap"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

// TestSetDefaultRoundtrip: the process-wide hook works.
func TestSetDefaultRoundtrip(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict, Allowlist: []string{"siem.example.com"}})()

	got := airgap.Default()
	if got.Mode != airgap.ModeStrict {
		t.Errorf("WithScope didn't set mode: %v", got.Mode)
	}
	if got.EndpointAllowed("https://siem.example.com") != nil {
		t.Error("scoped policy didn't take effect")
	}
}

// TestUnknownSchemeRefused: an unrecognised scheme should not
// silently pass — the gate is conservative.
func TestUnknownSchemeRefused(t *testing.T) {
	p := airgap.Policy{Mode: airgap.ModeStrict}
	if err := p.EndpointAllowed("ftp://internal.acme.example.com/data"); err == nil {
		t.Error("unknown scheme should be refused")
	}
}

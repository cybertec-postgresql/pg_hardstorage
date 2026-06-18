// Package airgap is the policy gate for outbound endpoints.
//
// pg_hardstorage is designed to run end-to-end inside an air-gapped
// network (regulated finance, classified, strict-data-residency
// shops). The binary itself never phones home — no telemetry, no
// auto-update checks, no Rekor lookups by default — but operators
// configure outbound endpoints (LLM providers, sinks, OTLP
// collectors, control planes, storage backends). When the operator
// declares the binary air-gapped, every one of those endpoints
// must resolve to a host the air-gapped network can actually
// reach: loopback, RFC1918/RFC4193 private space, Tailscale's
// CGNAT range, file://, unix:, or an explicit host allowlist.
//
// This package is the single arbiter. Every code path that opens
// an outbound URL calls Default().EndpointAllowed(rawURL) and
// surfaces the typed error to the operator. The policy is set
// once at startup (PersistentPreRunE) from the merged
// flag/env/config, never mutated thereafter.
//
// # Why not just refuse all outbound?
//
// Air-gapped networks aren't islands. They have local Slack
// replacements, local Jira replacements, local KMS, local LLM
// runtimes (Ollama / vLLM at /v1), local OTLP collectors. The
// gate's job is "this endpoint goes out of the perimeter" not
// "this endpoint exists." Loopback + private + explicit
// allowlist captures the deployment shapes operators actually
// run.
//
// # Resolution precedence
//
//	flag (--airgapped)
//	  > env (PG_HARDSTORAGE_AIRGAPPED=1)
//	    > config (airgapped: true at top level)
//	      > default (off)
//
// PersistentPreRunE merges the three and calls SetDefault once.
// Library code thereafter reads Default() — no mutation, no race.
package airgap

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// Mode is the policy posture.
type Mode int8

const (
	// ModeOff disables the gate. EndpointAllowed always returns nil.
	// This is the default; air-gap is opt-in.
	ModeOff Mode = iota

	// ModeStrict refuses every outbound endpoint that doesn't
	// resolve to loopback, RFC1918/RFC4193 private space, or an
	// explicit host allowlist. Schemes other than http/https/grpc
	// (file, unix, fd, stdio) are always permitted because they're
	// inherently local.
	ModeStrict
)

// String returns the YAML / log representation of the mode.
func (m Mode) String() string {
	switch m {
	case ModeStrict:
		return "strict"
	default:
		return "off"
	}
}

// ParseModeOrOff is a non-erroring wrapper for resolution paths
// where unrecognised input should fall back to ModeOff (env var
// inspection at process start, etc.).
func ParseModeOrOff(s string) Mode {
	m, err := ParseMode(s)
	if err != nil {
		return ModeOff
	}
	return m
}

// ParseMode maps the YAML / flag / env representation back to a
// Mode. Empty input returns ModeOff. Anything truthy
// ("1"/"true"/"strict"/"on"/"yes") returns ModeStrict.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "false", "0", "no", "none":
		return ModeOff, nil
	case "1", "true", "on", "yes", "strict":
		return ModeStrict, nil
	}
	return ModeOff, fmt.Errorf("airgap: unknown mode %q (use \"off\" or \"strict\")", s)
}

// ErrEndpointNotAllowed is wrapped into every refusal so callers
// can distinguish a policy refusal from a network error. The
// wrapped error carries the raw URL and the resolved host for
// the operator's diagnostic.
var ErrEndpointNotAllowed = errors.New("airgap: endpoint not allowed in air-gapped mode")

// Policy is the air-gap configuration in effect. The zero value
// is ModeOff with no allowlist, which permits everything — that's
// the safe default for the test suite and for operators who never
// opt in.
type Policy struct {
	Mode Mode

	// Allowlist holds host names (and host:port) the policy
	// permits even in strict mode. Useful for an operator-private
	// FQDN that resolves outside RFC1918 (e.g. a private VPC
	// endpoint behind a routable hostname). Comparison is
	// case-insensitive on the host portion; port (if present in
	// the entry) must match exactly.
	Allowlist []string
}

// EndpointAllowed reports nil if rawURL is permitted under p,
// or a wrapped ErrEndpointNotAllowed otherwise.
//
// Behaviour:
//   - If p.Mode == ModeOff, always nil.
//   - Empty URL returns nil (the caller's "no endpoint configured"
//     branch is air-gap-clean by definition).
//   - Schemes other than http / https / grpc / grpc+tls are
//     considered local (file, unix, fd, stdio) and always
//     allowed.
//   - For network schemes, the host is parsed and:
//   - loopback (127.0.0.0/8, ::1, "localhost") → allowed
//   - RFC1918 (10/8, 172.16/12, 192.168/16) → allowed
//   - RFC4193 (fc00::/7) IPv6 ULA → allowed
//   - link-local (169.254/16, fe80::/10) → allowed
//   - CGNAT (100.64.0.0/10, used by Tailscale) → allowed
//   - any host in p.Allowlist → allowed
//   - everything else → refused.
//
// Hostnames that are not literal IPs are checked against the
// allowlist; we deliberately do NOT call net.LookupHost because
// the operator's intent is "is this endpoint string air-gap
// clean?" and DNS resolution would (a) make startup slow, (b)
// expose us to DNS poisoning that bypasses the gate, and (c)
// behave differently between dev and production.  A hostname
// that isn't an explicit allowlist hit is refused; operators
// add it deliberately.
func (p Policy) EndpointAllowed(rawURL string) error {
	if p.Mode == ModeOff {
		return nil
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}

	// Parse. URLs that fail parsing in air-gap mode are refused —
	// we don't want a malformed URL to slip through as "scheme
	// unknown".
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %q is not a parseable URL: %v",
			ErrEndpointNotAllowed, rawURL, err)
	}

	// Local-only schemes are always allowed. URLs without a scheme
	// (rare; usually shorthand like "syslog.example.com:6514") are
	// treated as host-only and validated below — falling through
	// to host classification.
	switch strings.ToLower(u.Scheme) {
	case "":
		// No scheme — try host-only parse.
	case "file", "unix", "fd", "stdio":
		return nil
	case "http", "https", "grpc", "grpc+tls", "tcp", "tcp+tls", "tls", "syslog", "syslog+tls":
		// fall through to host classification
	default:
		// Unknown scheme — refuse. Better to surface than to silently allow.
		return fmt.Errorf("%w: scheme %q is not recognised by the air-gap policy (URL %q)",
			ErrEndpointNotAllowed, u.Scheme, rawURL)
	}

	host := u.Hostname()
	port := u.Port()
	if host == "" {
		// Try the rawURL as a host:port (no scheme) shortcut. This is
		// what shows up in syslog configs like "siem.example.com:6514".
		h, prt, splitErr := net.SplitHostPort(rawURL)
		if splitErr == nil {
			host = h
			port = prt
		} else {
			host = rawURL
		}
	}

	// Allowlist check (host or host:port).
	if hostInAllowlist(host, port, p.Allowlist) {
		return nil
	}

	// Loopback name shortcut.
	if strings.EqualFold(host, "localhost") {
		return nil
	}

	// IP literal classification.
	ip := net.ParseIP(host)
	if ip != nil {
		if isPrivateOrLocalIP(ip) {
			return nil
		}
		return fmt.Errorf("%w: host %s is publicly routable (URL %q); add it to airgap.allowlist if it's reachable inside your perimeter",
			ErrEndpointNotAllowed, host, rawURL)
	}

	// Hostname (not literal IP) and not in allowlist — refuse.
	return fmt.Errorf("%w: hostname %q is not in the airgap allowlist (URL %q); add it to airgap.allowlist or use a loopback/RFC1918 address",
		ErrEndpointNotAllowed, host, rawURL)
}

// hostInAllowlist returns true if (host) or (host:port) matches a
// case-insensitive entry. Entries with a port must match the
// caller's port exactly; entries without a port match any port.
func hostInAllowlist(host, port string, allowlist []string) bool {
	hp := host
	if port != "" {
		hp = host + ":" + port
	}
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.EqualFold(entry, host) || strings.EqualFold(entry, hp) {
			return true
		}
	}
	return false
}

// isPrivateOrLocalIP returns true for any IP address that is, by
// IETF assignment, not publicly routable. The list mirrors the
// "doc and example" tables in IANA's IPv4/IPv6 registry plus
// CGNAT (RFC 6598), which Tailscale and similar mesh VPNs use.
func isPrivateOrLocalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() {
		// Go's IsPrivate covers RFC1918 + RFC4193 since 1.17.
		return true
	}
	// CGNAT (100.64.0.0/10) — RFC 6598; commonly used by
	// Tailscale / mesh VPNs, treated as "inside the perimeter".
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
	}
	return false
}

// --- Process-wide default policy (initialised once, read many).
//
// Goroutine-safe via atomic.Value. SetDefault is called once at
// startup from PersistentPreRunE; tests use the With/Reset helpers
// to scope a policy to a single test.

var defaultPolicy atomic.Value // holds Policy

func init() { defaultPolicy.Store(Policy{Mode: envMode()}) }

// envMode returns the Mode implied by the environment, used as
// the seed value before flag/config parsing happens. Operators
// who set PG_HARDSTORAGE_AIRGAPPED=1 in /etc/environment get the
// gate even on commands that bypass installDispatcher (e.g.
// `version`).
func envMode() Mode {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PG_HARDSTORAGE_AIRGAPPED")))
	if v == "" {
		return ModeOff
	}
	m, err := ParseMode(v)
	if err != nil {
		return ModeOff
	}
	return m
}

// Default returns the process-wide policy.
func Default() Policy { return defaultPolicy.Load().(Policy) }

// SetDefault replaces the process-wide policy. Call this once at
// startup; downstream library code reads Default() at use time.
func SetDefault(p Policy) { defaultPolicy.Store(p) }

// withScope is the test helper. Returns a restore function the
// test must defer.
//
//	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()
func WithScope(p Policy) (restore func()) {
	prev := Default()
	SetDefault(p)
	return func() { SetDefault(prev) }
}

// scopeMu is exported for tests that need to sequence overlapping
// scopes; production code never touches it.
var scopeMu sync.Mutex

// LockForTest acquires the scope mutex for the duration of a test
// that needs to set + read the default policy without interleaving
// with another test in the same package. Tests call:
//
//	airgap.LockForTest(t)
//	defer airgap.WithScope(...)()
//
// where t.Cleanup releases the mutex via the returned restore.
func LockForTest(t interface {
	Helper()
	Cleanup(func())
}) {
	t.Helper()
	scopeMu.Lock()
	t.Cleanup(scopeMu.Unlock)
}

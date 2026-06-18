// Package privacy enforces the LLM helper's data-egress
// privacy modes.  The plan defines four:
//
//   - strict     — only error codes / metric names / runbook IDs
//     cross the LLM-provider boundary.  No
//     deployment names, no LSNs, no error-message
//     strings.  For regulated environments where
//     the LLM provider is treated as untrusted.
//   - standard   — metadata + redacted config.  PII detector
//     strips emails, IPs, connection strings, KMS
//     ARNs, S3 URLs with creds, etc.  Default.
//   - open       — everything goes (with credentials always
//     masked).  For dev / staging.
//   - local-only — refuses any provider whose endpoint isn't
//     loopback or RFC-1918 private.  Hard gate;
//     auto-selected when the deployment has
//     data_classification: confidential or higher.
//
// The redactor runs over message Content + ToolResult bodies
// before they leave the host.  System prompts are NOT redacted
// (they're authored by us; the skill template is the only
// content there).  Tool args are redacted because the model
// might pass operator input verbatim into a tool call.
//
// What's NOT in this commit:
//
//   - Per-tenant overrides of the redactor pattern set. +
//     adds a YAML allowlist/denylist alongside the skill files.
//   - Structured-field redaction (e.g. "always strip
//     manifest.encryption.kek_ref before send").  Today the
//     redactor works on the wire bytes.  Adding typed redaction
//     requires a tool-result schema we don't have yet.
//   - Token-stream redaction on the response side.  We assume
//     the model echoes back what it was given; if the response
//     contains data we never sent, the audit chain captures it
//     verbatim and a reviewer can spot the leak.  may add
//     a response-side scrubber.
package privacy

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Mode is one of the four documented privacy levels.
type Mode string

const (
	// ModeStrict permits only error codes, metric names, and
	// runbook IDs across the LLM boundary. For regulated
	// environments treating the LLM provider as untrusted.
	ModeStrict Mode = "strict"
	// ModeStandard permits metadata + redacted config; the PII
	// detector strips emails, IPs, connection strings, KMS ARNs,
	// and S3 URLs with credentials. Default.
	ModeStandard Mode = "standard"
	// ModeOpen permits everything except always-masked credentials.
	// For dev / staging.
	ModeOpen Mode = "open"
	// ModeLocalOnly refuses any provider whose endpoint is not
	// loopback or RFC-1918. Auto-selected for deployments with
	// data_classification confidential or higher.
	ModeLocalOnly Mode = "local-only"
)

// Default is the mode applied when none is configured.
const Default = ModeStandard

// Parse normalises a config-file string to a Mode.  Empty
// returns Default.  Unknown returns an error so a typo doesn't
// silently downgrade to Default.
func Parse(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return Default, nil
	case "strict":
		return ModeStrict, nil
	case "standard":
		return ModeStandard, nil
	case "open":
		return ModeOpen, nil
	case "local-only", "local_only", "localonly":
		return ModeLocalOnly, nil
	default:
		return "", fmt.Errorf("privacy: unknown mode %q (want strict | standard | open | local-only)", s)
	}
}

// EndpointAllowed reports whether the given LLM provider
// endpoint is acceptable under mode m.  local-only refuses any
// endpoint that isn't loopback or RFC-1918 private.  Other
// modes accept anything.
//
// Returns nil when allowed; a structured error otherwise.
func EndpointAllowed(m Mode, endpoint string) error {
	if m != ModeLocalOnly {
		return nil
	}
	if endpoint == "" {
		// No-endpoint = library default = api.openai.com (public).
		return fmt.Errorf("privacy: local-only mode refuses default LLM endpoint (api.openai.com); set llm.endpoint to a loopback / RFC-1918 host")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("privacy: local-only: parse endpoint %q: %w", endpoint, err)
	}
	host := u.Hostname()
	if isLocalHost(host) {
		return nil
	}
	return fmt.Errorf("privacy: local-only mode refuses endpoint %q (host %q is not loopback / RFC-1918 private; only local LLM runtimes are permitted under this mode)", endpoint, host)
}

// Redact applies the mode's content-rewriting rules to s and
// returns the cleaned text.  Modes:
//
//	strict     — replace any structured value (deployment name,
//	             LSN, error message body) with placeholders;
//	             keep only error CODES (matched by the
//	             StructuredErrorCode regex).
//	standard   — strip emails, IPs, connection strings, KMS
//	             ARNs, AWS-style secret-key shaped values.
//	open       — always-mask credentials (api_key=, password=,
//	             kms-secret://) but keep everything else.
//	local-only — same as standard (the egress is to a local
//	             box, but the operator's classification floor
//	             still applies).
//
// Idempotent: redacting an already-redacted string is a no-op.
func Redact(m Mode, s string) string {
	if s == "" {
		return s
	}
	switch m {
	case ModeOpen:
		return redactCredentialsOnly(s)
	case ModeStrict:
		return redactStrict(s)
	case ModeLocalOnly, ModeStandard, "":
		return redactStandard(s)
	default:
		// Unknown mode — fail closed: apply strict redaction
		// rather than risk a typo letting raw data egress.
		return redactStrict(s)
	}
}

// redactCredentialsOnly: only the always-mask patterns fire.
// Emails / IPs / connection strings remain.  Used in `open`
// mode for dev/staging.
func redactCredentialsOnly(s string) string {
	for _, re := range credentialPatterns {
		s = re.re.ReplaceAllString(s, re.replacement)
	}
	return s
}

// redactStandard: credentials + PII.
func redactStandard(s string) string {
	s = redactCredentialsOnly(s)
	for _, re := range piiPatterns {
		s = re.re.ReplaceAllString(s, re.replacement)
	}
	return s
}

// redactStrict: standard + drop everything except structured
// error codes.  We replace any non-allowed token with
// <REDACTED>.  This is aggressive: a strict-mode prompt is
// barely intelligible to the LLM, but the strict-mode operator
// has decided the LLM provider is untrusted infrastructure and
// data-leak prevention beats answer quality.
//
// Implementation: keep error codes (dotted: "restore.target_in_wal_gap"),
// runbook IDs ("R3"), metric names ("pg_hardstorage_*"), and
// pure punctuation.  Replace everything else with <REDACTED>.
//
// Idempotent: tokens that already start with `<REDACTED` (the
// placeholder shape we emit) are preserved verbatim, so a
// second redaction doesn't produce `<<REDACTED>>`.
func redactStrict(s string) string {
	// Start with standard redaction so credentials + PII go.
	s = redactStandard(s)
	// Then aggressively replace word-tokens that aren't on the
	// strict-allowlist.
	out := strictAllowedRe.ReplaceAllStringFunc(s, func(token string) string {
		if isStrictAllowed(token) {
			return token
		}
		// Preserve already-redacted placeholders so we don't
		// double-wrap into <<REDACTED>>.
		if strings.HasPrefix(token, "REDACTED") || strings.HasPrefix(token, "AWS_") ||
			strings.HasPrefix(token, "EMAIL") || strings.HasPrefix(token, "IPV") ||
			strings.HasPrefix(token, "S3_") || strings.HasPrefix(token, "kms-secret") {
			return token
		}
		return "<REDACTED>"
	})
	return out
}

// isStrictAllowed reports whether token survives strict-mode
// redaction.  Allowed: structured error codes (contain a `.`
// and only allowed chars), runbook IDs (R + digit), metric
// names (`pg_hardstorage_*`), short numbers (digits + units).
func isStrictAllowed(token string) bool {
	if structuredErrorCodeRe.MatchString(token) {
		return true
	}
	if runbookRe.MatchString(token) {
		return true
	}
	if metricNameRe.MatchString(token) {
		return true
	}
	if shortNumberRe.MatchString(token) {
		return true
	}
	return false
}

// --- pattern tables --------------------------------------------------

type pattern struct {
	re          *regexp.Regexp
	replacement string
}

var credentialPatterns = []pattern{
	// kms-secret:// URLs MUST fire before the generic api_key
	// pattern so a `secret: kms-secret://...` line keeps the
	// scheme visible to reviewers.
	{regexp.MustCompile(`\bkms-secret://\S+`), "kms-secret://<REDACTED>"},
	// Bearer tokens (Authorization: Bearer ABC...).
	{regexp.MustCompile(`(?i)\b(authorization|bearer)\s*[:= ]+\s*\S+`), "$1=<REDACTED>"},
	// AWS-style access key IDs.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "<AWS_KEY>"},
	// AWS-style secret keys (40 base64-ish chars after a known prefix
	// or aws_secret_access_key= shape).
	{regexp.MustCompile(`(?i)\b(aws[_-]?secret[_-]?access[_-]?key)\s*[:= ]+\s*[A-Za-z0-9+/=]{20,}`), "$1=<REDACTED>"},
	// Generic api_key / password / token in url-encoded form.
	// We deliberately do NOT match a bare `secret` word — too
	// ambiguous when surrounded by other tokens (e.g.
	// `kms-secret://...` would be eaten).  `aws_secret_access_key`
	// has its own dedicated pattern above; other "secret"
	// usages are caught case-by-case.  Stops the value match at
	// `<` so an already-redacted placeholder isn't re-consumed.
	{regexp.MustCompile(`(?i)\b(api[_-]?key|password|api[_-]?token|access[_-]?token|client[_-]?secret)\s*[=:]\s*[^<\s]+`), "$1=<REDACTED>"},
	// PostgreSQL connection strings with passwords.
	{regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:\s]+:)[^@\s]+(@)`), "${1}<REDACTED>${2}"},
	// Sentry-style DSNs.
	{regexp.MustCompile(`\bhttps?://[A-Za-z0-9]+:[^@\s]+@\S+`), "<REDACTED_DSN>"},
}

var piiPatterns = []pattern{
	// Email addresses.
	{regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`), "<EMAIL>"},
	// IPv4 addresses.
	{regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), "<IPV4>"},
	// IPv6 (very rough — anything with colons that looks
	// hexadecimal).
	{regexp.MustCompile(`\b[0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4}){5,}\b`), "<IPV6>"},
	// AWS ARNs.
	{regexp.MustCompile(`\barn:aws:[a-z0-9\-]+:[a-z0-9\-]*:\d{12}:\S+`), "<AWS_ARN>"},
	// AWS S3 URIs that include access creds (bucket aside; the
	// generic match for s3://x/y stays intact).
	{regexp.MustCompile(`\bs3://[A-Za-z0-9]+:[^@\s]+@\S+`), "<S3_WITH_CREDS>"},
}

var (
	// Structured error codes are a dotted lower-snake name.
	structuredErrorCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	// Runbook IDs.
	runbookRe = regexp.MustCompile(`^R[0-9]+$`)
	// pg_hardstorage_* metric names.
	metricNameRe = regexp.MustCompile(`^pg_hardstorage_[a-z0-9_]+$`)
	// Short numbers (and durations like 47s, 14m).
	shortNumberRe = regexp.MustCompile(`^\d+(\.\d+)?(s|m|h|ms|us|ns|GB|MB|KB|B)?$`)
	// Anything that LOOKS LIKE a token to the strict redactor.
	// Matches words / identifiers separated by whitespace and
	// punctuation; we deliberately exclude `.` from the token
	// class so a trailing period (end-of-sentence) doesn't
	// glue onto a number/duration ("47s.") and break the
	// shortNumberRe match.  Hyphens and slashes stay because
	// they appear inside the placeholder shape "kms-secret://".
	strictAllowedRe = regexp.MustCompile(`[A-Za-z0-9_/\-]+(\.[A-Za-z0-9_/\-]+)*`)
)

// isLocalHost reports whether host is loopback or RFC-1918 private.
// Used by EndpointAllowed under local-only.
//
// The host MUST be parsed as an IP address — string-prefix matching
// is a security hole: a public hostname like "10.x.attacker.com" or
// "127.0.0.1.evil.com" textually starts with a private-range prefix
// but resolves to attacker-controlled infrastructure, and local-only
// mode is the hard gate that protects `confidential` deployments.
// A bare (non-IP) hostname is local only when it is exactly
// "localhost".
func isLocalHost(host string) bool {
	low := strings.ToLower(strings.TrimSpace(host))
	if low == "localhost" {
		return true
	}
	ip := net.ParseIP(low)
	if ip == nil {
		// Not an IP literal — a hostname other than localhost.
		// We cannot vouch for where it resolves, so it is not
		// considered local.
		return false
	}
	// IsLoopback covers 127.0.0.0/8 and ::1; IsPrivate covers
	// RFC-1918 (10/8, 172.16/12, 192.168/16) and RFC-4193 ULA
	// (fc00::/7) — the IPv6 equivalent of "private".
	return ip.IsLoopback() || ip.IsPrivate()
}

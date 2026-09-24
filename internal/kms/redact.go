package kms

import (
	"net/url"
	"strings"
)

// redactedValue replaces a secret in a redacted KEKRef.
const redactedValue = "REDACTED"

// secretParams are KEKRef query parameters whose value is (or points
// at) a credential. Compared case-insensitively.
var secretParams = map[string]bool{
	"pin": true, "pin_source": true, "password": true, "passwd": true,
	"secret": true, "secret_id": true, "client_secret": true,
	"token": true, "vault_token": true, "access_key": true, "secret_key": true,
	"credentials": true, "sig": true,
}

// RedactKEKRef returns kekRef fit for an error message, log line or
// audit event: the value of every secret query parameter (pin=,
// pin_source=, token=, ...) and any userinfo password are replaced by
// REDACTED; everything else is kept byte for byte so the reference
// stays recognisable.
//
// A pkcs11 KEKRef may carry the HSM PIN inline (?pin=), and the
// registry and the providers' parsers quoted the whole KEKRef in their
// errors — which reach logs, --json output and audit events. The
// rewrite is string-based rather than url.Parse/String: many KEKRefs
// (aws-kms ARNs, gcp resource paths) are not valid URLs, and a failed
// parse must not become a reason to print the secret.
func RedactKEKRef(kekRef string) string {
	body, frag, hasFrag := strings.Cut(kekRef, "#")
	base, query, hasQuery := strings.Cut(body, "?")
	base = redactUserinfo(base)
	if hasQuery {
		parts := strings.Split(query, "&")
		for i, kv := range parts {
			k, _, hasEq := strings.Cut(kv, "=")
			name, err := url.QueryUnescape(k)
			if err != nil {
				name = k
			}
			if secretParams[strings.ToLower(name)] {
				if hasEq {
					parts[i] = k + "=" + redactedValue
				}
			}
		}
		base += "?" + strings.Join(parts, "&")
	}
	if hasFrag {
		base += "#" + frag
	}
	return base
}

// redactUserinfo masks the password in scheme://user:password@host/...
func redactUserinfo(s string) string {
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	authStart := i + 3
	authEnd := len(s)
	if j := strings.IndexByte(s[authStart:], '/'); j >= 0 {
		authEnd = authStart + j
	}
	auth := s[authStart:authEnd]
	at := strings.LastIndexByte(auth, '@')
	if at < 0 {
		return s
	}
	user, _, hasPass := strings.Cut(auth[:at], ":")
	if !hasPass {
		return s
	}
	return s[:authStart] + user + ":" + redactedValue + auth[at:] + s[authEnd:]
}

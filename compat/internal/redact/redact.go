// Package redact scrubs credentials out of the legacy-config values
// the `compat translate` translators echo back as "unmapped settings".
//
// Those notes go to stderr — terminals, CI logs, journald — while the
// translated YAML itself is written 0600. A WAL-G env file routinely
// carries AWS_SECRET_ACCESS_KEY, a pgbackrest.conf repo1-s3-key-secret
// and repo1-cipher-pass, a barman.conf passwords inside conninfo; the
// review note only needs the key to tell the operator what to look at.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces a redacted value.
const Placeholder = "<redacted>"

// secretKeyFragments are substrings (lower-cased) that mark a config
// key as naming a credential. Deliberately broad: redacting a harmless
// value such as repo1-s3-key-type costs the operator nothing, echoing a
// secret key into a log cannot be undone.
var secretKeyFragments = []string{
	"secret", "password", "passwd", "pass", "token", "credential",
	"private", "key", "auth",
}

// IsSecretKey reports whether key names a credential.
func IsSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, f := range secretKeyFragments {
		if strings.Contains(k, f) {
			return true
		}
	}
	return false
}

var (
	// password=... inside a libpq keyword/value string (quoted or not).
	kvPassword = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)
	// scheme://user:password@host
	urlUserinfo = regexp.MustCompile(`(://[^/:@\s]*:)[^@\s]*@`)
)

// Value returns the value safe to print for key: the placeholder when
// the key itself names a credential, otherwise the value with any
// embedded password (libpq password=, URL userinfo) scrubbed.
func Value(key, value string) string {
	if value == "" {
		return value
	}
	if IsSecretKey(key) {
		return Placeholder
	}
	value = kvPassword.ReplaceAllString(value, "${1}"+Placeholder)
	return urlUserinfo.ReplaceAllString(value, "${1}"+Placeholder+"@")
}

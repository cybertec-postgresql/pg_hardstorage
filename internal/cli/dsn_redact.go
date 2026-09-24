// dsn_redact.go — credential redaction for libpq connection strings (keyword/value and URI forms).
package cli

import (
	"net/url"
	"strings"
)

// redactedValue replaces every secret the redactor finds.
const redactedValue = "****"

// secretDSNKeys are the libpq parameters whose values are credentials.
var secretDSNKeys = map[string]bool{"password": true, "sslpassword": true}

// redactDSN masks the credentials in a libpq connection string so it
// can be shown (`deployment list`, runbooks, init's result). libpq
// accepts two forms and both are handled:
//
//   - URI form:     postgres://user:password@host/db?password=...
//   - Keyword form: host=db1 user=u password=secret dbname=app
//
// The keyword form is tokenized with libpq's own grammar rather than
// searched for "password=": whitespace may surround '=', a quoted value
// may contain \' and \\, and an unquoted value may contain "\ ". A
// substring scan missed `password = secret` and cut `'it\'s secret'` at
// the escaped quote, printing the rest of the password.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return redactDSNURI(dsn)
	}
	return redactDSNKeywords(dsn)
}

// redactDSNURI masks the userinfo password and any password query
// parameter of a postgres:// URI. Parsed by hand: url.Parse rejects the
// multi-host authority (h1:5432,h2:5433) libpq allows.
func redactDSNURI(dsn string) string {
	schemeEnd := strings.Index(dsn, "://") + 3
	rest := dsn[schemeEnd:]
	// The authority ends at the first '/' or '?'; within it the LAST '@'
	// ends the userinfo (an unencoded '@' in a password is common).
	authEnd := strings.IndexAny(rest, "/?")
	if authEnd < 0 {
		authEnd = len(rest)
	}
	authority, tail := rest[:authEnd], rest[authEnd:]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		userinfo := authority[:at]
		if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
			authority = userinfo[:colon+1] + redactedValue + authority[at:]
		}
	}
	if q := strings.IndexByte(tail, '?'); q >= 0 {
		params := strings.Split(tail[q+1:], "&")
		for i, kv := range params {
			k, _, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			if name, err := url.QueryUnescape(k); err == nil && secretDSNKeys[strings.ToLower(name)] {
				params[i] = k + "=" + redactedValue
			}
		}
		tail = tail[:q+1] + strings.Join(params, "&")
	}
	return dsn[:schemeEnd] + authority + tail
}

// redactDSNKeywords walks a keyword/value string with libpq's grammar
// (conninfo_parse) and replaces each secret value, leaving every other
// byte — spacing, quoting, the other parameters — as written. Input that
// does not parse is redacted from the first secret keyword onward: a
// malformed string must not leak through the fallback.
func redactDSNKeywords(s string) string {
	var b strings.Builder
	i := 0
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }
	for i < len(s) {
		// Leading whitespace.
		start := i
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		b.WriteString(s[start:i])
		if i >= len(s) {
			break
		}
		// Keyword: up to '=' or whitespace.
		kStart := i
		for i < len(s) && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		keyword := s[kStart:i]
		b.WriteString(keyword)
		// Optional whitespace, then '='.
		sep := i
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			// Not keyword=value: libpq would reject this. Emit the rest
			// untouched unless a secret could be in it.
			return b.String() + redactTailIfSecret(s[sep:])
		}
		i++ // '='
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		b.WriteString(s[sep:i])
		// Value: quoted or unquoted, both with backslash escapes.
		vStart := i
		quoted := i < len(s) && s[i] == '\''
		if quoted {
			i++
			for i < len(s) && s[i] != '\'' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				i++
			}
			if i < len(s) {
				i++ // closing quote
			}
		} else {
			for i < len(s) && !isSpace(s[i]) {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				i++
			}
		}
		value := s[vStart:i]
		if secretDSNKeys[strings.ToLower(keyword)] {
			if quoted {
				value = "'" + redactedValue + "'"
			} else {
				value = redactedValue
			}
		}
		b.WriteString(value)
	}
	return b.String()
}

// redactTailIfSecret is the fallback for an unparseable remainder.
func redactTailIfSecret(tail string) string {
	low := strings.ToLower(tail)
	for k := range secretDSNKeys {
		if idx := strings.Index(low, k); idx >= 0 {
			return tail[:idx] + redactedValue
		}
	}
	return tail
}

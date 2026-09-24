// dsn_redact.go — credential handling for libpq connection strings (keyword/value and URI forms).
package cli

import (
	"net/url"
	"strings"
)

// redactedValue replaces every secret the redactor finds.
const redactedValue = "****"

// secretDSNKeys are the libpq parameters whose values are credentials.
var secretDSNKeys = map[string]bool{"password": true, "sslpassword": true}

// isDSNURI reports whether dsn is libpq's URI form.
func isDSNURI(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

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
	if isDSNURI(dsn) {
		out, _ := rewriteDSNURI(dsn, true, func(string) (string, bool) { return redactedValue, true })
		return out
	}
	pairs, rest := tokenizeDSN(dsn)
	var b strings.Builder
	last := 0
	for _, p := range pairs {
		if !secretDSNKeys[strings.ToLower(p.key)] {
			continue
		}
		b.WriteString(dsn[last:p.valStart])
		if p.quoted {
			b.WriteString("'" + redactedValue + "'")
		} else {
			b.WriteString(redactedValue)
		}
		last = p.end
	}
	b.WriteString(dsn[last:rest])
	// Anything the tokenizer could not parse is redacted from the first
	// secret keyword onward: a malformed string must not leak through.
	tail := dsn[rest:]
	low := strings.ToLower(tail)
	for k := range secretDSNKeys {
		if idx := strings.Index(low, k); idx >= 0 {
			tail = tail[:idx] + redactedValue
			break
		}
	}
	return b.String() + tail
}

// splitDSNPassword removes the password from dsn and returns it
// separately, so a child process (psql) can receive it through
// PGPASSWORD instead of its argv — argv is world-readable via ps and
// /proc/<pid>/cmdline. ok is false when dsn carries no password.
func splitDSNPassword(dsn string) (stripped, password string, ok bool) {
	if isDSNURI(dsn) {
		var pw string
		out, found := rewriteDSNURI(dsn, false, func(v string) (string, bool) {
			pw = v
			return "", false
		})
		return out, pw, found
	}
	pairs, _ := tokenizeDSN(dsn)
	var b strings.Builder
	last := 0
	for _, p := range pairs {
		if strings.ToLower(p.key) != "password" {
			continue
		}
		b.WriteString(dsn[last:p.start])
		last = p.end
		password, ok = p.value(dsn), true
	}
	b.WriteString(dsn[last:])
	return strings.TrimSpace(b.String()), password, ok
}

// dsnPair is one keyword=value of the keyword form, as byte offsets.
type dsnPair struct {
	key                  string
	start, valStart, end int
	quoted               bool
}

// value returns the pair's value with libpq's quoting and backslash
// escapes removed.
func (p dsnPair) value(s string) string {
	raw := s[p.valStart:p.end]
	if p.quoted {
		raw = strings.TrimPrefix(raw, "'")
		raw = strings.TrimSuffix(raw, "'")
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			i++
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

// tokenizeDSN walks a keyword/value string with libpq's grammar
// (conninfo_parse) and returns each pair's offsets, plus the offset where
// parsing stopped (len(s) when the whole string parsed).
func tokenizeDSN(s string) (pairs []dsnPair, stop int) {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return pairs, len(s)
		}
		p := dsnPair{start: i}
		for i < len(s) && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		p.key = s[p.start:i]
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			// Not keyword=value: libpq would reject the string.
			return pairs, p.start
		}
		i++ // '='
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		p.valStart = i
		if i < len(s) && s[i] == '\'' {
			p.quoted = true
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
		p.end = i
		pairs = append(pairs, p)
	}
}

// rewriteDSNURI applies fn to every credential in a postgres:// URI —
// the userinfo password and password/sslpassword query parameters.
// fn receives the decoded value and returns the replacement and whether
// to keep the credential (false drops it). redactSSL masks sslpassword
// too (display); splitting leaves it intact so the DSN still connects. Parsed by hand: url.Parse rejects the multi-host authority
// (h1:5432,h2:5433) libpq allows. found reports whether a password
// (not sslpassword) was seen.
func rewriteDSNURI(dsn string, redactSSL bool, fn func(value string) (string, bool)) (out string, found bool) {
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
			found = true
			pw := userinfo[colon+1:]
			if dec, err := url.PathUnescape(pw); err == nil {
				pw = dec
			}
			if repl, keep := fn(pw); keep {
				authority = userinfo[:colon+1] + repl + authority[at:]
			} else {
				authority = userinfo[:colon] + authority[at:]
			}
		}
	}
	if q := strings.IndexByte(tail, '?'); q >= 0 {
		var kept []string
		for _, kv := range strings.Split(tail[q+1:], "&") {
			k, v, hasValue := strings.Cut(kv, "=")
			name, err := url.QueryUnescape(k)
			if !hasValue || err != nil || !secretDSNKeys[strings.ToLower(name)] {
				kept = append(kept, kv)
				continue
			}
			isPassword := strings.EqualFold(name, "password")
			if !isPassword {
				// sslpassword is only ever redacted, never split out.
				if redactSSL {
					kv = k + "=" + redactedValue
				}
				kept = append(kept, kv)
				continue
			}
			found = true
			if dec, err := url.QueryUnescape(v); err == nil {
				v = dec
			}
			if repl, keep := fn(v); keep {
				kept = append(kept, k+"="+repl)
			}
		}
		tail = tail[:q]
		if len(kept) > 0 {
			tail += "?" + strings.Join(kept, "&")
		}
	}
	return dsn[:schemeEnd] + authority + tail, found
}

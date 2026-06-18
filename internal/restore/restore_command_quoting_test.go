// restore_command_quoting_test.go — the restore_command quoting chain.
//
// restore_command is executed by PostgreSQL as a shell command during
// recovery, so its quoting is security-critical. It passes through
// four stages:
//
//	walfetchcmd.ShellQuote  — POSIX single-quote each argument
//	walfetchcmd.Build       — assemble the shell script
//	quoteSQL                — escape for postgresql.auto.conf
//	(PG)                    — un-escape the .conf value, run via /bin/sh
//
// This was validated end-to-end against real PG 16 + a real shell with
// adversarial repo URLs (single quotes, backslashes, $/backtick, a
// `'; touch …; echo '` injection, &, spaces, quotes): every URL
// reached the agent's --repo argument unchanged and nothing was
// injected. This test is the portable regression: it models the
// .conf un-escape PG performs (verified empirically: `\\`→`\`,
// `”`→`'`, `\n`/`\r`/`\0` control forms, unknown `\x`→`x`) and
// asserts quoteSQL is a lossless inverse — i.e. PG reconstructs
// Build's exact shell script, so the URL stays one shell token.
package restore

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// pgUnescapeConfValue models how PostgreSQL's configuration-file lexer
// un-escapes the inner content of a single-quoted value, for the
// escape forms quoteSQL emits. Empirically verified against PG 16.
func pgUnescapeConfValue(inner string) string {
	var b []byte
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\\' && i+1 < len(inner) {
			switch inner[i+1] {
			case '\\':
				b = append(b, '\\')
			case 'n':
				b = append(b, '\n')
			case 'r':
				b = append(b, '\r')
			case '0':
				b = append(b, 0)
			default:
				b = append(b, inner[i+1]) // unknown escape: backslash dropped
			}
			i++
			continue
		}
		if c == '\'' && i+1 < len(inner) && inner[i+1] == '\'' {
			b = append(b, '\'')
			i++
			continue
		}
		b = append(b, c)
	}
	return string(b)
}

func TestRestoreCommand_QuotingRoundTrips(t *testing.T) {
	urls := map[string]string{
		"plain_s3":     "s3://bucket/path?a=1&b=2",
		"single_quote": "s3://bucket/o'brien",
		"backslash":    `file:///srv/back\slash`,
		"dollar_btick": "s3://b/$HOME/`whoami`",
		"injection":    "x'; touch /tmp/pwned; echo '",
		"space_amp":    "s3://b/a b&c",
		"dquote":       `s3://b/"quoted"`,
		"newline":      "s3://b/line1\nline2",
		"trailing_bs":  `s3://b/dir\`,
	}
	for label, url := range urls {
		t.Run(label, func(t *testing.T) {
			cmd := walfetchcmd.Build("/usr/local/bin/pg_hardstorage", "dep1", url)
			quoted := quoteSQL(cmd) // "'" + inner + "'"
			if quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
				t.Fatalf("quoteSQL output not single-quoted: %q", quoted)
			}
			inner := quoted[1 : len(quoted)-1]

			// PG must reconstruct Build's exact script.
			got := pgUnescapeConfValue(inner)
			if got != cmd {
				t.Errorf("PG would NOT reconstruct the shell script:\n  built: %q\n  pg got: %q", cmd, got)
			}

			// And the reconstructed script must still contain the
			// ShellQuote'd URL verbatim (so the shell sees one token).
			shellTok := walfetchcmd.ShellQuote(url)
			if !contains(got, shellTok) {
				t.Errorf("reconstructed script lost the shell-quoted URL token %q:\n%s", shellTok, got)
			}

			// The inner .conf value must never end in an odd run of
			// backslashes — that would escape the closing quote and
			// make PG FATAL at config load.
			n := 0
			for i := len(inner) - 1; i >= 0 && inner[i] == '\\'; i-- {
				n++
			}
			if n%2 != 0 {
				t.Errorf("inner value ends in odd backslash run (%d) — escapes closing quote", n)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

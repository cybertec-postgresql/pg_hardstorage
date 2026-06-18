// recovery_render_backslash_test.go — backslash handling in quoted
// auto.conf values.
//
// PostgreSQL's configuration-file lexer processes C-style backslash
// escapes inside single-quoted values (verified against PG 16:
// `\\`→`\`, `\t`→tab, and a lone backslash before the closing quote
// makes PG FATAL at config load). So quoteSQL MUST double every
// backslash. These cases pin the rendered auto.conf bytes; each
// `wantGUC` is the exact line that real PG parses back to the original
// value — losing this means Windows restore_command paths get
// corrupted and a --to-name ending in `\` stops the cluster from
// starting.
package restore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestWriteRecoveryFiles_BackslashEscaping(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		wantGUC string
	}{
		// Windows-style path: every backslash doubled → PG yields the
		// original path back.
		{"windows_path", `C:\Users\pg`, `recovery_target_name = 'C:\\Users\\pg'`},
		// Trailing backslash: doubled so it can't escape the closing
		// quote (the case that made PG FATAL before the fix).
		{"trailing_backslash", `backup\`, `recovery_target_name = 'backup\\'`},
		// Literal backslash-t must survive as backslash-t, not a tab.
		{"literal_bs_t", `a\tb`, `recovery_target_name = 'a\\tb'`},
		// Two literal backslashes → four in the file.
		{"double_backslash", `a\\b`, `recovery_target_name = 'a\\\\b'`},
		// Backslash + quote together: quote doubled, backslash doubled.
		{"backslash_and_quote", `o'\brien`, `recovery_target_name = 'o''\\brien'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
				Enable:         true,
				RestoreCommand: "x",
				TargetName:     c.target,
				Inclusive:      true,
			}); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
			if !strings.Contains(string(body), c.wantGUC) {
				t.Errorf("backslash render mismatch.\n  want: %s\n  got:\n%s", c.wantGUC, body)
			}
			// A backslash immediately before the closing quote would
			// escape it and break PG's parser. After doubling, the
			// value must always end in an EVEN run of backslashes
			// before the terminating quote.
			line := gucLine(string(body), "recovery_target_name")
			if line == "" {
				t.Fatalf("no recovery_target_name line:\n%s", body)
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(
				strings.SplitN(line, "=", 2)[1]), " '"), "'")
			n := 0
			for i := len(inner) - 1; i >= 0 && inner[i] == '\\'; i-- {
				n++
			}
			if n%2 != 0 {
				t.Errorf("value ends in an ODD run of %d backslashes — would escape the closing quote: %q", n, inner)
			}
		})
	}
}

// TestWriteRecoveryFiles_RestoreCommandBackslash guards the
// restore_command path (where Windows agent binary paths land) the
// same way — it routes through quoteSQL too.
func TestWriteRecoveryFiles_RestoreCommandBackslash(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: `C:\pg\bin\pg_hardstorage.exe wal fetch db1 %f %p`,
		TargetLSN:      "0/3000028",
		Inclusive:      true,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	want := `restore_command = 'C:\\pg\\bin\\pg_hardstorage.exe wal fetch db1 %f %p'`
	if !strings.Contains(string(body), want) {
		t.Errorf("restore_command backslashes not doubled.\n  want: %s\n  got:\n%s", want, body)
	}
}

func gucLine(body, key string) string {
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), key+" =") {
			return ln
		}
	}
	return ""
}

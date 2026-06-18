// recovery_render_unicode_test.go — non-ASCII restore-point names.
//
// `--to-name` is a free-form PostgreSQL named-restore-point label, so
// operators in any locale may use it: Arabic (RTL), Cyrillic, CJK,
// emoji, combining diacritics. The name flows verbatim into
// recovery_target_name via quoteSQL; these tests pin that UTF-8 is
// preserved byte-for-byte and the auto.conf stays valid UTF-8, and
// that an embedded single quote is still doubled correctly when
// surrounded by multibyte runes.
package restore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestWriteRecoveryFiles_UnicodeTargetName(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		wantGUC string // expected recovery_target_name literal
	}{
		{"arabic", "نسخة-احتياطية", "recovery_target_name = 'نسخة-احتياطية'"},
		{"russian", "резервная-копия", "recovery_target_name = 'резервная-копия'"},
		{"chinese", "备份点-生产", "recovery_target_name = '备份点-生产'"},
		{"japanese", "復元ポイント", "recovery_target_name = '復元ポイント'"},
		{"emoji", "restore-✅-🚀", "recovery_target_name = 'restore-✅-🚀'"},
		{"combining", "café-naïve", "recovery_target_name = 'café-naïve'"},
		{"mixed_scripts", "بيانات-данные-数据", "recovery_target_name = 'بيانات-данные-数据'"},
		// Embedded single quote must double, multibyte runes intact.
		{"embedded_quote_cjk", "o'brien's-點", "recovery_target_name = 'o''brien''s-點'"},
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
			body, err := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.Valid(body) {
				t.Errorf("postgresql.auto.conf is not valid UTF-8 for %q", c.target)
			}
			if !strings.Contains(string(body), c.wantGUC) {
				t.Errorf("missing GUC line.\n  want: %s\n  got:\n%s", c.wantGUC, body)
			}
		})
	}
}

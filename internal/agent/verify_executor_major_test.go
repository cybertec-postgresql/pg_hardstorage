package agent

import (
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// Mirror of the regression in internal/cli/verify_full_major_test.go:
// manifests store the plain major (pg_version=17), and the pre-fix
// helper divided it to 0 → pg.DefaultSandboxMajor — so every
// agent-scheduled verify ran the wrong-major sandbox and failed
// healthy backups with "pg_control: CRC is incorrect".
func TestPGMajorFromManifestVersion(t *testing.T) {
	fallback := fmt.Sprintf("%d", pg.DefaultSandboxMajor)
	cases := []struct {
		in   int
		want string
	}{
		{17, "17"},     // the regression: plain major
		{16, "16"},     // plain major, older
		{170004, "17"}, // server_version_num style
		{0, fallback},  // unparseable → default
	}
	for _, tc := range cases {
		if got := pgMajorFromManifestVersion(tc.in); got != tc.want {
			t.Errorf("pgMajorFromManifestVersion(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

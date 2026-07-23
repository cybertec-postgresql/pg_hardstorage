package cli

import (
	"strconv"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// The manifest's pg_version field stores the PLAIN MAJOR (the runner
// writes probeVersion().Major, e.g. 17) — NOT PG_VERSION_NUM. The
// pre-fix helper divided every value by 10000, so pg_version=17
// resolved to major 0 → the pg.DefaultSandboxMajor fallback, and
// verify --full ran a PG18 pg_verifybackup against every PG17 backup:
// "pg_control: CRC is incorrect" on provably-healthy backups. Caught
// by the chaos soak's restore-proof gate (every committed backup
// failed the full-verify leg while `recovery drill`, which uses the
// manifest major directly, passed the same backups).
func TestPGMajorFromManifestVersion(t *testing.T) {
	fallback := strconv.Itoa(pg.DefaultSandboxMajor)
	cases := []struct {
		name string
		in   int
		want string
	}{
		// The regression: plain majors, as the runner writes them.
		{"plain_major_17", 17, "17"},
		{"plain_major_16", 16, "16"},
		{"plain_major_18", 18, "18"},
		// server_version_num style still divides.
		{"version_num_17", 170004, "17"},
		{"version_num_16", 160011, "16"},
		// Unparseable → the sandbox default.
		{"zero", 0, fallback},
		{"negative", -1, fallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgMajorFromManifestVersion(tc.in); got != tc.want {
				t.Errorf("pgMajorFromManifestVersion(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

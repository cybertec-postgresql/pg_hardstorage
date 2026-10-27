package sandbox

import (
	"strings"
	"testing"
)

// The sandbox boots a restored copy of the source's config; it must not
// be able to archive into the source's WAL archive.
func TestBuildSandboxAutoConf_DisablesArchiving(t *testing.T) {
	for _, existing := range [][]byte{nil, []byte("archive_mode = 'on'\narchive_command = 'x %p'\n")} {
		out := string(buildSandboxAutoConf(existing, "/s"))
		i := strings.LastIndex(out, "archive_mode")
		if i < 0 || !strings.HasPrefix(out[i:], "archive_mode = off") {
			t.Errorf("last archive_mode setting is not off:\n%s", out)
		}
	}
}

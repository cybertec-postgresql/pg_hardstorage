package restore

import (
	"strings"
	"testing"
)

// The skip warning's hint must match the skip's reason. For a host that
// lacks an extension the source cluster preloaded, "install
// postgresql-client/server" is wrong: PostgreSQL is installed.
func TestPostverifySkipHintMatchesReason(t *testing.T) {
	lib := postverifySkipHint(`the restored cluster preloads "bg_mon", which is not installed on this host`)
	if strings.Contains(lib, "postgresql-client") || !strings.Contains(lib, "extension") {
		t.Errorf("missing-extension skip got the wrong advice: %q", lib)
	}
	tools := postverifySkipHint("pg_ctl not found on PATH")
	if !strings.Contains(tools, "postgresql-client") {
		t.Errorf("missing-tools skip lost its install advice: %q", tools)
	}
}

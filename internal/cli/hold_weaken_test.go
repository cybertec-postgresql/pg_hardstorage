package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// `hold add --until 1d` on a backup under an indefinite legal hold
// silently replaced it with a one-day hold. It must refuse with
// conflict.hold_exists, and --force must replace it with an audit record
// of what was replaced.
func TestHoldAdd_RefusesToWeakenIndefiniteHold(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("body"))
	if err := w.store.PutHold(context.Background(), "db1", id, "legal", "litigation #7"); err != nil {
		t.Fatal(err)
	}

	stdout, errb, exit := runCLI(t, "hold", "add", "db1", id, "--repo", w.repoURL,
		"--holder", "legal", "--until", "1d", "-o", "json")
	if exit != int(output.ExitConflict) || !strings.Contains(stdout+errb, "conflict.hold_exists") {
		t.Fatalf("weakening re-add: exit=%d, want %d conflict.hold_exists\n%s\n%s", exit, output.ExitConflict, stdout, errb)
	}
	h, err := w.store.GetHold(context.Background(), "db1", id)
	if err != nil || h.ExpiresAt != nil {
		t.Fatalf("indefinite hold was weakened: %+v %v", h, err)
	}

	if _, errb, exit := runCLI(t, "hold", "add", "db1", id, "--repo", w.repoURL,
		"--holder", "legal", "--until", "1d", "--force", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("--force: exit=%d\n%s", exit, errb)
	}
	out, _, _ := runCLI(t, "audit", "search", "--repo", w.repoURL, "--action", "hold.replace", "-o", "json")
	if !strings.Contains(out, `"count": 1`) {
		t.Errorf("forced replace left no hold.replace audit record:\n%s", out)
	}
}

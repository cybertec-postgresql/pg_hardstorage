// recovery_render_dst_test.go — end-to-end DST check: a DST-affected
// operator-typed --to value must flow through naturaltime.Parse and
// land in postgresql.auto.conf as the correct UTC instant with an
// explicit +00 offset, so PG (whatever its `timezone` GUC) resolves
// the PITR stop point unambiguously.
package restore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

func TestWriteRecoveryFiles_DSTLocalTarget_RendersCorrectUTC(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin tzdata unavailable: %v", err)
	}

	cases := []struct {
		name    string
		ref     time.Time
		in      string
		wantGUC string // expected recovery_target_time literal (UTC, +00)
	}{
		{
			// Day after spring-forward (Sun 2026-03-29). "yesterday
			// noon" is noon CEST (+02) → 10:00 UTC. A bug that applied
			// today's offset or computed the date in UTC would drift.
			name:    "yesterday noon after spring-forward",
			ref:     time.Date(2026, 3, 30, 9, 0, 0, 0, berlin),
			in:      "yesterday 12:00",
			wantGUC: "recovery_target_time = '2026-03-29 10:00:00+00'",
		},
		{
			// Day after fall-back (Sun 2026-10-25). "yesterday noon" is
			// noon CET (+01) → 11:00 UTC.
			name:    "yesterday noon after fall-back",
			ref:     time.Date(2026, 10, 26, 9, 0, 0, 0, berlin),
			in:      "yesterday 12:00",
			wantGUC: "recovery_target_time = '2026-10-25 11:00:00+00'",
		},
		{
			// "today 09:00" in summer time (CEST, +02) → 07:00 UTC.
			name:    "today morning CEST",
			ref:     time.Date(2026, 7, 1, 15, 0, 0, 0, berlin),
			in:      "today 09:00",
			wantGUC: "recovery_target_time = '2026-07-01 07:00:00+00'",
		},
		{
			// "today 09:00" in winter time (CET, +01) → 08:00 UTC.
			name:    "today morning CET",
			ref:     time.Date(2026, 1, 15, 15, 0, 0, 0, berlin),
			in:      "today 09:00",
			wantGUC: "recovery_target_time = '2026-01-15 08:00:00+00'",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Parse exactly as runRestore / the agent executor do:
			// reference is the operator's local "now".
			target, perr := naturaltime.Parse(c.in, c.ref)
			if perr != nil {
				t.Fatalf("parse %q: %v", c.in, perr)
			}
			dir := t.TempDir()
			if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
				Enable:         true,
				RestoreCommand: "x",
				TargetTime:     target,
				Inclusive:      true,
			}); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
			if !strings.Contains(string(body), c.wantGUC) {
				t.Errorf("%q from %s:\n  want GUC: %s\n  got auto.conf:\n%s",
					c.in, c.ref.Format(time.RFC3339), c.wantGUC, body)
			}
			// Defence-in-depth: the rendered literal must carry an
			// explicit offset (never a bare timestamp PG would read in
			// its own timezone GUC).
			if !strings.Contains(string(body), "+00'") {
				t.Errorf("recovery_target_time missing explicit +00 offset:\n%s", body)
			}
		})
	}
}

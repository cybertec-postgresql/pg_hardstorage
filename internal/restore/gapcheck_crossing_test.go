package restore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression (M97): an LSN target BEYOND a recorded gap was accepted
// even when the chosen backup stopped BEFORE the gap, although replay
// from that backup has to cross the hole: PG stops at the gap start,
// cannot tell it from end-of-archive, and promotes short of the
// target. Refuse when stop < gap_end <= target, whatever the source.
func TestPreflightWALGap_TargetBeyondGapCrossedFromBackup_Refuses(t *testing.T) {
	cases := []struct {
		name, stop, target string
		refuse             bool
	}{
		{"backup-before-gap/target-after", "0/50", "0/300", true},
		{"backup-before-gap/target-at-end", "0/50", "0/200", true},
		{"backup-inside-gap/target-after", "0/150", "0/300", true},
		{"backup-at-gap-end/target-after", "0/200", "0/300", false},
		{"backup-after-gap/target-after", "0/250", "0/300", false},
		{"backup-before-gap/target-before", "0/50", "0/80", false},
	}
	for _, src := range []string{"live", "manifest"} {
		for _, tc := range cases {
			t.Run(src+"/"+tc.name, func(t *testing.T) {
				sp := newGapTestSP(t)
				var mg []backup.WALGap
				if src == "live" {
					putGap(t, sp, "db1", 1, "0/100", "0/200", 256, time.Now().UTC())
				} else {
					mg = []backup.WALGap{{GapStartLSN: "0/100", GapEndLSN: "0/200", GapBytes: 256, Timeline: 1}}
				}
				rec := &Recovery{Enable: true, TargetLSN: tc.target}
				err := preflightWALGap(context.Background(), sp, "db1", tc.stop, rec, mg, nil)
				if !tc.refuse {
					if err != nil {
						t.Fatalf("want allowed, got %v", err)
					}
					return
				}
				var oe *output.Error
				if !errors.As(err, &oe) || oe.Code != "restore.target_in_wal_gap" {
					t.Fatalf("want restore.target_in_wal_gap, got %v", err)
				}
				// The remedy must be one that works from here: a target
				// above the gap needs a backup that stopped at/after it.
				if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Human, "backup") {
					t.Errorf("suggestion does not point at a later backup: %+v", oe.Suggestion)
				}
			})
		}
	}
}

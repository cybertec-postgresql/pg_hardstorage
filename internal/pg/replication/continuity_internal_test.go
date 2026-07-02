package replication

import (
	"testing"

	"github.com/jackc/pglogrepl"
)

// TestPopulateGap_SharedByFoundAndRecreated verifies the gap-decision
// logic that BOTH the SlotFound and SlotRecreated paths now route
// through (bug #14 — data-integrity critical). A found slot whose
// restart_lsn is ahead of last-confirmed must report a gap exactly like
// a recreated one; previously the SlotFound path returned GapBytes=0
// unconditionally, masking a real WAL hole.
func TestPopulateGap_SharedByFoundAndRecreated(t *testing.T) {
	mustLSN := func(s string) pglogrepl.LSN {
		l, err := pglogrepl.ParseLSN(s)
		if err != nil {
			t.Fatalf("ParseLSN(%q): %v", s, err)
		}
		return l
	}

	cases := []struct {
		name          string
		restartLSN    string
		lastConfirmed pglogrepl.LSN
		wantGapBytes  uint64
		wantEnd       pglogrepl.LSN
		wantErr       bool
	}{
		{
			// The masked-hole scenario: Patroni presented a slot whose
			// restart_lsn is AHEAD of what we archived. Must report a gap.
			name:          "restart_ahead_reports_gap",
			restartLSN:    "0/5000000",
			lastConfirmed: mustLSN("0/3000000"),
			wantGapBytes:  0x2000000,
			wantEnd:       mustLSN("0/5000000"),
		},
		{
			name:          "restart_behind_no_gap",
			restartLSN:    "0/2000000",
			lastConfirmed: mustLSN("0/3000000"),
			wantGapBytes:  0,
			wantEnd:       mustLSN("0/2000000"),
		},
		{
			name:          "restart_equal_no_gap",
			restartLSN:    "0/3000000",
			lastConfirmed: mustLSN("0/3000000"),
			wantGapBytes:  0,
			wantEnd:       mustLSN("0/3000000"),
		},
		{
			// Fresh follower: no prior position -> gap undefined -> zero.
			name:          "no_prior_position_zero",
			restartLSN:    "0/5000000",
			lastConfirmed: 0,
			wantGapBytes:  0,
			wantEnd:       mustLSN("0/5000000"),
		},
		{
			// Empty restart_lsn: nothing to compare, no gap, no error.
			name:          "empty_restart_lsn_noop",
			restartLSN:    "",
			lastConfirmed: mustLSN("0/3000000"),
			wantGapBytes:  0,
			wantEnd:       0,
		},
		{
			name:          "unparseable_restart_lsn_errors",
			restartLSN:    "not-an-lsn",
			lastConfirmed: mustLSN("0/3000000"),
			wantErr:       true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := &SlotContinuityResult{}
			info := &SlotInfo{RestartLSN: c.restartLSN}
			err := populateGap(res, info, c.lastConfirmed)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("populateGap: %v", err)
			}
			if res.GapBytes != c.wantGapBytes {
				t.Errorf("GapBytes = %d, want %d", res.GapBytes, c.wantGapBytes)
			}
			if res.GapEndLSN != c.wantEnd {
				t.Errorf("GapEndLSN = %s, want %s", res.GapEndLSN, c.wantEnd)
			}
			if c.wantGapBytes > 0 && res.GapStartLSN != c.lastConfirmed {
				t.Errorf("GapStartLSN = %s, want %s", res.GapStartLSN, c.lastConfirmed)
			}
		})
	}
}

// TestPopulateGap_HasGapWiring confirms the HasGap() convenience the
// leader-follow loop uses to fan out wal_gap_detected reflects a gap
// found on the (previously silent) SlotFound path.
func TestPopulateGap_HasGapWiring(t *testing.T) {
	last, _ := pglogrepl.ParseLSN("0/3000000")
	res := &SlotContinuityResult{Outcome: SlotFound}
	if err := populateGap(res, &SlotInfo{RestartLSN: "0/9000000"}, last); err != nil {
		t.Fatalf("populateGap: %v", err)
	}
	if !res.HasGap() {
		t.Fatalf("HasGap() = false; a found slot ahead of last-confirmed must report a gap (bug #14)")
	}
}

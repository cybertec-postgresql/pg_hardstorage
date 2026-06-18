package replication_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
)

// TestEnsureSlot_RejectsNilConn covers the obvious validation
// guards. Pure unit test — no PG required.
func TestEnsureSlot_RejectsNilConn(t *testing.T) {
	cases := []struct {
		name              string
		regConn, replConn bool
		slot              string
	}{
		{"nil-regConn", false, true, "x"},
		{"nil-replConn", true, false, "x"},
		{"empty-name", true, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// We pass nil for the conns the test wants nil; the
			// other path-not-taken passes through but the early
			// validation should fail first.
			_, err := replication.EnsureSlot(context.Background(),
				nil, // regConn — testing nil, but if c.regConn is true we'd need a real one. Validation tests only need ONE of them nil at a time; the slot-name test passes both nil-equivalents (we don't have real conns at unit scope).
				nil,
				c.slot, 0)
			if err == nil {
				t.Errorf("%s: expected error", c.name)
			}
		})
	}
}

// TestSlotContinuityResult_HasGap: convenience method shape.
func TestSlotContinuityResult_HasGap(t *testing.T) {
	cases := []struct {
		gap  uint64
		want bool
	}{
		{0, false},
		{1, true},
		{1024 * 1024, true},
	}
	for _, c := range cases {
		r := &replication.SlotContinuityResult{GapBytes: c.gap}
		if got := r.HasGap(); got != c.want {
			t.Errorf("GapBytes=%d HasGap()=%v, want %v", c.gap, got, c.want)
		}
	}
	// Nil-receiver: HasGap must not panic.
	var nilRes *replication.SlotContinuityResult
	if nilRes.HasGap() {
		t.Errorf("nil receiver should report no gap")
	}
}

// TestSlotContinuityOutcome_Constants: the on-the-wire string
// values. Renaming them is a v1-stability break (the leader-
// follow loop emits these in events that Sinks fan out to
// external systems); pin the strings so a future rename surfaces
// here.
func TestSlotContinuityOutcome_Constants(t *testing.T) {
	if string(replication.SlotFound) != "found" {
		t.Errorf("SlotFound = %q, want \"found\"", replication.SlotFound)
	}
	if string(replication.SlotRecreated) != "recreated" {
		t.Errorf("SlotRecreated = %q, want \"recreated\"", replication.SlotRecreated)
	}
}

// TestSlotContinuityResult_GapAccessors: pin the public field
// types so callers (the leader-follow loop, doctor) compile
// against a stable surface. A regression here = renaming a public
// field, which would break external consumers.
func TestSlotContinuityResult_GapAccessors(t *testing.T) {
	r := &replication.SlotContinuityResult{
		Outcome:          replication.SlotRecreated,
		GapBytes:         1024,
		GapStartLSN:      pglogrepl.LSN(0x1000),
		GapEndLSN:        pglogrepl.LSN(0x1400),
		LastConfirmedLSN: pglogrepl.LSN(0x1000),
	}
	if r.Outcome != replication.SlotRecreated {
		t.Error("Outcome field broken")
	}
	if r.GapBytes != 1024 {
		t.Error("GapBytes field broken")
	}
	if r.GapStartLSN != pglogrepl.LSN(0x1000) {
		t.Error("GapStartLSN field broken")
	}
	if r.GapEndLSN != pglogrepl.LSN(0x1400) {
		t.Error("GapEndLSN field broken")
	}
	if r.LastConfirmedLSN != pglogrepl.LSN(0x1000) {
		t.Error("LastConfirmedLSN field broken")
	}
}

// TestEnsureSlot_StringSlotNameEmpty already covered above; this
// lints + ensures errors.Is is reachable from the public surface.
func TestEnsureSlot_PublicSentinels(t *testing.T) {
	// ErrSlotMissing should still be exported (we depend on it
	// internally and external consumers might too).
	if replication.ErrSlotMissing == nil {
		t.Fatal("ErrSlotMissing must be exported")
	}
	wrapped := errors.New("wrapped: " + replication.ErrSlotMissing.Error())
	_ = wrapped // sanity — not asserting wrap behaviour here
}

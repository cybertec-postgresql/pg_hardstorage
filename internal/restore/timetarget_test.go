package restore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestResolveBackupForTime_PicksLatestAtOrBeforeTarget: with
// five backups at 14:00, 14:01, 14:02, 14:03, 14:04 (the
// commitN fixture's pattern), a target at 14:02:30 must pick
// the 14:02 backup — the latest whose StoppedAt is at or
// before target.
func TestResolveBackupForTime_PicksLatestAtOrBeforeTarget(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 5)
	// commitN starts at 2026-04-28T14:00:00Z and adds 1 min
	// per i. Manifest StoppedAt = StartedAt + 30s, so:
	//   ids[0] StoppedAt = 14:00:30
	//   ids[1] StoppedAt = 14:01:30
	//   ids[2] StoppedAt = 14:02:30
	//   ids[3] StoppedAt = 14:03:30
	//   ids[4] StoppedAt = 14:04:30
	// Target 14:03:00 → ids[2] (StoppedAt 14:02:30 < target).
	target := time.Date(2026, 4, 28, 14, 3, 0, 0, time.UTC)
	got, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", target, verifier)
	if err != nil {
		t.Fatalf("ResolveBackupForTime: %v", err)
	}
	if got != ids[2] {
		t.Errorf("got %q, want %q (latest at-or-before target)", got, ids[2])
	}
}

// TestResolveBackupForTime_ExactBoundary: target == backup's
// StoppedAt is INCLUSIVE — that backup wins.
func TestResolveBackupForTime_ExactBoundary(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 3)
	// ids[1] StoppedAt = 14:01:30
	target := time.Date(2026, 4, 28, 14, 1, 30, 0, time.UTC)
	got, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", target, verifier)
	if err != nil {
		t.Fatalf("ResolveBackupForTime: %v", err)
	}
	if got != ids[1] {
		t.Errorf("got %q, want %q (exact boundary should be inclusive)", got, ids[1])
	}
}

// TestResolveBackupForTime_TargetBeforeAllBackups: target is
// older than every manifest → ErrNoBackupBeforeTime with the
// LaterCount populated.
func TestResolveBackupForTime_TargetBeforeAllBackups(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	commitN(t, sp, signer, "db1", 5)
	target := time.Date(2026, 4, 28, 13, 0, 0, 0, time.UTC) // 1h before backups start
	_, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", target, verifier)
	if err == nil {
		t.Fatal("expected error for target before all backups")
	}
	if !errors.Is(err, restore.ErrNoBackupBeforeTime) {
		t.Errorf("expected ErrNoBackupBeforeTime; got %v", err)
	}
	var notTime *restore.NoBackupBeforeTimeError
	if !errors.As(err, &notTime) {
		t.Fatalf("expected *NoBackupBeforeTimeError; got %T", err)
	}
	if notTime.LaterCount != 5 {
		t.Errorf("LaterCount = %d, want 5", notTime.LaterCount)
	}
	if notTime.Deployment != "db1" {
		t.Errorf("Deployment = %q, want db1", notTime.Deployment)
	}
}

// TestResolveBackupForTime_NoBackupsAtAll: empty deployment →
// ErrNoBackupsFound (mirroring ResolveLatest).
func TestResolveBackupForTime_NoBackupsAtAll(t *testing.T) {
	sp, _, verifier := newRepoWithSigner(t)
	target := time.Now().UTC()
	_, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", target, verifier)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, restore.ErrNoBackupsFound) {
		t.Errorf("expected ErrNoBackupsFound; got %v", err)
	}
}

// TestResolveBackupForTime_TargetAfterAll: target is newer
// than every backup → picks the most-recent backup (no
// constraint violation).
func TestResolveBackupForTime_TargetAfterAll(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 5)
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // far future
	got, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", target, verifier)
	if err != nil {
		t.Fatalf("ResolveBackupForTime: %v", err)
	}
	if got != ids[4] {
		t.Errorf("got %q, want %q (most-recent when target after all)", got, ids[4])
	}
}

// TestResolveBackupForTime_RejectsZeroTarget: zero target is
// a programmer error and must surface immediately.
func TestResolveBackupForTime_RejectsZeroTarget(t *testing.T) {
	sp, _, verifier := newRepoWithSigner(t)
	_, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", time.Time{}, verifier)
	if err == nil {
		t.Fatal("expected error for zero target time")
	}
}

// TestResolveBackupForTime_ChainBetweenTwoTargets: two backups
// at 14:00 and 14:02; target 14:01 → 14:00; target 14:03 →
// 14:02. Confirms the resolver picks correctly across the
// boundary.
func TestResolveBackupForTime_ChainBetweenTwoTargets(t *testing.T) {
	sp, signer, verifier := newRepoWithSigner(t)
	ids := commitN(t, sp, signer, "db1", 2) // 14:00 + 14:01
	// commitN spacing is 1 minute; ids[0] StoppedAt 14:00:30,
	// ids[1] StoppedAt 14:01:30.
	cases := []struct {
		name   string
		target time.Time
		want   string
	}{
		{"between", time.Date(2026, 4, 28, 14, 1, 0, 0, time.UTC), ids[0]},
		{"after-second", time.Date(2026, 4, 28, 14, 2, 0, 0, time.UTC), ids[1]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := restore.ResolveBackupForTime(context.Background(), sp, "db1", c.target, verifier)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestStatus_LifecycleCounts_AllZeroAreOmitted: a deployment
// with only live backups and no holds shows no lifecycle keys
// in the JSON body — schema-additive omitempty preserves the
// 24-month JSON-compat commitment.
func TestStatus_LifecycleCounts_AllZeroAreOmitted(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	stdout, _, exit := runCLI(t, "status", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	for _, banned := range []string{
		`"tombstoned_count"`,
		`"active_holds"`,
		`"expired_holds"`,
	} {
		if strings.Contains(stdout, banned) {
			t.Errorf("clean deployment should NOT include %s in JSON:\n%s", banned, stdout)
		}
	}
}

// TestStatus_LifecycleCounts_TombstonedSurfaces: a tombstoned
// manifest counts toward TombstonedCount and is excluded from
// BackupCount. The latest-backup pointer continues to come
// from the live set.
func TestStatus_LifecycleCounts_TombstonedSurfaces(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	w.commitManifest(t, "db1", 1)
	// Find the live IDs and tombstone the older one.
	var ids []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.BackupID)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 live; got %d", len(ids))
	}
	if err := w.store.SoftDelete(context.Background(), "db1", ids[0], "manual", "test"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "status", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	var view struct {
		Deployments []struct {
			Deployment      string `json:"deployment"`
			BackupCount     int    `json:"backup_count"`
			TombstonedCount int    `json:"tombstoned_count"`
			LatestBackupID  string `json:"latest_backup_id"`
		} `json:"deployments"`
	}
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 1 {
		t.Fatalf("expected 1 deployment; got %d", len(view.Deployments))
	}
	d := view.Deployments[0]
	if d.BackupCount != 1 {
		t.Errorf("BackupCount = %d, want 1 (tombstoned excluded)", d.BackupCount)
	}
	if d.TombstonedCount != 1 {
		t.Errorf("TombstonedCount = %d, want 1", d.TombstonedCount)
	}
	if d.LatestBackupID != ids[1] {
		t.Errorf("LatestBackupID = %q, want %q (live backup)", d.LatestBackupID, ids[1])
	}
}

// TestStatus_LifecycleCounts_HoldsBucketed: an indefinite hold
// counts toward ActiveHolds; an expired bounded hold counts
// toward ExpiredHolds.
func TestStatus_LifecycleCounts_HoldsBucketed(t *testing.T) {
	w := newReadWorld(t)
	idActive := commitVerifiableBackup(t, w, "db1", 0, []byte("active-hold"))
	idExpired := commitVerifiableBackup(t, w, "db1", 1, []byte("expired-hold"))
	if err := w.store.PutHold(context.Background(), "db1", idActive, "ops", "indefinite"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", idExpired,
		"old-debug", "stale", past); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "status", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	var view struct {
		Deployments []struct {
			ActiveHolds  int `json:"active_holds"`
			ExpiredHolds int `json:"expired_holds"`
		} `json:"deployments"`
	}
	bodyOf(t, stdout, &view)
	d := view.Deployments[0]
	if d.ActiveHolds != 1 {
		t.Errorf("ActiveHolds = %d, want 1", d.ActiveHolds)
	}
	if d.ExpiredHolds != 1 {
		t.Errorf("ExpiredHolds = %d, want 1", d.ExpiredHolds)
	}
}

// TestStatus_LifecycleCounts_TextRendering: the text mode
// drops a continuation line under each deployment when any
// lifecycle count is non-zero. A clean deployment shows
// nothing extra.
func TestStatus_LifecycleCounts_TextRendering(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("text-mode"))
	if err := w.store.PutHold(context.Background(), "db1", id,
		"ops", "indefinite"); err != nil {
		t.Fatal(err)
	}
	// Plant a tombstoned + an expired hold for full coverage.
	w.commitManifest(t, "db1", 5)
	var ids []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.BackupID)
	}
	// Pick the one that's NOT the held backup.
	var toTomb string
	for _, x := range ids {
		if x != id {
			toTomb = x
			break
		}
	}
	if err := w.store.SoftDelete(context.Background(), "db1", toTomb, "manual", "test"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "status", "db1", "--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{
		"1 tombstoned",
		"1 active hold",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// TestStatus_LifecycleCounts_TextCleanDeploymentNoExtraLine:
// a deployment with no lifecycle activity must NOT get a
// continuation line — the existing one-row-per-deployment
// shape is preserved.
func TestStatus_LifecycleCounts_TextCleanDeploymentNoExtraLine(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	stdout, _, exit := runCLI(t, "status", "db1", "--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	if strings.Contains(stdout, "tombstoned") || strings.Contains(stdout, "expired hold") || strings.Contains(stdout, "active hold") {
		t.Errorf("clean deployment should not show lifecycle continuation:\n%s", stdout)
	}
	// The "└─" tree marker should not appear under a clean row.
	if strings.Contains(stdout, "└─") {
		t.Errorf("clean deployment should not show continuation marker:\n%s", stdout)
	}
}

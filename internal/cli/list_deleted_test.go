package cli_test

import (
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestList_IncludeDeleted_SurfacesTombstoned: with --include-deleted,
// soft-deleted manifests appear in the list paired with
// tombstoned=true. Without the flag, they're invisible (the
// existing default behaviour).
func TestList_IncludeDeleted_SurfacesTombstoned(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	w.commitManifest(t, "db1", 1)
	w.commitManifest(t, "db1", 2)

	// Find the IDs the way the helper builds them, then tombstone
	// the middle one.
	var liveIDs []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		liveIDs = append(liveIDs, m.BackupID)
	}
	if len(liveIDs) != 3 {
		t.Fatalf("expected 3 live manifests, got %d", len(liveIDs))
	}
	tombID := liveIDs[1]
	if err := w.store.SoftDelete(context.Background(), "db1", tombID, "manual", "test"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	// Default list: tombstoned hidden.
	stdout, _, exit := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("default list exit=%d", exit)
	}
	var defaultView struct {
		Count   int `json:"count"`
		Backups []struct {
			BackupID   string `json:"backup_id"`
			Tombstoned bool   `json:"tombstoned"`
		} `json:"backups"`
		IncludeDeleted bool `json:"include_deleted"`
	}
	bodyOf(t, stdout, &defaultView)
	if defaultView.Count != 2 {
		t.Errorf("default list count = %d, want 2 (tombstoned hidden)", defaultView.Count)
	}
	if defaultView.IncludeDeleted {
		t.Errorf("default list should not flag include_deleted")
	}
	for _, b := range defaultView.Backups {
		if b.BackupID == tombID {
			t.Errorf("default list should not surface tombstoned %s", tombID)
		}
		if b.Tombstoned {
			t.Errorf("default list yielded tombstoned=true for %s", b.BackupID)
		}
	}

	// --include-deleted: all 3 visible, the tombstoned one
	// flagged.
	stdout, _, exit = runCLI(t, "list", "db1",
		"--repo", w.repoURL, "--include-deleted", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("include-deleted list exit=%d", exit)
	}
	var includedView struct {
		Count   int `json:"count"`
		Backups []struct {
			BackupID   string `json:"backup_id"`
			Tombstoned bool   `json:"tombstoned"`
		} `json:"backups"`
		IncludeDeleted bool `json:"include_deleted"`
	}
	bodyOf(t, stdout, &includedView)
	if includedView.Count != 3 {
		t.Errorf("include-deleted list count = %d, want 3", includedView.Count)
	}
	if !includedView.IncludeDeleted {
		t.Errorf("include-deleted list should flag include_deleted=true")
	}
	var foundTomb bool
	for _, b := range includedView.Backups {
		if b.BackupID == tombID {
			foundTomb = true
			if !b.Tombstoned {
				t.Errorf("expected tombstoned=true on %s", tombID)
			}
		} else if b.Tombstoned {
			t.Errorf("non-tombstoned %s flagged tombstoned=true", b.BackupID)
		}
	}
	if !foundTomb {
		t.Errorf("include-deleted list missed the tombstoned %s", tombID)
	}
}

// TestList_OnlyDeleted_ShowsOnlyTombstoned: --only-deleted is the
// strict-filter variant — yields nothing but tombstoned manifests.
// Useful for `backup undelete` discoverability.
func TestList_OnlyDeleted_ShowsOnlyTombstoned(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	w.commitManifest(t, "db1", 1)

	var ids []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.BackupID)
	}
	tombID := ids[0]
	if err := w.store.SoftDelete(context.Background(), "db1", tombID, "manual", "test"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "list", "db1",
		"--repo", w.repoURL, "--only-deleted", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("only-deleted exit=%d", exit)
	}
	var view struct {
		Count   int `json:"count"`
		Backups []struct {
			BackupID   string `json:"backup_id"`
			Tombstoned bool   `json:"tombstoned"`
		} `json:"backups"`
		OnlyDeleted    bool `json:"only_deleted"`
		IncludeDeleted bool `json:"include_deleted"`
	}
	bodyOf(t, stdout, &view)
	if view.Count != 1 {
		t.Errorf("only-deleted count = %d, want 1", view.Count)
	}
	if !view.OnlyDeleted {
		t.Errorf("only_deleted should be true in body")
	}
	if !view.IncludeDeleted {
		t.Errorf("only_deleted implies include_deleted; both should be true")
	}
	if len(view.Backups) != 1 || view.Backups[0].BackupID != tombID || !view.Backups[0].Tombstoned {
		t.Errorf("only-deleted list surface mismatch: %+v", view.Backups)
	}
}

// TestList_OnlyDeleted_NoTombstones_FriendlyEmpty: when there's
// nothing tombstoned, --only-deleted prints a clear "nothing to
// undelete" message rather than the generic empty-list line.
func TestList_OnlyDeleted_NoTombstones_FriendlyEmpty(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)

	stdout, _, exit := runCLI(t, "list", "db1",
		"--repo", w.repoURL, "--only-deleted", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("only-deleted (no tombstones) exit=%d", exit)
	}
	if !strings.Contains(stdout, "No deleted backups") {
		t.Errorf("expected 'No deleted backups' in only-deleted empty:\n%s", stdout)
	}
}

// TestList_IncludeDeleted_TextHasUndeleteHint: the text renderer
// drops a one-liner pointing at `backup undelete` so an operator
// who runs `list --include-deleted` discovers the recovery path.
func TestList_IncludeDeleted_TextHasUndeleteHint(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	var ids []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.BackupID)
	}
	if err := w.store.SoftDelete(context.Background(), "db1", ids[0], "manual", "test"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "list", "db1",
		"--repo", w.repoURL, "--include-deleted", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout, "[DELETED]") {
		t.Errorf("text should tag tombstoned rows with [DELETED]:\n%s", stdout)
	}
	if !strings.Contains(stdout, "backup undelete") {
		t.Errorf("text should hint at `backup undelete`:\n%s", stdout)
	}
}

// TestList_DefaultBodyShape_Unchanged: regression — adding
// IncludeDeleted/OnlyDeleted to listBody is schema-additive. A
// default `list` call (no flags) must NOT include those keys
// in the JSON body, preserving the existing 24-month
// compatibility commitment for existing scripts.
func TestList_DefaultBodyShape_Unchanged(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)

	stdout, _, exit := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	// Top-level body shape: "include_deleted" and "only_deleted"
	// must be omitted (omitempty), not just false. Strings.Contains
	// catches them either way.
	if strings.Contains(stdout, `"include_deleted"`) {
		t.Errorf("default list body should not include include_deleted key:\n%s", stdout)
	}
	if strings.Contains(stdout, `"only_deleted"`) {
		t.Errorf("default list body should not include only_deleted key:\n%s", stdout)
	}
	if strings.Contains(stdout, `"tombstoned"`) {
		t.Errorf("default list rows should not include tombstoned key:\n%s", stdout)
	}

	// Sanity: it's still valid JSON we can parse with the old
	// schema.
	var legacy struct {
		Deployment string `json:"deployment"`
		Count      int    `json:"count"`
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unwrap Result: %v\n%s", err, stdout)
	}
	bb, _ := stdjson.Marshal(res.Result)
	if err := stdjson.Unmarshal(bb, &legacy); err != nil {
		t.Fatalf("decode legacy body: %v", err)
	}
	if legacy.Deployment != "db1" {
		t.Errorf("deployment = %q, want db1", legacy.Deployment)
	}
}

// TestList_IncludeDeleted_SurfacesTombstoneMetadata: with
// --include-deleted, the tombstoned rows carry deleted_at,
// delete_reason, delete_policy from the marker body. This is
// the operational signal — "I know that's deleted; when, why,
// by what policy?" — without chasing the audit log.
func TestList_IncludeDeleted_SurfacesTombstoneMetadata(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	var ids []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.BackupID)
	}
	if err := w.store.SoftDelete(context.Background(), "db1", ids[0], "manual", "operator-error"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "list", "db1",
		"--repo", w.repoURL, "--include-deleted", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	var view struct {
		Backups []struct {
			BackupID     string  `json:"backup_id"`
			Tombstoned   bool    `json:"tombstoned"`
			DeletedAt    *string `json:"deleted_at"`
			DeleteReason string  `json:"delete_reason"`
			DeletePolicy string  `json:"delete_policy"`
		} `json:"backups"`
	}
	bodyOf(t, stdout, &view)
	if len(view.Backups) != 1 {
		t.Fatalf("expected 1 backup; got %d", len(view.Backups))
	}
	r := view.Backups[0]
	if !r.Tombstoned {
		t.Errorf("tombstoned should be true")
	}
	if r.DeletedAt == nil || *r.DeletedAt == "" {
		t.Errorf("deleted_at should be populated; got %v", r.DeletedAt)
	}
	if r.DeleteReason != "operator-error" {
		t.Errorf("delete_reason = %q, want operator-error", r.DeleteReason)
	}
	if r.DeletePolicy != "manual" {
		t.Errorf("delete_policy = %q, want manual", r.DeletePolicy)
	}
}

// TestList_OnlyDeleted_TextHasMetadata: text mode for
// --only-deleted shows the deletion timestamp and reason on a
// continuation line so the operator sees the why at a glance.
func TestList_OnlyDeleted_TextHasMetadata(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	var ids []string
	for m, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.BackupID)
	}
	if err := w.store.SoftDelete(context.Background(), "db1", ids[0], "manual", "GDPR-art-17-request-1234"); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "list", "db1",
		"--repo", w.repoURL, "--only-deleted", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{
		"[DELETED]",
		"deleted ",                 // continuation line prefix
		"manual",                   // policy
		"GDPR-art-17-request-1234", // reason
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// TestList_FlagsDiscoverable: --include-deleted and --only-deleted
// show up in `list --help`.
func TestList_FlagsDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "list", "--help")
	for _, want := range []string{"--include-deleted", "--only-deleted", "tombstoned"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list --help missing %q:\n%s", want, stdout)
		}
	}
}

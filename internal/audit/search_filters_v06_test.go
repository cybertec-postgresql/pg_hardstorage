package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// seedRichEvents plants a fixed set of events with varied
// fields (deployments, backup IDs, action namespaces) for the
// new v0.6+ filter tests.
func seedRichEvents(t *testing.T, store *audit.Store) {
	t.Helper()
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	events := []struct {
		action, actor, deployment, backupID string
		t                                   time.Time
	}{
		{"backup.create", "alice@acme.com", "db1", "db1.full.A", t0},
		{"backup.create", "bob@acme.com", "db2", "db2.full.B", t0.Add(15 * time.Minute)},
		{"backup.delete", "alice@acme.com", "db1", "db1.full.A", t0.Add(30 * time.Minute)},
		{"backup.delete", "bob@acme.com", "db2", "db2.full.B", t0.Add(45 * time.Minute)},
		{"backup.undelete", "alice@acme.com", "db1", "db1.full.A", t0.Add(60 * time.Minute)},
		{"kms.rotate", "system@cron", "", "", t0.Add(75 * time.Minute)},
		{"hold.add", "compliance@acme.com", "db1", "db1.full.A", t0.Add(90 * time.Minute)},
	}
	for i, e := range events {
		ev := &audit.Event{
			Action:    e.action,
			Actor:     e.actor,
			Timestamp: e.t,
			Subject: audit.Subject{
				Deployment: e.deployment,
				BackupID:   e.backupID,
			},
		}
		if err := store.Append(context.Background(), ev); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// TestSearch_ActionPrefix: --action-prefix backup. captures
// every backup.* event but excludes kms.rotate and hold.add.
func TestSearch_ActionPrefix(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	events, err := store.Search(context.Background(), audit.ListFilters{
		ActionPrefix: "backup.",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 2 create + 2 delete + 1 undelete = 5
	if len(events) != 5 {
		t.Errorf("got %d, want 5", len(events))
	}
	for _, ev := range events {
		if !startsWith(ev.Action, "backup.") {
			t.Errorf("non-backup action leaked: %q", ev.Action)
		}
	}
}

// TestSearch_ActionAndPrefix_Combined: --action + --action-prefix
// AND-combine. Action=backup.delete with ActionPrefix=backup.
// matches just the deletes; ActionPrefix=hold. with
// Action=backup.delete matches nothing.
func TestSearch_ActionAndPrefix_Combined(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	events, err := store.Search(context.Background(), audit.ListFilters{
		Action:       "backup.delete",
		ActionPrefix: "backup.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("Action+matching-Prefix got %d, want 2", len(events))
	}
	// Mismatched prefix → empty
	events, err = store.Search(context.Background(), audit.ListFilters{
		Action:       "backup.delete",
		ActionPrefix: "hold.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("Action+non-matching-Prefix got %d, want 0", len(events))
	}
}

// TestSearch_Deployment: --deployment exact match against
// Subject.Deployment.
func TestSearch_Deployment(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	events, err := store.Search(context.Background(), audit.ListFilters{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// db1 events: create + delete + undelete + hold.add = 4
	if len(events) != 4 {
		t.Errorf("got %d, want 4 db1 events", len(events))
	}
	for _, ev := range events {
		if ev.Subject.Deployment != "db1" {
			t.Errorf("non-db1 leaked: %q", ev.Subject.Deployment)
		}
	}
}

// TestSearch_BackupID: --backup-id narrows to one backup's
// lifecycle events across multiple actions.
func TestSearch_BackupID(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	events, err := store.Search(context.Background(), audit.ListFilters{
		BackupID: "db1.full.A",
	})
	if err != nil {
		t.Fatal(err)
	}
	// db1.full.A events: create + delete + undelete + hold.add = 4
	if len(events) != 4 {
		t.Errorf("got %d, want 4", len(events))
	}
}

// TestSearch_ActorContains: substring match against actor.
// "@acme.com" captures every acme principal but not the cron
// system actor.
func TestSearch_ActorContains(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	events, err := store.Search(context.Background(), audit.ListFilters{
		ActorContains: "@acme.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 6 acme events (2 create + 2 delete + 1 undelete + 1 hold.add) — kms.rotate is system@cron
	if len(events) != 6 {
		t.Errorf("got %d, want 6", len(events))
	}
}

// TestSearch_Reverse: --reverse flips commit order — newest
// first. Combined with --limit, returns the N most-recent
// events.
func TestSearch_Reverse(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	// Forward: oldest first.
	forward, err := store.Search(context.Background(), audit.ListFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(forward) != 7 {
		t.Fatalf("expected 7 events; got %d", len(forward))
	}
	// Reverse with limit 3 — newest 3.
	reverse, err := store.Search(context.Background(), audit.ListFilters{
		Reverse: true,
		Limit:   3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reverse) != 3 {
		t.Fatalf("expected 3 events; got %d", len(reverse))
	}
	// reverse[0] should be the newest forward event.
	if reverse[0].ID != forward[len(forward)-1].ID {
		t.Errorf("reverse[0]=%s, want %s (last forward)", reverse[0].ID, forward[len(forward)-1].ID)
	}
}

// TestSummaryByAction_GroupsCorrectly: SummaryByAction groups
// events by action name. Filters apply BEFORE grouping.
func TestSummaryByAction_GroupsCorrectly(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	counts, total, err := store.SummaryByAction(context.Background(), audit.ListFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 7 {
		t.Errorf("total = %d, want 7", total)
	}
	want := map[string]int{
		"backup.create":   2,
		"backup.delete":   2,
		"backup.undelete": 1,
		"kms.rotate":      1,
		"hold.add":        1,
	}
	for action, n := range want {
		if counts[action] != n {
			t.Errorf("%s = %d, want %d", action, counts[action], n)
		}
	}
}

// TestSummaryByAction_RespectsFilters: filters narrow the
// rollup. --deployment db1 → 4 events bucketed.
func TestSummaryByAction_RespectsFilters(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	counts, total, err := store.SummaryByAction(context.Background(), audit.ListFilters{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 (db1 events)", total)
	}
	// db1 has: 1 create, 1 delete, 1 undelete, 1 hold.add
	for _, action := range []string{"backup.create", "backup.delete", "backup.undelete", "hold.add"} {
		if counts[action] != 1 {
			t.Errorf("%s = %d, want 1", action, counts[action])
		}
	}
	if _, present := counts["kms.rotate"]; present {
		t.Errorf("kms.rotate should not be in db1-scoped summary")
	}
}

// TestSummaryByAction_IgnoresLimitAndReverse: SummaryByAction
// counts ALL matches — Limit and Reverse on the input filter
// must not narrow the rollup (otherwise "summary" would give
// the wrong answer).
func TestSummaryByAction_IgnoresLimitAndReverse(t *testing.T) {
	store, _ := newAuditStore(t)
	seedRichEvents(t, store)
	counts, total, err := store.SummaryByAction(context.Background(), audit.ListFilters{
		Limit:   2, // would cap Search() at 2; summary must override
		Reverse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 7 {
		t.Errorf("Limit/Reverse should not narrow summary; total=%d, want 7", total)
	}
	if counts["backup.create"] != 2 {
		t.Errorf("backup.create count must reflect ALL matches; got %d, want 2", counts["backup.create"])
	}
}

// startsWith is a small helper to keep the test self-contained.
func startsWith(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	return s[:len(prefix)] == prefix
}

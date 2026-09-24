package audit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
)

// TestAppend_FillsMissingActor pins H18: kms shred, backup delete, repo
// gc/wipe/set-mode append events with no Actor, and insider detection
// skips actor-less events -- so the destructive ops it exists to watch
// were invisible to it. Append now attributes every event: the
// documented $PG_HARDSTORAGE_ACTOR override, else user@host. The fill
// happens before hashing, so the chain still verifies.
func TestAppend_FillsMissingActor(t *testing.T) {
	ctx := context.Background()

	t.Run("env override", func(t *testing.T) {
		t.Setenv(audit.EnvActor, "svc-backup")
		store, _ := newAuditStore(t)
		ev := &audit.Event{Action: "repo.wipe"}
		if err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if ev.Actor != "svc-backup" {
			t.Errorf("Actor = %q, want svc-backup", ev.Actor)
		}
	})

	t.Run("os identity", func(t *testing.T) {
		t.Setenv(audit.EnvActor, "")
		t.Setenv("USER", "alice")
		store, _ := newAuditStore(t)
		if err := store.Append(ctx, &audit.Event{Action: "kms.shred"}); err != nil {
			t.Fatal(err)
		}
		evs, err := store.Search(ctx, audit.ListFilters{})
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 1 || !strings.HasPrefix(evs[0].Actor, "alice") {
			t.Fatalf("stored events = %+v, want one with actor alice@<host>", evs)
		}
		if _, err := store.VerifyChain(ctx); err != nil {
			t.Errorf("chain with a filled actor must verify: %v", err)
		}
	})

	t.Run("explicit actor kept", func(t *testing.T) {
		t.Setenv(audit.EnvActor, "svc-backup")
		store, _ := newAuditStore(t)
		ev := &audit.Event{Action: "backup.delete", Actor: "bob"}
		if err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if ev.Actor != "bob" {
			t.Errorf("Actor = %q, want the caller's bob", ev.Actor)
		}
	})
}

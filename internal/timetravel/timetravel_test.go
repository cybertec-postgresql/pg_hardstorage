package timetravel_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/timetravel"
)

func TestManager_NewIsEmpty(t *testing.T) {
	m := timetravel.NewManager(filepath.Join(t.TempDir(), "tt.json"), "/usr/bin/pg_hardstorage")
	out, err := m.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("fresh state should be empty; got %+v", out)
	}
}

func TestManager_DestroyMissing(t *testing.T) {
	m := timetravel.NewManager(filepath.Join(t.TempDir(), "tt.json"), "/usr/bin/pg_hardstorage")
	err := m.Destroy(context.Background(), "ghost", timetravel.DestroyOptions{})
	if !errors.Is(err, timetravel.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestSession_IsExpired(t *testing.T) {
	now := time.Now()
	active := timetravel.Session{ExpiresAt: now.Add(time.Hour)}
	if active.IsExpired() {
		t.Error("session expiring in 1h should not be expired")
	}
	expired := timetravel.Session{ExpiresAt: now.Add(-time.Hour)}
	if !expired.IsExpired() {
		t.Error("session expired 1h ago should be expired")
	}
	noExpiry := timetravel.Session{}
	if noExpiry.IsExpired() {
		t.Error("session with zero ExpiresAt should not be expired")
	}
}

func TestCleanup_NoSessions(t *testing.T) {
	m := timetravel.NewManager(filepath.Join(t.TempDir(), "tt.json"), "/usr/bin/pg_hardstorage")
	res, err := m.Cleanup(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reaped) != 0 {
		t.Errorf("empty cleanup should reap nothing; got %v", res.Reaped)
	}
	if res.RemainingActive != 0 {
		t.Errorf("RemainingActive = %d", res.RemainingActive)
	}
}

//go:build integration

package server

import (
	"context"
	"testing"
	"time"

	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestPGBackend_SweepTimestampsIgnoreSessionTimeZone pins that the
// abandoned-job sweep stamps completed_at / updated_at with the real
// instant regardless of the session TimeZone. `now() AT TIME ZONE 'UTC'`
// yields a zone-less UTC wall clock that PostgreSQL re-interprets in the
// session zone when assigned to a timestamptz column, so on a server
// whose TimeZone is not UTC the stamps were off by the zone offset
// (+14h here) -- and updated_at is the liveness field the sweep keys on.
func TestPGBackend_SweepTimestampsIgnoreSessionTimeZone(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := pg.DSN + "&timezone=Pacific/Kiritimati"
	b, err := OpenPGBackend(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var tz string
	if err := b.Pool().QueryRow(ctx, `SHOW TimeZone`).Scan(&tz); err != nil || tz != "Pacific/Kiritimati" {
		t.Fatalf("fixture: session TimeZone = %q (%v), want Pacific/Kiritimati", tz, err)
	}
	j, err := b.Enqueue(ctx, EnqueueOptions{Kind: JobBackup, Deployment: "db1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(ctx, ClaimOptions{AgentID: "a", Deployments: []string{"db1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Pool().Exec(ctx, `UPDATE phs.jobs SET updated_at = now() - interval '2 hours' WHERE id = $1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := b.SweepAbandoned(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("SweepAbandoned = %d, %v; want 1", n, err)
	}
	got, err := b.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, ts := range map[string]time.Time{"completed_at": derefTime(got.CompletedAt), "updated_at": got.UpdatedAt} {
		if d := time.Since(ts); d > time.Minute || d < -time.Minute {
			t.Errorf("%s = %s, %s from now; want the sweep instant", name, ts, d)
		}
	}
}

func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

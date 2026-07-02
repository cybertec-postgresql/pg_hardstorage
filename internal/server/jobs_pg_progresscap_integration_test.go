//go:build integration

package server_test

import (
	"context"
	"testing"
	"time"

	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestPGBackend_AppendProgress_Caps is the regression for bug #23:
// PGBackend.AppendProgress had no cap (unlike MemoryBackend's
// maxProgressEvents=1000), so the jsonb progress array grew unbounded
// and every append rewrote the whole thing. After the fix, appending
// far more than the bound must leave the stored array capped at the
// same 1000-event bound MemoryBackend uses, keeping the most recent
// events.
func TestPGBackend_AppendProgress_Caps(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	b, err := server.OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatalf("OpenPGBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if _, err := b.Pool().Exec(ctx, `TRUNCATE phs.jobs`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	j, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := b.Claim(ctx, server.ClaimOptions{AgentID: "a1", Deployments: []string{"db1"}}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The cap mirrors MemoryBackend.maxProgressEvents (1000). Push well
	// past it.
	const bound = 1000
	const total = bound + 250
	for i := 0; i < total; i++ {
		if err := b.AppendProgress(ctx, j.ID, server.ProgressEvent{
			At: time.Now().UTC(),
			Op: "tick",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := b.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Progress) != bound {
		t.Fatalf("stored progress = %d events, want capped at %d", len(got.Progress), bound)
	}
}

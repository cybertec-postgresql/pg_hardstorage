//go:build integration

package server_test

// jobs_pg_progressdropped_integration_test.go — the durable backend
// trimmed progress silently.
//
// Job.ProgressDropped exists for one reason, stated in its own doc:
// progress is bounded, and "ProgressDropped records how many were shed"
// so "the truncation isn't silent" (memory-leak audit #3).
// MemoryBackend maintains it.
//
// PGBackend got the BOUND from that audit — bug #23 added the same
// 1000-event cap — but not its companion counter. There was no
// progress_dropped column, nothing incremented it, and scanJob left it
// zero. So on the backend that actually runs for months, a long job's
// progress was trimmed with no record at all, and an operator reading
// it saw the most recent events with nothing to say earlier ones had
// existed. For a backup or restore that array is the record of what the
// agent did.
//
// Same shape as PruneTerminal in this file's sibling: a bound added to
// the in-memory backend, and the durable one inheriting the bound
// without the honesty that came with it.

import (
	"context"
	"testing"
	"time"

	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

func TestPGBackend_AppendProgress_RecordsWhatItDropped(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
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

	const bound = 1000
	const over = 50
	for i := 0; i < bound+over; i++ {
		if err := b.AppendProgress(ctx, j.ID, server.ProgressEvent{
			At: time.Now().UTC(), Op: "tick",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := b.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Progress) != bound {
		t.Fatalf("progress = %d events, want capped at %d", len(got.Progress), bound)
	}
	if got.ProgressDropped != over {
		t.Fatalf("ProgressDropped = %d, want %d.\n\n"+
			"%d events were discarded to hold the cap and the record says none were. "+
			"MemoryBackend counts them precisely so the truncation is not silent; the "+
			"durable backend inherited the bound without the counter, so an operator "+
			"reading a long job's progress cannot tell that earlier events existed.",
			got.ProgressDropped, over, over)
	}
}

// Under the cap nothing is dropped, so the counter must stay zero —
// otherwise every job reports phantom loss and the field is noise.
func TestPGBackend_AppendProgress_UnderCapDropsNothing(t *testing.T) {
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
	for i := 0; i < 25; i++ {
		if err := b.AppendProgress(ctx, j.ID, server.ProgressEvent{
			At: time.Now().UTC(), Op: "tick",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := b.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProgressDropped != 0 {
		t.Errorf("ProgressDropped = %d with only 25 events appended; a counter that fires "+
			"below the cap makes every job look lossy", got.ProgressDropped)
	}
	if len(got.Progress) != 25 {
		t.Errorf("progress = %d events, want 25", len(got.Progress))
	}
}

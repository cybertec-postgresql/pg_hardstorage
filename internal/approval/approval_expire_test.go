package approval_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/approval"
)

// TestSweepExpired_RecordsAndIsIdempotent pins the approval-expiry gap
// fix: SweepExpired stamps ExpiredAt on a request past its TTL (so the
// caller can emit an audit event), leaves the derived status untouched,
// and is idempotent — a second sweep records nothing new. --dry-run
// previews without stamping.
func TestSweepExpired_RecordsAndIsIdempotent(t *testing.T) {
	store, _ := newApprovalStore(t)
	_, pubA := genKey(t)

	req, err := store.Create(context.Background(), approval.CreateOptions{
		Op:           "backup.delete",
		Threshold:    1,
		ApproverKeys: [][]byte{pubA},
		TTL:          20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)

	// Dry-run returns the expired request but must not stamp it.
	preview, err := store.SweepExpired(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || preview[0].ID != req.ID {
		t.Fatalf("dry-run preview = %+v, want the expired request", preview)
	}
	if got, _ := store.Get(context.Background(), req.ID); got.ExpiredAt != nil {
		t.Errorf("dry-run must not stamp ExpiredAt; got %v", got.ExpiredAt)
	}

	// Real sweep records expiry and returns the request.
	recorded, err := store.SweepExpired(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].ID != req.ID {
		t.Fatalf("sweep recorded = %+v, want the expired request", recorded)
	}
	got, err := store.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiredAt == nil {
		t.Error("ExpiredAt not stamped after sweep")
	}
	// Status stays derived (expired) — the stamp doesn't change the verdict.
	if st, _ := store.StatusOf(context.Background(), req.ID); st != approval.StatusExpired {
		t.Errorf("status = %q, want expired", st)
	}

	// Idempotent: a second sweep records nothing new.
	again, err := store.SweepExpired(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second sweep recorded %d, want 0 (idempotent)", len(again))
	}
}

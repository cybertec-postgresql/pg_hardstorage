package compliance_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/compliance"
)

// TestGenerate_ApprovalRequest_CountsRequestsCreated pins the approval
// section to the REAL emitter action. The CLI writes `approval.request`
// (approval.go), but the report consumer used to match `approval.create`
// — which is never emitted — so RequestsCreated was always 0. That breaks
// assessApprovals: a destructive op with real approval requests could be
// scored as "executed with zero approval requests" (CC8.1 / AC-3 fail).
func TestGenerate_ApprovalRequest_CountsRequestsCreated(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "approval.request", "db1", now.Add(-1*time.Hour), nil)
	w.appendAudit(t, "approval.approve", "db1", now.Add(-30*time.Minute), nil)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Approvals.RequestsCreated != 1 {
		t.Errorf("RequestsCreated = %d, want 1 (approval.request must be counted)",
			rep.Approvals.RequestsCreated)
	}
	if rep.Approvals.ByStatus["pending"] != 1 || rep.Approvals.ByStatus["approved"] != 1 {
		t.Errorf("ByStatus = %v, want pending=1 approved=1", rep.Approvals.ByStatus)
	}
}

// TestGenerate_HoldPurgeExpired_CountsExpired pins the hold section to the
// REAL emitter action `hold.purge_expired` (hold.go). The consumer used to
// match the fictional `hold.expire` / `hold.purge`, so HoldsExpired was
// always 0 in production.
func TestGenerate_HoldPurgeExpired_CountsExpired(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "hold.add", "db1", now.Add(-1*time.Hour), nil)
	w.appendAudit(t, "hold.purge_expired", "db1", now.Add(-15*time.Minute), nil)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Holds.HoldsExpired != 1 {
		t.Errorf("HoldsExpired = %d, want 1 (hold.purge_expired must be counted)",
			rep.Holds.HoldsExpired)
	}
}

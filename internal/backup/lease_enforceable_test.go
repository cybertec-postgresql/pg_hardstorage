package backup

// lease_enforceable_test.go — the lease must refuse a backend that
// cannot enforce it.
//
// Every guarantee the lease makes is Put(IfNotExists). The acquire is
// one, and the claim that makes breaking a lapsed lease exclusive is
// another. A backend that only EMULATES conditional put — stat, then
// write — turns both into a check followed by an unrelated action, so
// two runners can pass the check together and both proceed.
//
// The lease would still be written, and would still look correct in the
// repo and to `repo gc`. That is what makes it worth refusing: an
// operator who believes they have exclusion they do not have never
// looks again, and finds out when two backups of one deployment hit
// the same primary.
//
// sftp is the real case. It advertises ConditionalPut only when the
// server offers hardlink@openssh.com; without it the plugin documents
// a fallback whose race is fine for content-addressed chunks and NOT
// fine for a lock.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// noCondPutSP is a backend that cannot create atomically. Put ignores
// IfNotExists entirely — the worst honest emulation, and what
// stat-then-write degenerates to when two callers interleave.
type noCondPutSP struct {
	storage.StoragePlugin
}

func (s *noCondPutSP) Name() string { return "fake-no-condput" }

func (s *noCondPutSP) Capabilities() storage.Capabilities {
	c := s.StoragePlugin.Capabilities()
	c.ConditionalPut = false
	return c
}

// TestLease_RefusesBackendWithoutConditionalPut is the guard.
func TestLease_RefusesBackendWithoutConditionalPut(t *testing.T) {
	sp := &noCondPutSP{StoragePlugin: newLeaseSP(t)}

	_, err := AcquireBackupLease(context.Background(), sp, "db1", LeaseOptions{
		Owner: "A", TTL: time.Minute,
	})
	if err == nil {
		t.Fatal("acquired a lease on a backend that cannot enforce it — the lease excludes " +
			"nothing, but reads as a guarantee everywhere it is inspected")
	}
	if !errors.Is(err, ErrLeaseNotEnforceable) {
		t.Errorf("err = %v, want ErrLeaseNotEnforceable", err)
	}
	// The message has to be actionable: an operator seeing this needs
	// to know which backend, and what to do about it.
	msg := err.Error()
	for _, want := range []string{"fake-no-condput", "db1", "hardlink@openssh.com"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q; got: %v", want, err)
		}
	}
}

// TestLease_AllowUnenforceableOptsIn pins the escape hatch. Without it
// the guard would strand operators whose SFTP server lacks the
// extension but who know only one runner exists.
func TestLease_AllowUnenforceableOptsIn(t *testing.T) {
	sp := &noCondPutSP{StoragePlugin: newLeaseSP(t)}

	l, err := AcquireBackupLease(context.Background(), sp, "db1", LeaseOptions{
		Owner: "A", TTL: time.Minute, AllowUnenforceable: true,
	})
	if err != nil {
		t.Fatalf("AllowUnenforceable did not opt in: %v", err)
	}
	if l == nil {
		t.Fatal("nil lease with nil error")
	}
}

// TestLease_EnforceableBackendUnaffected is the control: the guard must
// not disturb a backend that can enforce the lease. Without this, a
// guard that refused everything would satisfy the test above.
func TestLease_EnforceableBackendUnaffected(t *testing.T) {
	sp := newLeaseSP(t) // fs: ConditionalPut = true
	if !sp.Capabilities().ConditionalPut {
		t.Fatal("the fs fixture no longer advertises ConditionalPut; this control proves nothing")
	}
	if _, err := AcquireBackupLease(context.Background(), sp, "db1", LeaseOptions{
		Owner: "A", TTL: time.Minute,
	}); err != nil {
		t.Fatalf("refused an enforceable backend: %v", err)
	}
}

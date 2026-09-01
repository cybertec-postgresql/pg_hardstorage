package jit_test

// `jit list` resolved each token's revocation state with the error
// dropped:
//
//	revoked, _ := s.IsRevoked(ctx, id)
//
// so a failed lookup defaulted to "not revoked" and computeEffectiveStatus
// then reported the token as ACTIVE. Worse, the filter runs on
// EffectiveStatus, so `jit list --status active` would list a token
// whose revocation could not be confirmed alongside genuinely live ones.
//
// That is the surface an operator uses to confirm a break-glass
// revocation took effect during an incident, and it failed in the
// permissive direction. Enforcement was never affected — VerifyAt
// propagates the same error and refuses the token — which is exactly
// why the reporting gap could sit here unnoticed.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/jit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// revokeProbeFailsSP fails the revocation probe while leaving token
// reads intact — the shape of a partial backend outage. IsRevoked
// Stats "jit/<id>.json.revoked"; token bodies are "jit/<id>.json", so
// intercepting the .revoked suffix isolates exactly the one lookup.
type revokeProbeFailsSP struct {
	storage.StoragePlugin
}

func (s *revokeProbeFailsSP) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if strings.HasSuffix(key, ".revoked") {
		return storage.ObjectInfo{}, errors.New("backend unavailable")
	}
	return s.StoragePlugin.Stat(ctx, key)
}

func TestList_UndeterminedRevocationIsNotReportedActive(t *testing.T) {
	w := setupJITWorld(t)
	ctx := context.Background()

	tok, err := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "alice", Scope: []string{"restore"},
		Duration: time.Hour, Reason: "break-glass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Put(ctx, tok); err != nil {
		t.Fatal(err)
	}

	// Same repo, but the revocation probe now fails.
	broken := jit.NewStore(&revokeProbeFailsSP{StoragePlugin: w.sp})
	entries, err := broken.List(ctx, jit.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *jit.ListEntry
	for _, e := range entries {
		if e.Token.ID == tok.ID {
			found = e
		}
	}
	if found == nil {
		t.Fatal("the token was dropped from the listing entirely")
	}
	if !found.RevocationUnknown {
		t.Error("the revocation lookup failed but the entry does not say so")
	}
	if found.EffectiveStatus == jit.StatusActive {
		t.Error("a token whose revocation state could not be determined was reported ACTIVE — " +
			"an operator confirming that a break-glass revocation took effect is told the " +
			"token is still live, or worse, told nothing is wrong when it is")
	}

	// And it must not be swept up by an active-only filter.
	active, err := broken.List(ctx, jit.ListFilter{Status: jit.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range active {
		if e.Token.ID == tok.ID {
			t.Error("`--status active` returned a token whose revocation state is unknown")
		}
	}
}

// The healthy path must be unchanged, or the flag turns every listing
// into a warning.
func TestList_HealthyRevocationStateIsReportedNormally(t *testing.T) {
	w := setupJITWorld(t)
	ctx := context.Background()

	live, err := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "alice", Scope: []string{"restore"}, Duration: time.Hour, Reason: "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Put(ctx, live); err != nil {
		t.Fatal(err)
	}
	dead, err := jit.Issue(w.signer, jit.IssueOptions{
		Principal: "bob", Scope: []string{"restore"}, Duration: time.Hour, Reason: "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Put(ctx, dead); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Revoke(ctx, dead.ID, "admin", "no longer needed",
		time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	entries, err := w.store.List(ctx, jit.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.RevocationUnknown {
			t.Errorf("token %s flagged unknown on a healthy backend", e.Token.ID)
		}
		switch e.Token.ID {
		case live.ID:
			if e.EffectiveStatus != jit.StatusActive {
				t.Errorf("live token = %q, want active", e.EffectiveStatus)
			}
		case dead.ID:
			if e.EffectiveStatus != jit.StatusRevoked {
				t.Errorf("revoked token = %q, want revoked", e.EffectiveStatus)
			}
		}
	}
}

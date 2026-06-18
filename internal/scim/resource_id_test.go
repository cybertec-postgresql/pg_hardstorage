package scim

import (
	"context"
	"errors"
	"testing"
)

func TestValidateResourceID(t *testing.T) {
	bad := []string{
		"",
		"../groups/x",  // cross-namespace via traversal
		"a/b",          // path separator
		`a\b`,          // backslash
		"a\x00b",       // NUL
		"a\nb",         // control char
		"users/../etc", // separators
	}
	for _, id := range bad {
		if err := validateResourceID(id); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
	// Server-minted ids and ordinary SCIM ids must pass — including a
	// bare ".." substring (harmless without a separator).
	good := []string{
		"5f3c2a1b-9d8e-4c7a-bb12-0011223344ff",
		"alice",
		"weird..name",
		"u_42",
	}
	for _, id := range good {
		if err := validateResourceID(id); err != nil {
			t.Errorf("id %q should be accepted: %v", id, err)
		}
	}
}

// TestStore_RejectsCrossNamespaceID confirms the gate fires through the
// public CRUD surface: a Get/Delete with a traversal id is refused
// (ErrInvalidPayload) rather than resolving into another namespace.
func TestStore_RejectsCrossNamespaceID(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"../groups/admins", "a/b", "x\x00y"} {
		if _, err := st.GetUser(ctx, id); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("GetUser(%q) err = %v; want ErrInvalidPayload", id, err)
		}
		if err := st.DeleteUser(ctx, id); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("DeleteUser(%q) err = %v; want ErrInvalidPayload", id, err)
		}
		if _, err := st.GetGroup(ctx, id); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("GetGroup(%q) err = %v; want ErrInvalidPayload", id, err)
		}
	}
}

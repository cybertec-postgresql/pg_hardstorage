package invariant

import (
	"strings"
	"testing"
)

func TestAssert(t *testing.T) {
	// Holding invariant: no panic.
	Assert(true, "never shown")

	// Violated invariant: panic with the greppable prefix and the
	// formatted detail.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("violated invariant did not panic — the fail-closed contract is broken")
		}
		msg, ok := r.(string)
		if !ok || !strings.HasPrefix(msg, "invariant violation: ") {
			t.Fatalf("panic value = %v, want string with 'invariant violation: ' prefix", r)
		}
		if !strings.Contains(msg, "boundary 0 outside (0, 262144]") {
			t.Fatalf("formatted detail missing: %q", msg)
		}
	}()
	Assert(false, "boundary %d outside (0, %d]", 0, 262144)
}

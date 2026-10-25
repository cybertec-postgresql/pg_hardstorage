package output

import (
	"context"
	"testing"
)

type ctxKey struct{}

func TestDetachedContext_ValuesFromCallerCancellationFromStop(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "trace-1"))
	stop, cancelStop := context.WithCancel(context.Background())
	defer cancelStop()
	dc := detachedContext{Context: stop, values: caller}

	if got := dc.Value(ctxKey{}); got != "trace-1" {
		t.Fatalf("caller value lost: %v", got)
	}
	cancelCaller() // the command finished; queued events must still be delivered
	if dc.Err() != nil {
		t.Fatal("caller cancellation leaked into the sink context")
	}
	cancelStop()
	if dc.Err() == nil {
		t.Fatal("dispatcher stop did not cancel the sink context")
	}
}

func TestDetachedContext_NilValuesContext(t *testing.T) {
	dc := detachedContext{Context: context.Background()}
	if dc.Value(ctxKey{}) != nil {
		t.Fatal("a nil values context must yield nil")
	}
}

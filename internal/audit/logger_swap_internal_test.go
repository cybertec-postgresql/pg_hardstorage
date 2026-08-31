package audit

// The package-level error loggers are the ONLY thing that makes two
// silent-by-design failures audible: a head-pointer write that fails
// after the event committed, and AppendOrLog's swallowed append error.
// Both leave the chain usable, so nothing else surfaces them — if the
// swap mechanism itself is broken, an operator loses the one signal
// that says the tamper-evident log had trouble. These functions had no
// test executing them at all (coverage ratchet).

import (
	"errors"
	"sync"
	"testing"
)

func TestSetDefaultAppendErrorLogger_RoundTripAndDisable(t *testing.T) {
	// Restore whatever the process had, so this test can't leak state
	// into others that assert on logger behaviour.
	orig := SetDefaultAppendErrorLogger(nil)
	t.Cleanup(func() { SetDefaultAppendErrorLogger(orig) })

	var got error
	prior := SetDefaultAppendErrorLogger(func(err error) { got = err })
	if prior != nil {
		t.Errorf("prior logger should be the nil we just installed, got non-nil")
	}
	if fn := currentAppendErrorLogger(); fn == nil {
		t.Fatal("installed logger not visible to currentAppendErrorLogger")
	} else {
		fn(errors.New("boom"))
	}
	if got == nil || got.Error() != "boom" {
		t.Errorf("installed logger did not receive the error, got %v", got)
	}

	// Setting nil disables: the call site guards on nil, so a disabled
	// logger must read back as nil rather than a no-op wrapper.
	returned := SetDefaultAppendErrorLogger(nil)
	if returned == nil {
		t.Error("Set should return the PRIOR logger, not the new one")
	}
	if currentAppendErrorLogger() != nil {
		t.Error("nil did not disable the logger")
	}
}

func TestSetDefaultHeadPointerErrorLogger_RoundTripAndDisable(t *testing.T) {
	orig := SetDefaultHeadPointerErrorLogger(nil)
	t.Cleanup(func() { SetDefaultHeadPointerErrorLogger(orig) })

	var got error
	if prior := SetDefaultHeadPointerErrorLogger(func(err error) { got = err }); prior != nil {
		t.Error("prior should be the nil just installed")
	}
	fn := currentHeadPointerErrorLogger()
	if fn == nil {
		t.Fatal("installed logger not visible")
	}
	fn(errors.New("pointer-boom"))
	if got == nil || got.Error() != "pointer-boom" {
		t.Errorf("logger did not receive the error, got %v", got)
	}
	if SetDefaultHeadPointerErrorLogger(nil) == nil {
		t.Error("Set should return the prior logger")
	}
	if currentHeadPointerErrorLogger() != nil {
		t.Error("nil did not disable")
	}
}

// The setters and readers are mutex-guarded because Append runs
// concurrently with whatever an operator's init does. Exercise both
// under -race so a future refactor that drops the lock is caught here
// rather than as a rare production data race.
func TestLoggerSwap_ConcurrentSetAndReadIsRaceFree(t *testing.T) {
	origA := SetDefaultAppendErrorLogger(nil)
	origH := SetDefaultHeadPointerErrorLogger(nil)
	t.Cleanup(func() {
		SetDefaultAppendErrorLogger(origA)
		SetDefaultHeadPointerErrorLogger(origH)
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				SetDefaultAppendErrorLogger(func(error) {})
				SetDefaultHeadPointerErrorLogger(func(error) {})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if fn := currentAppendErrorLogger(); fn != nil {
					fn(errors.New("x"))
				}
				if fn := currentHeadPointerErrorLogger(); fn != nil {
					fn(errors.New("y"))
				}
			}
		}()
	}
	wg.Wait()
}

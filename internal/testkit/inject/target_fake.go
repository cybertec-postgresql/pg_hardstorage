// target_fake.go — FakeTarget: test-only Target that records every call for assertions.
package inject

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// FakeTarget is a test-only Target that records every call.
// Tests assert against the recorded operations to prove the
// fault primitive issued the expected commands without
// touching any container runtime.
type FakeTarget struct {
	NameStr string
	RoleStr string

	// ExecResponses can be set to control return values.
	// Lookup is keyed on the joined argv ("dd if=/dev/zero ...").
	// Anything not in the map returns ("", nil).
	ExecResponses map[string][]byte

	// SignalErr is returned from every Signal call when set.
	SignalErr error

	// StartErr is returned from every Start call when set.
	StartErr error

	// MemoryLimitErr is returned from every SetMemoryLimit
	// call when set — lets tests exercise the error branch.
	MemoryLimitErr error

	// Files is a fake filesystem CopyOut reads / CopyIn writes.
	Files map[string][]byte

	mu         sync.Mutex
	execCmds   [][]string
	signals    []int
	startCalls int
	written    []string
	memLimits  []int64
}

// StartCalls returns the number of Start invocations the
// fault driver dispatched.  Tests assert on this to confirm
// signalFault.Apply paired Signal with the expected Start.
func (f *FakeTarget) StartCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls
}

// Name implements Target.
func (f *FakeTarget) Name() string { return f.NameStr }

// Role implements Target.
func (f *FakeTarget) Role() string { return f.RoleStr }

// Exec records the call and returns the configured response.
func (f *FakeTarget) Exec(_ context.Context, argv ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCmds = append(f.execCmds, append([]string{}, argv...))
	if f.ExecResponses != nil {
		key := joinArgv(argv)
		if r, ok := f.ExecResponses[key]; ok {
			return r, nil
		}
	}
	return nil, nil
}

// Signal records the signal and returns SignalErr.
func (f *FakeTarget) Signal(_ context.Context, sig int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return f.SignalErr
}

// Start records the call for tests that want to assert
// post-Signal restart behaviour and returns StartErr.
func (f *FakeTarget) Start(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	return f.StartErr
}

// CopyOut reads from the fake filesystem.
func (f *FakeTarget) CopyOut(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Files == nil {
		return nil, errors.New("fake: no files")
	}
	body, ok := f.Files[path]
	if !ok {
		return nil, fmt.Errorf("fake: file %q not present", path)
	}
	return append([]byte(nil), body...), nil
}

// CopyIn writes to the fake filesystem.
func (f *FakeTarget) CopyIn(_ context.Context, path string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Files == nil {
		f.Files = map[string][]byte{}
	}
	f.Files[path] = append([]byte(nil), body...)
	f.written = append(f.written, path)
	return nil
}

// SetMemoryLimit records the requested limit and returns
// MemoryLimitErr if set.  Tests can read MemoryLimits to
// assert the sequence of values applied (e.g.,
// [33554432, -1] for an apply + recovery-removes pair).
func (f *FakeTarget) SetMemoryLimit(_ context.Context, bytes int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memLimits = append(f.memLimits, bytes)
	return f.MemoryLimitErr
}

// MemoryLimits returns the sequence of limit values
// SetMemoryLimit was called with, in order.
func (f *FakeTarget) MemoryLimits() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64{}, f.memLimits...)
}

// ExecCalls returns a copy of every Exec invocation in order.
func (f *FakeTarget) ExecCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.execCmds))
	for i, c := range f.execCmds {
		out[i] = append([]string{}, c...)
	}
	return out
}

// Signals returns the list of signals seen, in order.
func (f *FakeTarget) Signals() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int{}, f.signals...)
}

// Written returns paths recorded by CopyIn, in order.
func (f *FakeTarget) Written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.written...)
}

func joinArgv(argv []string) string {
	out := ""
	for i, a := range argv {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

package faultinject_test

// Two properties of the fault-injection middleware, neither previously
// executed by a test (coverage ratchet):
//
//  1. EVERY injectable op honours a matching rule. This is what the
//     chaos and gameday gates rest on — an op that silently bypasses
//     injection makes those gates report a pass for a fault that was
//     never applied, which is worse than no gate at all.
//  2. EVERY method delegates when no rule matches. A wrapper that
//     drops a call would corrupt normal operation while the fault
//     harness is merely installed.
//
// Both are checked across the whole AllOps set rather than a sample,
// so a newly added op that forgets its matchAndRecord hook is caught.

import (
	"context"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
)

// recorder is a minimal inner plugin that notes which method ran.
type recorder struct{ last string }

func (r *recorder) Name() string { return "rec" }
func (r *recorder) Open(context.Context, storage.StorageConfig) error {
	r.last = "Open"
	return nil
}
func (r *recorder) Put(_ context.Context, key string, body io.Reader, _ storage.PutOptions) (storage.PutResult, error) {
	r.last = "Put"
	b, _ := io.ReadAll(body)
	return storage.PutResult{Key: key, Size: int64(len(b))}, nil
}
func (r *recorder) Get(context.Context, string) (io.ReadCloser, error) {
	r.last = "Get"
	return io.NopCloser(strings.NewReader("ok")), nil
}
func (r *recorder) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	r.last = "Stat"
	return storage.ObjectInfo{Key: key}, nil
}
func (r *recorder) List(_ context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	r.last = "List"
	return func(yield func(storage.ObjectInfo, error) bool) {
		yield(storage.ObjectInfo{Key: prefix}, nil)
	}
}
func (r *recorder) Delete(context.Context, string) error { r.last = "Delete"; return nil }
func (r *recorder) RenameIfNotExists(context.Context, string, string) error {
	r.last = "RenameIfNotExists"
	return nil
}
func (r *recorder) SetRetention(context.Context, string, time.Time, storage.WORMMode) error {
	r.last = "SetRetention"
	return nil
}
func (r *recorder) Barrier(context.Context) error      { r.last = "Barrier"; return nil }
func (r *recorder) Capabilities() storage.Capabilities { return storage.Capabilities{WORM: true} }
func (r *recorder) Close() error                       { r.last = "Close"; return nil }

// invoke calls the middleware method matching op and reports the error
// the caller would see.
func invoke(m *faultinject.Middleware, op faultinject.Op, key string) error {
	ctx := context.Background()
	switch op {
	case faultinject.OpPut:
		_, err := m.Put(ctx, key, strings.NewReader("x"), storage.PutOptions{})
		return err
	case faultinject.OpGet:
		_, err := m.Get(ctx, key)
		return err
	case faultinject.OpStat:
		_, err := m.Stat(ctx, key)
		return err
	case faultinject.OpList:
		for _, err := range m.List(ctx, key) {
			if err != nil {
				return err
			}
		}
		return nil
	case faultinject.OpDelete:
		return m.Delete(ctx, key)
	case faultinject.OpRename:
		return m.RenameIfNotExists(ctx, key, key+".dst")
	case faultinject.OpSetRetention:
		return m.SetRetention(ctx, key, time.Now().Add(time.Hour), storage.WORMCompliance)
	}
	return nil
}

var allOps = []struct {
	op   faultinject.Op
	name string
}{
	{faultinject.OpPut, "Put"},
	{faultinject.OpGet, "Get"},
	{faultinject.OpStat, "Stat"},
	{faultinject.OpList, "List"},
	{faultinject.OpDelete, "Delete"},
	{faultinject.OpRename, "RenameIfNotExists"},
	{faultinject.OpSetRetention, "SetRetention"},
}

func TestFaultInject_EveryOpHonoursItsRule(t *testing.T) {
	boom := errors.New("injected")
	for _, tc := range allOps {
		t.Run(tc.name, func(t *testing.T) {
			m := faultinject.New(&recorder{})
			m.Activate([]faultinject.Rule{{
				Name: "all", Ops: tc.op, Err: boom,
			}}, faultinject.ActivateOptions{})

			if err := invoke(m, tc.op, "any/key"); !errors.Is(err, boom) {
				t.Fatalf("%s did not honour its fault rule (got %v) — a gate asserting "+
					"this fault would report a pass for an injection that never happened", tc.name, err)
			}
		})
	}
}

func TestFaultInject_EveryOpDelegatesWhenNoRuleMatches(t *testing.T) {
	for _, tc := range allOps {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			m := faultinject.New(rec)
			// A rule that matches a DIFFERENT key prefix: active
			// middleware, non-matching rule.
			m.Activate([]faultinject.Rule{{
				Name: "other", Ops: faultinject.AllOps, KeyPrefix: "nomatch/",
				Err: errors.New("should not fire"),
			}}, faultinject.ActivateOptions{})

			if err := invoke(m, tc.op, "real/key"); err != nil {
				t.Fatalf("%s returned %v for a non-matching rule — the wrapper is "+
					"corrupting normal operation", tc.name, err)
			}
			if rec.last != tc.name {
				t.Errorf("%s did not reach the inner plugin (inner saw %q)", tc.name, rec.last)
			}
		})
	}
}

// The non-injectable methods must always pass through: the harness is
// installed for whole soak runs, so a dropped Barrier here would break
// durability for every test that merely has faults available.
func TestFaultInject_NonInjectableMethodsAlwaysDelegate(t *testing.T) {
	rec := &recorder{}
	m := faultinject.New(rec)
	m.Activate([]faultinject.Rule{{
		Name: "everything", Ops: faultinject.AllOps, Err: errors.New("boom"),
	}}, faultinject.ActivateOptions{})

	// Open is deliberately not injectable: a rule that could fail Open
	// would make the wrapper impossible to install at all.
	if err := m.Open(context.Background(), storage.StorageConfig{}); err != nil || rec.last != "Open" {
		t.Errorf("Open must pass through even with AllOps armed (err=%v, inner=%q)", err, rec.last)
	}
	if err := m.Barrier(context.Background()); err != nil || rec.last != "Barrier" {
		t.Errorf("Barrier must pass through even with AllOps armed (err=%v, inner=%q)", err, rec.last)
	}
	if caps := m.Capabilities(); !caps.WORM {
		t.Error("Capabilities must reflect the inner backend")
	}
	if err := m.Close(); err != nil || rec.last != "Close" {
		t.Errorf("Close not delegated (err=%v, inner=%q)", err, rec.last)
	}
}

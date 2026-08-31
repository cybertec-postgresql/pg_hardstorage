package throttle_test

// Every Throttle method that is NOT the bandwidth-shaped path must
// forward to the wrapped plugin with its arguments intact. These
// pass-throughs had no test executing them (coverage ratchet), and a
// dropped or mis-wired one is silent: a Barrier that never reaches the
// backend disables the durability fsync the WAL sink relies on before
// advancing SyncedLSN, and a SetRetention that never lands leaves a
// "WORM-locked" object deletable. Both look fine until the day they
// matter.

import (
	"context"
	"io"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
)

// spy records each call it receives, with the arguments that reached it.
type spy struct {
	calls []string
	// captured args for the assertions that care
	retKey  string
	retTime time.Time
	retMode storage.WORMMode
	renSrc  string
	renDst  string
	delKey  string
	statKey string
	listPfx string
	barrier int
	closed  int
}

func (s *spy) Name() string { s.calls = append(s.calls, "Name"); return "spy" }
func (s *spy) Open(context.Context, storage.StorageConfig) error {
	s.calls = append(s.calls, "Open")
	return nil
}
func (s *spy) Put(_ context.Context, key string, r io.Reader, _ storage.PutOptions) (storage.PutResult, error) {
	s.calls = append(s.calls, "Put")
	b, _ := io.ReadAll(r)
	return storage.PutResult{Key: key, Size: int64(len(b))}, nil
}
func (s *spy) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.calls = append(s.calls, "Get")
	return io.NopCloser(strings.NewReader("payload")), nil
}
func (s *spy) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	s.calls = append(s.calls, "Stat")
	s.statKey = key
	return storage.ObjectInfo{Key: key}, nil
}
func (s *spy) List(_ context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	s.calls = append(s.calls, "List")
	s.listPfx = prefix
	return func(yield func(storage.ObjectInfo, error) bool) {
		yield(storage.ObjectInfo{Key: prefix + "a"}, nil)
	}
}
func (s *spy) Delete(_ context.Context, key string) error {
	s.calls = append(s.calls, "Delete")
	s.delKey = key
	return nil
}
func (s *spy) RenameIfNotExists(_ context.Context, src, dst string) error {
	s.calls = append(s.calls, "RenameIfNotExists")
	s.renSrc, s.renDst = src, dst
	return nil
}
func (s *spy) SetRetention(_ context.Context, key string, until time.Time, mode storage.WORMMode) error {
	s.calls = append(s.calls, "SetRetention")
	s.retKey, s.retTime, s.retMode = key, until, mode
	return nil
}
func (s *spy) Barrier(context.Context) error {
	s.calls = append(s.calls, "Barrier")
	s.barrier++
	return nil
}
func (s *spy) Capabilities() storage.Capabilities {
	s.calls = append(s.calls, "Capabilities")
	return storage.Capabilities{WORM: true, DurabilityBarrier: true}
}
func (s *spy) Close() error { s.calls = append(s.calls, "Close"); s.closed++; return nil }

func (s *spy) saw(name string) bool {
	for _, c := range s.calls {
		if c == name {
			return true
		}
	}
	return false
}

func TestThrottle_DelegatesEveryMethodToInner(t *testing.T) {
	sp := &spy{}
	// A generous cap so throttling never dominates the timing.
	th := throttle.New(sp, 1<<30)
	ctx := context.Background()

	if got := th.Name(); !sp.saw("Name") {
		t.Errorf("Name not delegated (got %q)", got)
	}
	if err := th.Open(ctx, storage.StorageConfig{}); err != nil || !sp.saw("Open") {
		t.Errorf("Open not delegated: %v", err)
	}
	if _, err := th.Stat(ctx, "k1"); err != nil || sp.statKey != "k1" {
		t.Errorf("Stat key not forwarded: got %q", sp.statKey)
	}
	for range th.List(ctx, "pfx/") {
		break
	}
	if sp.listPfx != "pfx/" {
		t.Errorf("List prefix not forwarded: got %q", sp.listPfx)
	}
	if err := th.Delete(ctx, "k2"); err != nil || sp.delKey != "k2" {
		t.Errorf("Delete key not forwarded: got %q", sp.delKey)
	}
	if err := th.RenameIfNotExists(ctx, "a", "b"); err != nil || sp.renSrc != "a" || sp.renDst != "b" {
		t.Errorf("Rename args not forwarded: %q -> %q", sp.renSrc, sp.renDst)
	}

	// SetRetention: a WORM lock that never reaches the backend leaves
	// the object deletable while the repo believes it is locked.
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := th.SetRetention(ctx, "k3", until, storage.WORMCompliance); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	if sp.retKey != "k3" || !sp.retTime.Equal(until) || sp.retMode != storage.WORMCompliance {
		t.Errorf("SetRetention args mangled: key=%q until=%v mode=%v", sp.retKey, sp.retTime, sp.retMode)
	}

	// Barrier: the WAL sink calls this before advancing SyncedLSN. If
	// the wrapper swallows it, PG is told WAL is durable that isn't.
	if err := th.Barrier(ctx); err != nil || sp.barrier != 1 {
		t.Errorf("Barrier not delegated (count=%d)", sp.barrier)
	}

	// Capabilities must reflect the INNER backend — the wrapper adds no
	// features and must not mask any.
	if caps := th.Capabilities(); !caps.WORM || !caps.DurabilityBarrier {
		t.Errorf("Capabilities not delegated: %+v", caps)
	}
	if err := th.Close(); err != nil || sp.closed != 1 {
		t.Errorf("Close not delegated (count=%d)", sp.closed)
	}
}

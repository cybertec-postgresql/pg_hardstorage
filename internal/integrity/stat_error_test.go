package integrity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/integrity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// chunkStatFault fails every chunk Stat with err, and optionally cancels
// a context on the first one.
type chunkStatFault struct {
	storage.StoragePlugin
	err    error
	cancel context.CancelFunc
}

func (c *chunkStatFault) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if strings.HasPrefix(key, "chunks/") {
		if c.cancel != nil {
			c.cancel()
			return storage.ObjectInfo{}, ctx.Err()
		}
		return storage.ObjectInfo{}, c.err
	}
	return c.StoragePlugin.Stat(ctx, key)
}

// A throttled Stat is not a missing chunk: every chunk used to be
// counted Missing and the attestation signed "N chunks lost" over a 503.
func TestExecute_TransientStatIsNotMissing(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 3)
	sp := &chunkStatFault{StoragePlugin: f.sp, err: errors.New("503 SlowDown")}
	eng := integrity.NewEngine(integrity.EngineOptions{Storage: sp, Manifests: f.manifests, Verifier: f.verifier})
	r, err := eng.Execute(context.Background(), "", integrity.Strategy{Mode: "presence"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Chunks.Missing != 0 {
		t.Errorf("Missing = %d; a transient Stat error was counted as a lost chunk", r.Chunks.Missing)
	}
	if r.Chunks.PresenceUnchecked != 3 {
		t.Errorf("PresenceUnchecked = %d, want 3", r.Chunks.PresenceUnchecked)
	}
	if r.Status != integrity.StatusError {
		t.Errorf("Status = %s; a run that could not check presence (and found nothing wrong) is incomplete: error", r.Status)
	}
	for _, fl := range r.Chunks.Failures {
		if fl.Reason != "stat_failed" {
			t.Errorf("failure reason = %q, want stat_failed", fl.Reason)
		}
	}
}

// A cancelled run aborts; it does not produce an attestation claiming
// every remaining chunk is missing.
func TestExecute_CancelAborts(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "x", 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp := &chunkStatFault{StoragePlugin: f.sp, cancel: cancel}
	eng := integrity.NewEngine(integrity.EngineOptions{Storage: sp, Manifests: f.manifests, Verifier: f.verifier})
	r, err := eng.Execute(ctx, "", integrity.Strategy{Mode: "presence"}, "")
	if err == nil {
		t.Fatalf("cancelled run returned a Run (missing=%d) instead of an error", r.Chunks.Missing)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

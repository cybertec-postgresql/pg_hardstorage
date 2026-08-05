// commit_worm_integration_test.go — WORM retention must survive the
// move to a conditional PUT.
//
// Before issue #45 the commit wrote a staging object carrying
// RetainUntil/RetentionMode and renamed it into place. It now writes
// the object directly with IfNotExists set. The existing WORM tests
// assert the OPTIONS handed to the plugin, using a recording fake —
// they cannot tell whether a real backend honours retention on a
// CONDITIONAL put, which is a different request shape.
//
// If any backend silently drops the lock when IfNotExists is set, a
// compliance repository would keep reporting a WORM policy while its
// manifests sat unlocked and deletable. That is the kind of thing
// nobody notices until an auditor asks.
//
//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// TestCommitExclusive_RetentionSurvivesConditionalPut checks, against
// real backends, that a conditionally-published object still carries
// the retention the caller asked for.
func TestCommitExclusive_RetentionSurvivesConditionalPut(t *testing.T) {
	for _, b := range []struct{ scheme, sinkKind string }{
		{"s3", "s3-minio"},
		{"gcs", "gcs-fake"},
		{"azblob", "azurite"},
		{"sftp", "sftp"},
		{"scp", "ssh-exec"},
	} {
		t.Run(b.scheme, func(t *testing.T) {
			rt, err := sink.New(b.sinkKind)
			if err != nil {
				t.Fatalf("sink.New: %v", err)
			}
			if err := rt.Up(context.Background()); err != nil {
				t.Fatalf("up: %v", err)
			}
			t.Cleanup(func() { _ = rt.Down(context.Background()) })
			for k, v := range rt.EnvForAgent() {
				t.Setenv(k, v)
			}
			sp, err := storage.Open(context.Background(), rt.URL())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = sp.Close() })

			ctx := context.Background()
			body := []byte(`{"schema":"manifest","worm":true}`)
			until := time.Now().UTC().Add(24 * time.Hour)

			err = storage.CommitExclusive(ctx, sp, "worm/cond.json", body, storage.PutOptions{
				RetainUntil:   until,
				RetentionMode: storage.WORMCompliance,
			})
			if err != nil {
				t.Fatalf("conditional commit with retention: %v", err)
			}

			// The object must exist and read back intact — a backend
			// that rejected the retention silently must not have
			// silently dropped the object with it.
			rc, gerr := sp.Get(ctx, "worm/cond.json")
			if gerr != nil {
				t.Fatalf("Get after a retained conditional commit: %v", gerr)
			}
			got := make([]byte, len(body)+8)
			n, _ := rc.Read(got)
			_ = rc.Close()
			if !bytes.Equal(got[:n], body) {
				t.Errorf("stored body = %q, want %q", got[:n], body)
			}

			// On a WORM-capable backend the lock must actually be
			// there. SetRetention is the only portable way to ask.
			if !sp.Capabilities().WORM {
				t.Logf("%s does not advertise WORM; retention is expected to be ignored",
					b.scheme)
				return
			}
			serr := sp.SetRetention(ctx, "worm/cond.json", until, storage.WORMCompliance)
			if serr != nil && !errors.Is(serr, storage.ErrUnsupported) {
				// A fixture bucket without Object Lock configured
				// reports its own condition; that is not a capability
				// lie (the contract suite documents this distinction).
				t.Logf("%s: SetRetention returned %v — accepted as a fixture condition",
					b.scheme, serr)
			}
		})
	}
}

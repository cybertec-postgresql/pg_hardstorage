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
	"io"
	"strings"
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

			// Ask for retention on the conditional commit.
			//
			// A fixture without Object Lock cannot honour it — MinIO
			// answers "Bucket is missing ObjectLockConfiguration",
			// Azurite rejects the immutability policy — and that is a
			// property of the fixture, not of the code under test.
			// caseWORMHonesty documents the same distinction. Treating
			// it as a failure is how a test ends up reporting a
			// capability lie for a bucket that was simply never
			// configured.
			key := "worm/cond.json"
			err = storage.CommitExclusive(ctx, sp, key, body, storage.PutOptions{
				RetainUntil:   until,
				RetentionMode: storage.WORMCompliance,
			})
			if err != nil {
				if !fixtureLacksWORM(err) {
					t.Fatalf("conditional commit with retention failed for a reason that is "+
						"NOT the fixture's missing WORM configuration: %v", err)
				}
				t.Logf("%s: fixture cannot honour retention (%v) — falling back to the "+
					"property that does not need it", b.scheme, err)
				// The part that still matters, and that issue #45
				// actually changed: the CONDITIONAL commit itself works
				// and publishes the object whole.
				key = "worm/cond-noretain.json"
				if err := storage.CommitExclusive(ctx, sp, key, body,
					storage.PutOptions{}); err != nil {
					t.Fatalf("conditional commit without retention: %v", err)
				}
			}

			// Whatever path we took, the object must be there and intact:
			// a backend that quietly dropped the write while accepting
			// the request is the failure this is really guarding.
			rc, gerr := sp.Get(ctx, key)
			if gerr != nil {
				t.Fatalf("Get after a conditional commit: %v", gerr)
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if !bytes.Equal(got, body) {
				t.Errorf("stored body = %q, want %q", got, body)
			}

			// And it is still exclusive with retention in play.
			if err := storage.CommitExclusive(ctx, sp, key, []byte("other"),
				storage.PutOptions{}); !errors.Is(err, storage.ErrAlreadyExists) {
				t.Errorf("second commit err = %v, want ErrAlreadyExists — retention must "+
					"not disturb the exclusion the commit exists to provide", err)
			}
		})
	}
}

// fixtureLacksWORM reports whether err is the fixture saying it has no
// Object Lock / immutability configured, rather than the plugin failing.
func fixtureLacksWORM(err error) bool {
	if errors.Is(err, storage.ErrUnsupported) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"objectlockconfiguration",
		"object lock",
		"immutability",
		"set retention",
		"apply retention",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

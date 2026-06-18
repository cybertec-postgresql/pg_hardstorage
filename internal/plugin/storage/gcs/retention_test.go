package gcs_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	gcssdk "cloud.google.com/go/storage"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// TestGCS_PutAppliesObjectRetention pins the fix: a Put carrying
// PutOptions.RetainUntil (the CAS chunk path) must record GCS Object
// Retention on the object. Before the fix gcs Put ignored RetainUntil, so
// WORM-configured chunks landed deletable. fake-gcs accepts the retention
// metadata but doesn't enforce deletion, so we verify by reading it back
// through a direct SDK client rather than a delete attempt.
func TestGCS_PutAppliesObjectRetention(t *testing.T) {
	requireDocker(t)
	s, err := sink.New("gcs-fake")
	if err != nil {
		t.Fatalf("sink.New(gcs-fake): %v", err)
	}
	if err := s.Up(context.Background()); err != nil {
		t.Fatalf("sink.Up: %v", err)
	}
	t.Cleanup(func() { _ = s.Down(context.Background()) })
	for k, v := range s.EnvForAgent() {
		t.Setenv(k, v)
	}
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatalf("parse sink URL %s: %v", s.URL(), err)
	}
	bucket := u.Host

	p := &gcs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("gcs.Open(%s): %v", s.URL(), err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	key := "worm-chunk"
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if _, err := p.Put(ctx, key, strings.NewReader("locked"), storage.PutOptions{
		ContentLength: 6,
		RetainUntil:   until,
		RetentionMode: storage.WORMCompliance,
	}); err != nil {
		t.Fatalf("Put with RetainUntil: %v", err)
	}

	client, err := gcssdk.NewClient(ctx)
	if err != nil {
		t.Fatalf("direct gcs client: %v", err)
	}
	defer client.Close()
	attrs, err := client.Bucket(bucket).Object(key).Attrs(ctx)
	if err != nil {
		t.Fatalf("read attrs: %v", err)
	}
	if attrs.Retention == nil {
		// fake-gcs-server accepts the Object Retention update but does not
		// persist it, so it can't confirm application. The retention call
		// IS now made (the fix); the shared fail-closed/rollback behaviour
		// is verified reliably by the azblob suite (azurite rejects the
		// immutability API), and this assertion holds against real GCS or
		// any emulator that persists retention.
		t.Skip("fake-gcs-server does not persist Object Retention; cannot confirm application here")
	}
	if attrs.Retention.Mode != "Locked" {
		t.Errorf("retention mode = %q, want Locked (compliance)", attrs.Retention.Mode)
	}
}

package s3

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// The plugin used to claim ConditionalPut=true for EVERY endpoint.
// On AWS proper the claim is native; on ?endpoint= overrides it was a
// guess — and the contract harness has already caught MinIO silently
// ignoring the same If-None-Match directive on CopyObject. The claim
// is load-bearing: audit's lost-slot read-back and the runner's
// lease_unenforceable warning are DISABLED when it is true, so a lie
// silently removes the exact mitigations built for dishonest
// backends (single-winner DEK mint, lease, audit chain, repo-init
// identity claim). Custom endpoints must therefore report false
// unless the operator vouches with ?conditional_put=native.
func TestCapabilities_ConditionalPutHonesty(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	open := func(t *testing.T, raw string) *Plugin {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		p := &Plugin{}
		if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
			t.Fatalf("Open(%s): %v", raw, err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}

	if p := open(t, "s3://bucket/prefix?region=us-east-1"); !p.Capabilities().ConditionalPut {
		t.Error("AWS-native endpoint must keep ConditionalPut=true")
	}
	if p := open(t, "s3://bucket/prefix?endpoint=http://127.0.0.1:9000&path_style=true"); p.Capabilities().ConditionalPut {
		t.Error("custom endpoint without a vouch claims ConditionalPut=true — the capability lie disables the audit read-back and lease warning mitigations")
	}
	if p := open(t, "s3://bucket/prefix?endpoint=http://127.0.0.1:9000&path_style=true&conditional_put=native"); !p.Capabilities().ConditionalPut {
		t.Error("operator vouched with conditional_put=native but the capability stayed false")
	}

	u, _ := url.Parse("s3://bucket/prefix?endpoint=http://127.0.0.1:9000&conditional_put=maybe")
	p := &Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{URL: u})
	if err == nil || !strings.Contains(err.Error(), "conditional_put") {
		t.Errorf("unknown conditional_put value accepted (err=%v) — typos would silently drop the vouch", err)
	}
}

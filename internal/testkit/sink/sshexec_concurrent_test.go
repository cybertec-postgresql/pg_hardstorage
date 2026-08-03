// sshexec_concurrent_test.go — several ssh-exec instances must be able
// to be up at once, each accepting its OWN key.
//
// This is not a hypothetical. `make test-integration` passes no -p, so
// go test runs packages concurrently, and two packages use this sink:
// internal/plugin/storage's wiring test and .../storage/scp's contract
// test. The fixture used to bake each instance's public key into an
// image under one shared tag, so the later build re-pointed the tag and
// the earlier instance started a container authorising the other
// package's key.
//
// It failed on roughly one PG-matrix leg in four, always as ~20
// simultaneous "unable to authenticate, attempted methods [none
// publickey]" failures inside whichever contract suite lost — a
// signature that points at the scp plugin and not at the fixture that
// actually caused it.
//
// Running instances concurrently in ONE package makes that race
// deterministic: without the fix this test fails every time, with the
// same error text CI produced.
//
//go:build integration

package sink_test

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// TestSSHExec_ConcurrentInstancesEachAcceptOwnKey brings three
// instances up simultaneously and drives a real Put through each.
//
// Each instance is exercised through storage.Open on its own URL, so
// the assertion covers the whole path a test actually uses — not just
// that the containers started.
func TestSSHExec_ConcurrentInstancesEachAcceptOwnKey(t *testing.T) {
	const instances = 3

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, instances)

	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = exerciseSSHExecInstance(ctx, i)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d: %v\n\nEach ssh-exec instance must accept the keypair "+
				"IT generated. An authentication failure here means instances are sharing "+
				"something they must not — historically an image tag with the key baked in, "+
				"which made whichever package lost the build race fail with exactly this "+
				"error.", i, err)
		}
	}
}

// exerciseSSHExecInstance runs one instance end to end. Config goes
// through Extras rather than the env vars EnvForAgent returns: env is
// process-global, so concurrent instances would overwrite each other's
// settings and this test would measure its own interference instead of
// the fixture's. Extras addresses exactly one plugin instance.
func exerciseSSHExecInstance(ctx context.Context, i int) error {
	rt, err := sink.New("ssh-exec")
	if err != nil {
		return err
	}
	if err := rt.Up(ctx); err != nil {
		return err
	}
	defer func() { _ = rt.Down(context.Background()) }()

	u, err := url.Parse(rt.URL())
	if err != nil {
		return err
	}
	p := &scp.Plugin{}
	if err := p.Open(ctx, storage.StorageConfig{URL: u, Extras: rt.Extras()}); err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	body := strings.Repeat("concurrent-instance", 64)
	if _, err := p.Put(ctx, "probe/concurrent", strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		return err
	}
	return nil
}

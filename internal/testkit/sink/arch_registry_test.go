//go:build integration

package sink_test

// arch_registry_test.go — the live half of the fixture-image
// architecture guard.
//
// arch_test.go proves the check itself works against canned manifests.
// This one asks the actual registries whether the actual pins can run
// on the machine the suite is running on. It is integration-tagged
// because it needs network, and it is the test that would have caught
// atmoz/sftp:alpine-3.7 on aarch64 before nine tests failed for a
// reason none of them named.

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func TestSinkImages_PublishForHostArch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	err := sink.VerifyAllImageArch(ctx)

	// "The registry would not answer" is not "the image cannot run
	// here", and saying the second when only the first is true is
	// actively misleading. Docker Hub refuses anonymous manifest
	// queries, so on any host without credentials this test used to
	// announce that a pinned image could not run on linux/arm64 —
	// about images sitting in the local daemon, built for linux/arm64,
	// working. VerifyAllImageArch now consults the daemon when the
	// registry declines and only reports this when neither could
	// settle it.
	if errors.Is(err, sink.ErrRegistryUnavailable) {
		t.Skipf("cannot reach the registry to verify image architectures, and the images "+
			"are not present locally to inspect instead — this says nothing about the pins.\n\n%v\n\n"+
			"Authenticate to the registry (docker login) or pre-pull the fixtures "+
			"(pg_hardstorage_testkit image pull-sinks) to make this test meaningful here.", err)
	}
	if err != nil {
		t.Fatalf("a pinned fixture image cannot run on %s/%s.\n\n%v\n\n"+
			"Every sink image is pulled from a registry, base images included: a locally-built "+
			"fixture still needs its FROM line to resolve here. An image without a manifest for "+
			"this host does not fail loudly — its container exits at startup and the backend it "+
			"fronts silently loses all real-server coverage.",
			runtime.GOOS, runtime.GOARCH, err)
	}
	// Deliberately "can run" rather than "publishes a manifest": some
	// pins may have been settled against the local daemon rather than
	// the registry, and overstating which is what this test was fixed
	// for.
	t.Logf("all %d sink image pins can run on %s/%s",
		len(sink.KnownKinds()), runtime.GOOS, runtime.GOARCH)
}

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
	"runtime"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func TestSinkImages_PublishForHostArch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := sink.VerifyAllImageArch(ctx); err != nil {
		t.Fatalf("a pinned fixture image cannot run on %s/%s.\n\n%v\n\n"+
			"Every sink image is pulled from a registry, base images included: a locally-built "+
			"fixture still needs its FROM line to resolve here. An image without a manifest for "+
			"this host does not fail loudly — its container exits at startup and the backend it "+
			"fronts silently loses all real-server coverage.",
			runtime.GOOS, runtime.GOARCH, err)
	}
	t.Logf("all %d sink image pins publish a %s/%s manifest",
		len(sink.KnownKinds()), runtime.GOOS, runtime.GOARCH)
}

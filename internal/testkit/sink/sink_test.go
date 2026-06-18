package sink_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// TestNew_KnownKinds runs without Docker — pure plumbing.
// Exercises the kind-name dispatch and the unknown-kind
// error path.  Real Up/Down testing lives in build-tagged
// integration tests below.
func TestNew_KnownKinds(t *testing.T) {
	for _, k := range sink.KnownKinds() {
		r, err := sink.New(k)
		if err != nil {
			t.Errorf("New(%q): %v", k, err)
			continue
		}
		if r.Name() != k {
			t.Errorf("New(%q).Name() = %q", k, r.Name())
		}
	}
}

func TestNew_UnknownKind(t *testing.T) {
	_, err := sink.New("bogus-sink")
	if err == nil {
		t.Fatal("expected error on unknown kind")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error %q does not mention unknown kind", err.Error())
	}
	// Error must list the known set so an operator's typo
	// surfaces with a corrective hint.
	for _, k := range sink.KnownKinds() {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q should list known kind %q", err.Error(), k)
		}
	}
}

func TestKnownKinds_Sorted(t *testing.T) {
	got := sink.KnownKinds()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("KnownKinds not sorted: %v", got)
			return
		}
	}
}

func TestSinkImages_TagsArePinned(t *testing.T) {
	// Pinned tags are required for reproducibility and
	// air-gap pre-pull semantics.  `:latest` would
	// silently drift.
	for k, img := range sink.SinkImages {
		if !strings.Contains(img, ":") {
			t.Errorf("sink %q image %q missing tag", k, img)
			continue
		}
		if strings.HasSuffix(img, ":latest") {
			t.Errorf("sink %q uses :latest (must be pinned for air-gap)", k)
		}
	}
}

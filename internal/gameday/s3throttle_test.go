package gameday_test

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
)

// Without --repo, s3_throttle injected nothing and returned Pass=true
// ("passes-by-contract") — a hollow pass indistinguishable, from the
// exit code and `gameday report`, from a drill that ran a 503 storm.
// It must refuse the way agent_kill and patroni_split_brain do.
func TestS3Throttle_NoRepoIsMisconfiguredNotPass(t *testing.T) {
	res, err := gameday.Run(context.Background(), "s3_throttle", gameday.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pass {
		t.Fatal("s3_throttle without --repo reported Pass=true having injected nothing")
	}
	if !res.Misconfigured {
		t.Errorf("want Misconfigured=true (a usage refusal), got %+v", res)
	}
}

// With a repository the drill drives the fault and proves recovery by
// reading the post-storm write back byte-identical.
func TestS3Throttle_WithRepoDrivesAndVerifiesRecovery(t *testing.T) {
	res, err := gameday.Run(context.Background(), "s3_throttle", gameday.RunOptions{
		RepoURL: "file://" + t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pass {
		t.Fatalf("s3_throttle against a healthy repo failed: %s", res.Failure)
	}
	kinds := map[string]bool{}
	for _, e := range res.Evidence {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"fault_active", "fault_observed", "fault_cleared", "recovered"} {
		if !kinds[k] {
			t.Errorf("evidence missing %q", k)
		}
	}
}

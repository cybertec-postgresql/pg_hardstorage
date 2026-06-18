package bisect_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/bisect"
)

func TestBisect_FindsRegressor(t *testing.T) {
	// Newest..oldest ordering: c5 (bad), c4 (bad), c3 (regressor), c2 (good), c1 (good)
	commits := []string{"c5", "c4", "c3", "c2", "c1"}
	outcomes := map[string]bisect.Outcome{
		"c5": bisect.Bad,
		"c4": bisect.Bad,
		"c3": bisect.Bad,
		"c2": bisect.Good,
		"c1": bisect.Good,
	}
	r, err := bisect.Run(context.Background(), bisect.Options{
		CommitRange: commits,
		Runner:      bisect.FromMap(outcomes),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.FirstBadSHA != "c3" {
		t.Errorf("FirstBadSHA = %q, want c3", r.FirstBadSHA)
	}
}

func TestBisect_HandlesSkippedCommits(t *testing.T) {
	commits := []string{"c5", "c4", "c3", "c2", "c1"}
	outcomes := map[string]bisect.Outcome{
		"c5": bisect.Bad,
		"c4": bisect.Skip, // unbuildable middle
		"c3": bisect.Bad,
		"c2": bisect.Good,
		"c1": bisect.Good,
	}
	r, err := bisect.Run(context.Background(), bisect.Options{
		CommitRange: commits,
		Runner:      bisect.FromMap(outcomes),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.FirstBadSHA != "c3" {
		t.Errorf("FirstBadSHA = %q", r.FirstBadSHA)
	}
}

func TestBisect_RefusesNonBadEnd(t *testing.T) {
	commits := []string{"c2", "c1"}
	outcomes := map[string]bisect.Outcome{"c2": bisect.Good, "c1": bisect.Good}
	_, err := bisect.Run(context.Background(), bisect.Options{
		CommitRange: commits,
		Runner:      bisect.FromMap(outcomes),
	})
	if err == nil || !strings.Contains(err.Error(), "bad-end") {
		t.Errorf("expected bad-end error, got %v", err)
	}
}

func TestBisect_RefusesNonGoodEnd(t *testing.T) {
	commits := []string{"c2", "c1"}
	outcomes := map[string]bisect.Outcome{"c2": bisect.Bad, "c1": bisect.Bad}
	_, err := bisect.Run(context.Background(), bisect.Options{
		CommitRange: commits,
		Runner:      bisect.FromMap(outcomes),
	})
	if err == nil || !strings.Contains(err.Error(), "good-end") {
		t.Errorf("expected good-end error, got %v", err)
	}
}

func TestBisect_TwoCommitRangeReturnsImmediate(t *testing.T) {
	commits := []string{"new", "old"}
	outcomes := map[string]bisect.Outcome{"new": bisect.Bad, "old": bisect.Good}
	r, err := bisect.Run(context.Background(), bisect.Options{
		CommitRange: commits,
		Runner:      bisect.FromMap(outcomes),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.FirstBadSHA != "new" {
		t.Errorf("FirstBadSHA = %q, want new", r.FirstBadSHA)
	}
}

func TestBisect_RecordsSteps(t *testing.T) {
	commits := []string{"c5", "c4", "c3", "c2", "c1"}
	outcomes := map[string]bisect.Outcome{
		"c5": bisect.Bad, "c4": bisect.Bad, "c3": bisect.Bad,
		"c2": bisect.Good, "c1": bisect.Good,
	}
	r, _ := bisect.Run(context.Background(), bisect.Options{
		CommitRange: commits, Runner: bisect.FromMap(outcomes),
	})
	if len(r.Steps) < 3 {
		t.Errorf("expected at least 3 steps, got %d", len(r.Steps))
	}
}

func TestSafeMap_Concurrent(t *testing.T) {
	m := bisect.NewSafeMap()
	m.Set("c1", bisect.Good)
	m.Set("c2", bisect.Bad)
	r := m.Runner()
	o, err := r(context.Background(), "c2")
	if err != nil || o != bisect.Bad {
		t.Errorf("expected Bad; got %v err=%v", o, err)
	}
}

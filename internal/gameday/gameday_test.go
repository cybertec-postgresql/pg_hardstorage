package gameday_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
)

func TestList_NonEmpty(t *testing.T) {
	scenarios := gameday.List()
	if len(scenarios) == 0 {
		t.Fatal("List() should return at least the v0.1 scenarios")
	}
	wantNames := map[string]bool{
		"agent_kill":       false,
		"s3_throttle":      false,
		"patroni_failover": false,
	}
	for _, s := range scenarios {
		if _, ok := wantNames[s.Name]; ok {
			wantNames[s.Name] = true
		}
		if s.Name == "" || s.Description == "" || s.Tier == "" {
			t.Errorf("scenario %+v missing required fields", s)
		}
		if s.Run == nil {
			t.Errorf("scenario %s has nil Run", s.Name)
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("expected scenario %q not registered", name)
		}
	}
}

func TestGet_UnknownScenario(t *testing.T) {
	_, err := gameday.Get("not-a-real-scenario")
	if !errors.Is(err, gameday.ErrNoSuchScenario) {
		t.Errorf("expected ErrNoSuchScenario; got %v", err)
	}
}

func TestRun_DryRun_AlwaysPasses(t *testing.T) {
	for _, name := range []string{"agent_kill", "s3_throttle", "patroni_failover"} {
		res, err := gameday.Run(context.Background(), name, gameday.RunOptions{DryRun: true})
		if err != nil {
			t.Fatalf("%s dry-run: %v", name, err)
		}
		if !res.Pass {
			t.Errorf("%s dry-run: Pass=%v, want true", name, res.Pass)
		}
		if !res.DryRun {
			t.Errorf("%s dry-run: DryRun flag not set on result", name)
		}
		if res.Schema != gameday.SchemaResult {
			t.Errorf("%s schema = %q", name, res.Schema)
		}
		if len(res.Evidence) == 0 {
			t.Errorf("%s should record at least one evidence event in dry-run mode", name)
		}
	}
}

func TestRun_NonExistent(t *testing.T) {
	_, err := gameday.Run(context.Background(), "no-such-scenario", gameday.RunOptions{})
	if !errors.Is(err, gameday.ErrNoSuchScenario) {
		t.Errorf("expected ErrNoSuchScenario; got %v", err)
	}
}

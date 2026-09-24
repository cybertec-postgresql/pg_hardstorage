package inject_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

// The repo-corruption faults picked a random file across the whole
// repository, which the soak's cells share: the damage landed on
// whichever cell owned the file, and that innocent cell's next restore
// failed. Injected on behalf of a deployment, they touch only its files.
func TestRepoCorruptionScopedToDeployment(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{"manifest_targeted_corruption(target=repo)", "/var/lib/pg_hardstorage/repo/manifests/cell-a "},
		{"truncated_wal_segment(target=repo)", "/var/lib/pg_hardstorage/repo/wal/cell-a "},
		{"missing_wal_segment(target=repo)", "/var/lib/pg_hardstorage/repo/wal/cell-a "},
	} {
		t.Run(tc.action, func(t *testing.T) {
			repo := &inject.FakeTarget{NameStr: "r", RoleStr: "repo",
				ExecFunc: func(argv []string) ([]byte, error) {
					if strings.Contains(strings.Join(argv, " "), "find ") {
						return []byte("/var/lib/pg_hardstorage/repo/x/f\n"), nil
					}
					return nil, nil
				}}
			ts := inject.NewStaticTargetSet([]inject.Target{repo}, 1)
			if _, err := inject.DefaultRegistry.ApplyForDeployment(context.Background(), tc.action, ts, "cell-a"); err != nil {
				t.Fatal(err)
			}
			find := strings.Join(repo.ExecCalls()[0], " ")
			if !strings.Contains(find, "find "+tc.want) {
				t.Errorf("search not confined to the deployment: %s", find)
			}
			if !inject.DefaultRegistry.CorruptsRepo(tc.action) {
				t.Error("CorruptsRepo must recognise the fault")
			}
		})
	}
	if inject.DefaultRegistry.CorruptsRepo("signal(target=pg, sig=9)") {
		t.Error("signal does not corrupt the repository")
	}
}

// A corruption fault that found nothing to corrupt changed no byte; it
// must not be counted as damage the product survived.
func TestRepoCorruptionWithNothingToCorruptIsNotApplied(t *testing.T) {
	repo := &inject.FakeTarget{NameStr: "r", RoleStr: "repo"} // find prints nothing
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 1)
	for _, a := range []string{"manifest_targeted_corruption(target=repo)",
		"truncated_wal_segment(target=repo)", "missing_wal_segment(target=repo)"} {
		if _, err := inject.DefaultRegistry.Apply(context.Background(), a, ts); !errors.Is(err, inject.ErrNotApplicable) {
			t.Errorf("%s with no files: want ErrNotApplicable, got %v", a, err)
		}
	}
}

func TestRepoCorruptionRejectsUnsafeDeployment(t *testing.T) {
	repo := &inject.FakeTarget{NameStr: "r", RoleStr: "repo"}
	ts := inject.NewStaticTargetSet([]inject.Target{repo}, 1)
	if _, err := inject.DefaultRegistry.ApplyForDeployment(context.Background(),
		"missing_wal_segment(target=repo)", ts, "a; rm -rf /"); err == nil {
		t.Fatal("a deployment name with shell metacharacters must be refused")
	}
}

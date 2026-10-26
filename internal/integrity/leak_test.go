package integrity_test

import (
	"context"
	"os"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/integrity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// `verify` runs repeatedly from the agent/cron: every pass builds a spill
// set, streams manifests and fetches chunk samples. Run it many times in
// one process (presence, content-sample, cancelled) and check nothing
// accumulates.
func TestLeak_IntegrityEngineRepeated(t *testing.T) {
	f := newFixture(t)
	f.commitBackup(t, "db1", "a", 20)
	f.commitBackup(t, "db1", "b", 35)
	eng := integrity.NewEngine(integrity.EngineOptions{Storage: f.sp, Manifests: f.manifests, Verifier: f.verifier, CAS: f.cas})
	n := 150
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 2000
	}
	step := func(i int) {
		strat := integrity.DefaultStrategy()
		ctx := context.Background()
		switch i % 3 {
		case 1:
			strat = integrity.Strategy{Mode: "content-sample", Percent: 100, Seed: int64(i)}
		case 2:
			c, cancel := context.WithCancel(ctx)
			cancel()
			ctx = c
		}
		_, _ = eng.Execute(ctx, "db1", strat, "leak")
	}
	for i := 0; i < 6; i++ {
		step(i)
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, n, n/16, 8<<20, step)
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

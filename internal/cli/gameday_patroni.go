// gameday_patroni.go — adapts the production Patroni client to the
// game-day scenario's driver seam.
//
// internal/gameday is compiled into the shipped binary and deliberately
// imports neither internal/patroni nor the pg layer: it declares a
// narrow PatroniDriver interface and lets its caller supply one. This
// file is that caller. Keeping the adapter here means the scenario can
// be unit-tested with a fake, driven against a real 3-node cluster by
// the integration suite, and wired to an operator's own cluster by the
// CLI — all through the same seam.

package cli

import (
	"context"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// patroniGameDayDriver satisfies gameday.PatroniDriver.
type patroniGameDayDriver struct{ c *patroni.Client }

func (d patroniGameDayDriver) Leader(ctx context.Context) (string, uint32, error) {
	m, err := d.c.Leader(ctx)
	if err != nil {
		return "", 0, err
	}
	return m.Name, m.Timeline, nil
}

func (d patroniGameDayDriver) Switchover(ctx context.Context, leader string) error {
	// Candidate is left empty so Patroni picks the healthiest replica.
	// A drill that hand-picks the target tests our choice, not the
	// cluster's.
	return d.c.Switchover(ctx, patroni.SwitchoverRequest{Leader: leader})
}

// gameDayPatroniDriver builds a driver for the named deployment, or
// returns nil when the deployment has no Patroni block.
//
// Returning (nil, nil) rather than an error is deliberate: the scenario
// itself reports the missing endpoint, with text that names the
// setting to configure. Failing here instead would produce a usage
// error for `gameday run agent_kill` too, which needs no cluster.
func gameDayPatroniDriver(deployment, overrideURL string) (gameday.PatroniDriver, error) {
	// An explicit --patroni-url wins: it lets an operator drill a
	// cluster that has no deployment entry yet, which is the common
	// case the first time somebody tries this.
	if overrideURL != "" {
		c, err := patroni.NewClient(overrideURL)
		if err != nil {
			return nil, fmt.Errorf("gameday: build Patroni client for --patroni-url: %w", err)
		}
		return patroniGameDayDriver{c: c}, nil
	}
	if deployment == "" {
		return nil, nil
	}
	// Same non-fatal load posture as deploymentDefaults: an unreadable
	// config must not turn `gameday run agent_kill` into a usage error.
	pth, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, nil
	}
	loaded, err := config.Load(pth)
	if err != nil || loaded == nil {
		return nil, nil
	}
	dep, ok := loaded.Config.Deployments[deployment]
	if !ok || !dep.Patroni.IsEnabled() {
		return nil, nil
	}
	c, err := patroni.NewClient(dep.Patroni.URL, patroniClientOpts(dep.Patroni)...)
	if err != nil {
		return nil, fmt.Errorf("gameday: build Patroni client for %q: %w", deployment, err)
	}
	return patroniGameDayDriver{c: c}, nil
}

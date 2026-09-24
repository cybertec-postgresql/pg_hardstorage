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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
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

// LeaderAddr returns the current leader's PG host and port, so the slot
// observation can connect to whoever leads NOW rather than to the
// static DSN (which, after the switchover, names the old leader).
func (d patroniGameDayDriver) LeaderAddr(ctx context.Context) (string, int, error) {
	m, err := d.c.Leader(ctx)
	if err != nil {
		return "", 0, err
	}
	return m.Host, m.Port, nil
}

// leaderAddresser is the slice of the driver the slot observation
// needs. A driver that cannot say where the leader is leaves slot
// continuity unmeasured rather than measured on the wrong node.
type leaderAddresser interface {
	LeaderAddr(ctx context.Context) (host string, port int, err error)
}

// gameDaySlotName is the slot the agent's leader-follow loop keeps on
// the leader for this deployment: patroni.slot, else the leader-role
// entry of patroni.slots (Mechanism 3), else the agent's default name.
// The drill used to look only at patroni.slot, so a Mechanism 3
// deployment was measured on a slot the agent never uses.
func gameDaySlotName(deployment string, p config.PatroniConfig) (string, error) {
	if p.Slot != "" {
		return p.Slot, nil
	}
	if len(p.Slots) > 0 {
		for _, s := range p.Slots {
			if role, err := parseSlotRole(s.Role); err == nil && role == follower.SlotRoleLeader && s.Name != "" {
				return s.Name, nil
			}
		}
		return "", fmt.Errorf("patroni.slots has no leader-role slot, so there is no leader slot whose continuity a switchover could break")
	}
	return "pg_hardstorage_" + deployment, nil
}

// gameDayLeaderDSN splices the current leader's host/port into the
// deployment's DSN — the same splice the agent's leader-follow loop
// makes (patroniDSNFor) — so credentials and options carry over.
func gameDayLeaderDSN(ctx context.Context, la leaderAddresser, staticDSN string) (string, error) {
	if la == nil {
		return "", fmt.Errorf("no Patroni endpoint to locate the current leader")
	}
	host, port, err := la.LeaderAddr(ctx)
	if err != nil {
		return "", fmt.Errorf("locate the current leader: %w", err)
	}
	dsn := patroniDSNFor(staticDSN)(host, port)
	if dsn == "" {
		return "", fmt.Errorf("cannot point pg_connection at leader %s:%d (unparseable DSN or leader without host/port)", host, port)
	}
	return dsn, nil
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
	pOpts, pErr := patroniClientOpts(dep.Patroni)
	if pErr != nil {
		return nil, pErr
	}
	c, err := patroni.NewClient(dep.Patroni.URL, pOpts...)
	if err != nil {
		return nil, fmt.Errorf("gameday: build Patroni client for %q: %w", deployment, err)
	}
	return patroniGameDayDriver{c: c}, nil
}

// gameDayObserveSlot builds the ObserveSlot seam for the named
// deployment, or returns nil when it cannot be measured.
//
// Without this, runPatroniFailover takes its "unmeasured" branch and
// asserts only that the leader moved. That is a hollow pass in a
// scenario declared tier L4 — the tier an auditor reads as "we tested
// catastrophic failover" — because the invariant it exists to check is
// that a planned switchover does not COST us WAL, and the leader moving
// is not evidence of that.
//
// The measurement is the same one the agent's leader-follow loop makes:
// find-or-create the deployment's replication slot on whoever is now
// the leader, with the archive frontier as the confirmed position, and
// read the gap EnsureSlot computes. Reusing that path rather than
// inventing a second one is the point — what the drill proves is what
// the production reconciler would have done.
//
// Returns nil (rather than an error) when the deployment has no PG
// connection or no repository to compare against: the scenario reports
// the missing measurement itself, and refusing here would break
// `gameday run agent_kill`, which needs neither.
//
// The connection is resolved at each observation from Patroni's current
// leader (drv), not from the static pg_connection: after the
// switchover that DSN points at the demoted node, and the drill would
// have "measured" the old leader's slot while the new leader's went
// unexamined.
func gameDayObserveSlot(deployment, repoURL string, drv gameday.PatroniDriver) func(ctx context.Context) (*gameday.SlotObservation, error) {
	if deployment == "" || repoURL == "" {
		return nil
	}
	la, _ := drv.(leaderAddresser)
	pth, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil
	}
	loaded, err := config.Load(pth)
	if err != nil || loaded == nil {
		return nil
	}
	dep, ok := loaded.Config.Deployments[deployment]
	if !ok || dep.PGConnection == "" {
		return nil
	}
	staticDSN := dep.PGConnection
	patroniCfg := dep.Patroni

	return func(ctx context.Context) (*gameday.SlotObservation, error) {
		slotName, serr := gameDaySlotName(deployment, patroniCfg)
		if serr != nil {
			return nil, fmt.Errorf("gameday: %w", serr)
		}
		dsn, derr := gameDayLeaderDSN(ctx, la, staticDSN)
		if derr != nil {
			return nil, fmt.Errorf("gameday: %w", derr)
		}

		_, sp, rerr := repo.Open(ctx, repoURL)
		if rerr != nil {
			return nil, fmt.Errorf("gameday: open repository to read the archive frontier: %w", rerr)
		}
		defer sp.Close()

		regConn, cerr := pg.Connect(ctx, dsn, pg.ModeRegular)
		if cerr != nil {
			return nil, fmt.Errorf("gameday: connect to the leader: %w", cerr)
		}
		defer regConn.Close(ctx)

		replConn, cerr := pg.Connect(ctx, dsn, pg.ModeReplication)
		if cerr != nil {
			return nil, fmt.Errorf("gameday: open replication connection to the leader: %w", cerr)
		}
		defer replConn.Close(ctx)

		tli, terr := currentTimelineOf(ctx, regConn)
		if terr != nil {
			return nil, fmt.Errorf("gameday: read the leader's timeline: %w", terr)
		}

		// archiveFrontierForLeader is the same lookup the agent's
		// coordinator uses, including its fall back to the previous
		// timeline — which is essential here, because the drill
		// observes immediately after a promotion and the new timeline
		// has nothing archived under it yet. Passing zero would make
		// EnsureSlot skip the gap calculation and the drill would
		// measure nothing while appearing to.
		frontier, ferr := archiveFrontierForLeader(ctx, sp, deployment, tli)
		if ferr != nil {
			return nil, fmt.Errorf("gameday: read the archive frontier: %w", ferr)
		}

		res, eerr := replication.EnsureSlot(ctx, regConn, replConn, slotName, frontier)
		if eerr != nil {
			return nil, fmt.Errorf("gameday: ensure replication slot %q: %w", slotName, eerr)
		}
		if res == nil {
			return nil, fmt.Errorf("gameday: ensure replication slot %q returned nothing", slotName)
		}
		return &gameday.SlotObservation{
			Outcome:  string(res.Outcome),
			GapBytes: res.GapBytes,
		}, nil
	}
}

// currentTimelineOf reads the timeline the server is on.
func currentTimelineOf(ctx context.Context, conn *pg.Conn) (uint32, error) {
	rows, err := conn.PgConn().Exec(ctx, "SELECT timeline_id FROM pg_control_checkpoint()").ReadAll()
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		for _, row := range r.Rows {
			if len(row) == 0 || row[0] == nil {
				continue
			}
			var tli uint64
			if _, serr := fmt.Sscanf(string(row[0]), "%d", &tli); serr != nil {
				return 0, serr
			}
			return uint32(tli), nil
		}
	}
	return 0, fmt.Errorf("pg_control_checkpoint() returned no timeline_id")
}

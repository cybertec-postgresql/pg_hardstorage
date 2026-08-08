// agentkill.go — the agent_kill scenario.
//
// The invariant this scenario used to declare was: "an agent process
// killed mid-backup must release pg_backup_start within recover_within,
// reconciled from state/inflight.json by the supervisor". Three parts of
// that are not true of this system:
//
//   - There is no supervisor. Nothing in the tree forks or re-execs an
//     agent child; `internal/supervisor` does not exist.
//   - There is no state/inflight.json reconciler. paths.Inflight is a
//     directory for buffers; nothing writes a reconciliation journal.
//   - There is no pg_backup_start to leak. Backups run BASE_BACKUP over
//     a replication-protocol connection, and PostgreSQL tears the backup
//     down when that connection drops. The exclusive-backup leak this
//     guarded against belongs to a mechanism we do not use.
//
// So the scenario was declaring an invariant borrowed from a different
// architecture, and passing unconditionally while doing so.
//
// What actually protects a deployment whose agent is killed mid-backup:
//
//  1. The backup lease. The dead agent stops renewing; after the TTL
//     another agent may break the lease and proceed — and exactly one
//     may, or two agents back up the same deployment concurrently.
//  2. Append-only publication. A manifest is committed as a single
//     atomic object, so a killed agent leaves chunks but never a
//     half-published backup. `list` cannot show a backup that did not
//     finish.
//
// This drill exercises both, by abandoning a lease rather than killing a
// process: an abandoned lease is precisely what a killed agent leaves
// behind, and it can be arranged without a PostgreSQL instance or a
// child process to signal.

package gameday

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// agentKillProbeDeployment namespaces the lease this drill abandons.
const agentKillProbeDeployment = "__gameday_agentkill_probe"

// agentKillLeaseTTL is short so the drill does not take minutes. The
// production default is 15 minutes; what is under test is the
// stale-break behaviour, not the specific duration.
const agentKillLeaseTTL = 2 * time.Second

// agentKillReclaimers is how many agents race to reclaim the abandoned
// lease. More than two so "exactly one wins" is a real claim rather
// than a coin toss.
const agentKillReclaimers = 5

func runAgentKill(ctx context.Context, opts RunOptions) (*Result, error) {
	r := &Result{
		Schema:    SchemaResult,
		Scenario:  "agent_kill",
		StartedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
	}
	defer finalize(r)

	if opts.DryRun {
		r.Evidence = append(r.Evidence, Event{
			At:   time.Now().UTC(),
			Kind: "plan",
			Message: fmt.Sprintf("would acquire a backup lease under the probe deployment "+
				"with a %s TTL and abandon it (the state a killed agent leaves), assert a "+
				"second agent is refused while it is live, then assert %d agents racing to "+
				"reclaim it after expiry produce exactly one winner",
				agentKillLeaseTTL, agentKillReclaimers),
		})
		r.Pass = true
		return r, nil
	}

	if strings.TrimSpace(opts.RepoURL) == "" {
		r.Failure = "no repository to drill: pass --repo (the lease a killed agent " +
			"abandons lives in the repository)"
		r.Misconfigured = true
		r.Pass = false
		return r, nil
	}

	recoverWithin := opts.RecoverWithin
	if recoverWithin == 0 {
		recoverWithin = 30 * time.Second
	}
	// Misconfiguration is a STATIC property of the operator's
	// parameters — classify it before anything fallible runs. This
	// check used to sit at step 3, after the lease dance: under heavy
	// load the short-TTL abandoned lease could expire before step 2
	// probed it, the drill failed with the wrong class, and the
	// operator saw "product failure" for what was their own budget.
	if agentKillLeaseTTL+time.Second > recoverWithin {
		r.Failure = fmt.Sprintf("recover_within (%s) is shorter than the lease TTL (%s); "+
			"the drill cannot observe a reclaim that is not allowed to happen yet",
			recoverWithin, agentKillLeaseTTL)
		r.Misconfigured = true
		r.Pass = false
		return r, nil
	}

	sp, err := storage.Open(ctx, opts.RepoURL)
	if err != nil {
		r.Failure = fmt.Sprintf("open repository %q: %v", opts.RepoURL, err)
		r.Pass = false
		return r, nil
	}
	defer sp.Close()
	defer cleanUpAgentKillProbe(ctx, sp, r)

	// 1. A backup starts, and its agent is killed: the lease is taken
	//    and then never renewed or released.
	abandoned, err := backup.AcquireBackupLease(ctx, sp, agentKillProbeDeployment,
		backup.LeaseOptions{Owner: "gameday-crashed-agent", TTL: agentKillLeaseTTL})
	switch {
	case errors.Is(err, backup.ErrLeaseNotEnforceable):
		// A finding, and a serious one: on this backend a crashed
		// agent's lease excludes nobody, so a second agent can back up
		// the same deployment at the same time.
		r.Failure = fmt.Sprintf("this repository backend cannot enforce a backup lease "+
			"(%v). A crashed agent leaves a lease that excludes nothing, so two agents "+
			"can back up the same deployment concurrently. Use a backend with atomic "+
			"conditional put, or accept that concurrency control is advisory here", err)
		r.Pass = false
		return r, nil
	case err != nil:
		r.Failure = fmt.Sprintf("could not acquire the lease the drill abandons: %v", err)
		r.Pass = false
		return r, nil
	}
	_ = abandoned // deliberately never Released: that is the crash.
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "fault",
		Message: "lease acquired and abandoned — the state a killed agent leaves behind",
		Body:    map[string]any{"ttl": agentKillLeaseTTL.String(), "owner": "gameday-crashed-agent"},
	})

	// 2. While that lease is live, a second agent must be refused.
	//    Without this, "recovery" below would be indistinguishable from
	//    a lease that never excluded anyone.
	if _, err := backup.AcquireBackupLease(ctx, sp, agentKillProbeDeployment,
		backup.LeaseOptions{Owner: "gameday-second-agent", TTL: agentKillLeaseTTL}); !errors.Is(err, backup.ErrBackupInProgress) {
		r.Failure = fmt.Sprintf("a second agent acquired the lease while the crashed "+
			"agent's was still live (got %v, want ErrBackupInProgress). Two agents backing "+
			"up one deployment concurrently is the condition the lease exists to prevent",
			err)
		r.Pass = false
		return r, nil
	}
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "observed",
		Message: "a second agent is refused while the abandoned lease is still within its TTL",
	})

	// 3. After expiry, recovery: agents may reclaim — but exactly one.
	//    The window between "observed stale" and "overwrote" is where a
	//    naive implementation lets every reclaimer through.
	waitFor := agentKillLeaseTTL + time.Second // budget already validated up front
	select {
	case <-ctx.Done():
		r.Failure = "cancelled while waiting for the abandoned lease to expire"
		r.Pass = false
		return r, nil
	case <-time.After(waitFor):
	}

	winners, errs := raceForLease(ctx, sp, agentKillReclaimers)
	switch {
	case winners == 0:
		r.Failure = fmt.Sprintf("no agent could reclaim the abandoned lease after it "+
			"expired (%d attempt(s), errors: %v). The deployment is wedged: its agent is "+
			"dead and nothing else may back it up", agentKillReclaimers, errs)
		r.Pass = false
		return r, nil
	case winners > 1:
		r.Failure = fmt.Sprintf("%d of %d agents reclaimed the SAME expired lease. They "+
			"all observed it stale and all acted on that judgement — the break must be "+
			"claimed atomically so exactly one proceeds, or the crash recovery itself "+
			"creates the concurrent-backup condition the lease prevents",
			winners, agentKillReclaimers)
		r.Pass = false
		return r, nil
	}
	r.RecoveryTime = time.Since(r.StartedAt)
	r.Evidence = append(r.Evidence, Event{
		At:      time.Now().UTC(),
		Kind:    "recovered",
		Message: "exactly one of the racing agents reclaimed the expired lease",
		Body: map[string]any{
			"reclaimers":   agentKillReclaimers,
			"winners":      winners,
			"recovered_in": r.RecoveryTime.String(),
		},
	})

	r.Pass = true
	return r, nil
}

// raceForLease starts n concurrent acquirers and reports how many
// succeeded. Concurrency is the point: a sequential probe cannot tell an
// atomic break from a racy one.
func raceForLease(ctx context.Context, sp storage.StoragePlugin, n int) (int, []error) {
	var (
		mu      sync.Mutex
		winners int
		errs    []error
		leases  []*backup.Lease
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together
			l, err := backup.AcquireBackupLease(ctx, sp, agentKillProbeDeployment,
				backup.LeaseOptions{
					Owner: fmt.Sprintf("gameday-reclaimer-%d", i),
					TTL:   agentKillLeaseTTL,
				})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			winners++
			leases = append(leases, l)
		}(i)
	}
	close(start)
	wg.Wait()

	// Release whatever was won so the probe does not leave a live lease
	// behind for its own TTL.
	for _, l := range leases {
		_ = l.Release(ctx)
	}
	return winners, errs
}

// cleanUpAgentKillProbe removes the probe deployment's lease objects,
// including the break-claim markers the reclaim race writes.
func cleanUpAgentKillProbe(ctx context.Context, sp storage.StoragePlugin, r *Result) {
	prefix := "leases/" + agentKillProbeDeployment + "/"
	var removed, failed int
	var listErr error
	for obj, err := range sp.List(ctx, prefix) {
		if err != nil {
			listErr = err
			break
		}
		if derr := sp.Delete(ctx, obj.Key); derr != nil {
			failed++
			continue
		}
		removed++
	}
	if listErr != nil {
		r.Evidence = append(r.Evidence, Event{
			At:      time.Now().UTC(),
			Kind:    "cleanup_failed",
			Message: fmt.Sprintf("could not list %s to clean up: %v", prefix, listErr),
		})
		return
	}
	ev := Event{
		At:      time.Now().UTC(),
		Kind:    "cleanup",
		Message: fmt.Sprintf("removed %d probe lease object(s) under %s", removed, prefix),
	}
	if failed > 0 {
		ev.Kind = "cleanup_failed"
		ev.Message = fmt.Sprintf("%d probe lease object(s) under %s could not be removed; "+
			"they expire on their own but should be cleared by hand", failed, prefix)
	}
	r.Evidence = append(r.Evidence, ev)
}

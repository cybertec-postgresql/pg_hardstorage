# R7 — Patroni split-brain

Two nodes both claim to be primary. Patroni's REST endpoint
disagrees with the connected PG instance about the leader, or two
Patroni instances both report `role=primary`. The agent's pre-flight
refuses to write WAL or take backups in this state because it
cannot determine which timeline is canonical.

## Symptoms

- `wal stream` and `backup` exit 4 with
  `preflight.patroni_split_brain`.
- `pg_hardstorage doctor` flags `Patroni topology` as critical.
- `curl http://patroni-a:8008/cluster` and `curl
  http://patroni-b:8008/cluster` show conflicting `members[].role`
  fields.
- Application logs show writes succeeding against both nodes.

## Pre-flight

This is the only scenario where the runbook starts with **stop the
agent, stop application traffic, and call your DBA team**. The
recovery path is operator-only because resolving split-brain
involves choosing a winner and discarding the loser's writes.

Confirm split-brain (rather than slow Patroni state propagation):

```sh
for h in node-a node-b node-c; do
  echo "=== $h ===" ; curl -s http://$h:8008/cluster
done
psql -h node-a -tAc "SELECT pg_is_in_recovery(), pg_current_wal_lsn();"
psql -h node-b -tAc "SELECT pg_is_in_recovery(), pg_current_wal_lsn();"
```

If two nodes both return `pg_is_in_recovery()=false`, you have
split-brain. If one is `true`, Patroni is just slow to propagate;
wait a few cycles and re-check.

## Procedure

1. **Pause `pg_hardstorage`** so it doesn't write any chunks
   tagged with the wrong timeline:

   ```sh
   systemctl stop pg_hardstorage
   ```

2. **Pause application writes.** Whatever your traffic-control
   mechanism is — load balancer, PgBouncer admin, application
   feature flag — drop writes until the cluster is reconciled.

3. **Pick the winner.** Engineering judgment, not a tool decision.
   Typically the node with the higher `pg_current_wal_lsn()` and
   the most recent application-acknowledged commits. Capture both
   nodes' state for forensics:

   ```sh
   for h in node-a node-b; do
     pg_dumpall -h $h --schema-only > /tmp/$h.schema.sql
     psql -h $h -tAc "SELECT pg_current_wal_lsn(), pg_walfile_name(pg_current_wal_lsn());"
   done
   ```

4. **Stop the loser.** Whatever you don't pick:

   ```sh
   ssh <loser-host> 'pg_ctl stop -D <pgdata> -m fast'
   ```

   Mark the host as offline in Patroni:

   ```sh
   patronictl pause --wait
   patronictl remove <cluster-name>
   ```

5. **Reset Patroni state.** Patroni's DCS keys (etcd, Consul,
   Zookeeper) hold the leader lock. Clear them so the surviving
   node's Patroni can re-establish leadership cleanly:

   ```sh
   patronictl reinit <cluster-name> <loser-host>     # re-bootstraps from winner
   patronictl resume
   ```

   The `reinit` rebuilds the loser's data dir from the winner via
   `pg_basebackup` or `pg_rewind`.

6. **Restart `pg_hardstorage`.** With Patroni back to one leader
   the pre-flight refusal clears:

   ```sh
   systemctl start pg_hardstorage
   pg_hardstorage doctor <deployment>
   ```

7. **Take a fresh backup.** The split-brain window may have left
   inconsistent WAL on either side; a fresh backup serves as the
   safe restore floor:

   ```sh
   pg_hardstorage backup <deployment>
   ```

## Verification

- `pg_hardstorage doctor` is clean across the cluster.
- `patronictl list <cluster-name>` shows exactly one leader, the
  rest replicas streaming.
- `pg_hardstorage status <deployment>` shows the new leader as the
  endpoint, WAL lag near zero.
- A test restore against the post-incident backup passes the
  verify gate.

## Rollback

There is no rollback for the loser's discarded writes. That is the
nature of split-brain resolution — one node's transactions lose. If
those transactions are recoverable from application logs, replay
them via the application; if not, this is a data-loss event and
must be reported per organisational policy.

## Post-incident

- Append an audit event with the LSNs of both candidates and the
  decision.
- File a Patroni-side incident: split-brain implies the DCS
  lost coherence (etcd quorum loss, Zookeeper partition,
  `loop_wait` misconfigured, fencing absent).
- Review fencing setup. STONITH-style power-fencing or
  network-fencing prevents two-leader scenarios; Patroni's
  software-only fencing relies on cooperative shutdown.
- Re-enable application traffic only after at least one fresh
  backup has succeeded and the verify gate has passed.
- Plan a game-day exercise (`pg_hardstorage gameday run
  --scenario patroni_split_brain` in v0.5+) to confirm the
  recovery procedure works under realistic conditions.

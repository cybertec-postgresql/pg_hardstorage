# Operator runbook: control-plane setup

The pg_hardstorage control plane is the optional fleet-orchestration
layer. Single-host deployments don't need it — `pg_hardstorage agent`
runs the local schedule engine and writes manifests directly. The
control plane becomes useful when:

- You have **2+ agent hosts** sharing a repo and want fleet-wide
  visibility.
- You want to **dispatch ad-hoc backups** from a central place
  instead of SSH-ing to each host.
- You need a **read-only API** for monitoring tooling (Prometheus,
  Grafana) to query deployment + backup state without each scrape
  reaching every agent.
- Your fleet sits behind **mTLS** and you want one auth-policy
  surface instead of per-host configuration.

Current limitations (still on the roadmap):

- gRPC. The control plane ships REST-only. The same handlers are
  planned over gRPC; the proto schema isn't stable yet.
- OIDC + per-verb RBAC. Auth is single-token. Use mTLS for fleets
  that need richer auth today.

Already shipped and covered below:

- Persistent dispatch state is optional. The default JobRegistry
  is in-memory (`--coord-backend memory`); a control-plane restart
  then loses queued + in-flight jobs. Pass `--coord-backend pg`
  with `--coord-dsn` for a persistent, multi-instance-HA
  PostgreSQL-backed registry (used throughout this runbook).

---

## Prerequisites

- `pg_hardstorage` v0.4+ installed on every agent host and on the
  control-plane host.
- A repository (`file://`, `s3://`, ...) reachable from both the
  control plane and each agent. If the control plane and agents
  see different repo URLs (e.g. agents use a private S3 endpoint,
  control plane the public one), each agent's local config wins —
  dispatch refuses cross-repo writes loudly to prevent the wrong
  bucket from receiving data.
- Network reachability:
  - **Agent → Control plane**: TCP to the listener port (default
    `8443`). Heartbeats and job claim/progress/complete posts ride
    here.
  - **Control plane → Agent**: not required in v0.4. Dispatch is
    pull-based (agents poll); push-based dispatch lands in v0.5.

---

## Step 1 — Provision the bearer token

```sh
# On the control-plane host:
sudo install -d -m 0700 -o pgbackup -g pgbackup /etc/pg_hardstorage/server
openssl rand -hex 32 | sudo tee /etc/pg_hardstorage/server/token >/dev/null
sudo chown pgbackup:pgbackup /etc/pg_hardstorage/server/token
sudo chmod 0600 /etc/pg_hardstorage/server/token
```

Copy the token bytes to a secret store (HashiCorp Vault, AWS
Secrets Manager, sealed K8s Secret) and distribute the same value
to every agent. The control plane reads the token from a file; the
agent reads it from a file too.

**Token rotation.** The control plane reads the token at startup.
To rotate: write the new token, restart the control plane, then
restart agents one at a time so they pick up the new value. v0.5's
multi-token support removes the restart requirement.

---

## Step 2 — Provision TLS certificates

The minimum-viable production posture is TLS (clients verify the
server). Full mTLS (server also verifies the client) is recommended
for production.

### Self-signed cert (lab / proof-of-concept)

```sh
# Server cert + key (single-host self-signed):
openssl req -x509 -newkey rsa:4096 -nodes \
    -days 365 \
    -subj "/CN=control.pg-hardstorage.local" \
    -addext "subjectAltName=DNS:control.pg-hardstorage.local,IP:10.0.0.10" \
    -keyout /etc/pg_hardstorage/server/key.pem \
    -out /etc/pg_hardstorage/server/cert.pem

sudo chown pgbackup:pgbackup /etc/pg_hardstorage/server/key.pem \
                              /etc/pg_hardstorage/server/cert.pem
sudo chmod 0600 /etc/pg_hardstorage/server/key.pem
```

### Production: cert from a real CA

Use whatever your environment already issues — public CA, private
CA, cert-manager + ACME, smallstep CA. The control plane reads a
PEM-encoded cert + key pair via `--tls-cert` and `--tls-key`.

### mTLS: client CA bundle

For mTLS, also provide a CA bundle the control plane uses to verify
client certs:

```sh
# Aggregate the CAs that signed your agents' certs:
cat /etc/pki/agent-ca-1.pem /etc/pki/agent-ca-2.pem | \
    sudo tee /etc/pg_hardstorage/server/client-ca.pem >/dev/null
```

When `--client-ca` is set, the control plane refuses any client
that doesn't present a cert signed by one of these CAs.

---

## Step 3 — Start the control plane

### One-shot (foreground)

```sh
sudo -u pgbackup pg_hardstorage server \
    --listen 0.0.0.0:8443 \
    --tls-cert /etc/pg_hardstorage/server/cert.pem \
    --tls-key  /etc/pg_hardstorage/server/key.pem \
    --client-ca /etc/pg_hardstorage/server/client-ca.pem \
    --token-file /etc/pg_hardstorage/server/token \
    --repo s3://acme-pg-backups/
```

You should see:

```
INFO  control plane listening on 0.0.0.0:8443 (TLS=true, mTLS=true, token=true)
```

### systemd

A reasonable unit file:

```ini
[Unit]
Description=pg_hardstorage control plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=pgbackup
Group=pgbackup
ExecStart=/usr/bin/pg_hardstorage server \
    --listen 0.0.0.0:8443 \
    --tls-cert /etc/pg_hardstorage/server/cert.pem \
    --tls-key /etc/pg_hardstorage/server/key.pem \
    --client-ca /etc/pg_hardstorage/server/client-ca.pem \
    --token-file /etc/pg_hardstorage/server/token \
    --repo s3://acme-pg-backups/
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Drop at `/etc/systemd/system/pg_hardstorage-server.service`,
`systemctl daemon-reload && systemctl enable --now
pg_hardstorage-server`.

### Verify the listener is up

```sh
# From any host that can reach the control plane:
curl --cacert /etc/pg_hardstorage/server/cert.pem \
     https://control.pg-hardstorage.local:8443/v1/healthz

# Expected:
# {
#   "schema": "pg_hardstorage.server.v1",
#   "generated_at": "...",
#   "result": { "status": "ok" }
# }
```

`/v1/healthz` is unauthenticated by design (k8s liveness probes
need it); it does not expose any state.

### Bounding concurrent jobs (large fleets)

By default the control plane hands out as many jobs as agents
claim. On a large fleet a burst of queued work can run an
unbounded number of concurrent backups and storm storage or the
source databases. Cap it with `--max-concurrent-jobs`:

```sh
pg_hardstorage server … --max-concurrent-jobs 200
```

Once the cap is reached, claims are refused and queued work waits;
agents keep polling and pick it up as running jobs finish. `0`
(the default) is unlimited. For multi-control-plane HA
(`--coord-backend pg`) set the *same* value on every instance — the
cap is then enforced globally over the shared jobs table.

Agents also jitter their heartbeat/poll intervals automatically, so
a fleet started together doesn't hit the control plane in
synchronized bursts — no configuration needed.

See [Scaling to large fleets](../../operations/scaling-large-fleets.md)
for sizing guidance and the rest of the fleet-scale behaviour.

---

## Step 4 — Wire each agent

On every agent host:

```sh
# Drop the same bearer token (provisioned in Step 1):
sudo install -d -m 0700 -o pgbackup -g pgbackup /etc/pg_hardstorage/agent
sudo install -m 0600 -o pgbackup -g pgbackup ./token \
    /etc/pg_hardstorage/agent/control-plane.token

# Start the agent in control-plane mode:
sudo -u pgbackup pg_hardstorage agent \
    --control-plane https://control.pg-hardstorage.local:8443 \
    --control-plane-token-file /etc/pg_hardstorage/agent/control-plane.token \
    --agent-id db1.example.com
```

Within 10 seconds the agent's first heartbeat should land. Verify
on the control-plane host:

```sh
curl --cacert /etc/pg_hardstorage/server/cert.pem \
     -H "Authorization: Bearer $(cat /etc/pg_hardstorage/server/token)" \
     https://control.pg-hardstorage.local:8443/v1/agents

# Expected:
# {
#   "result": {
#     "agents": [
#       {
#         "id": "db1.example.com",
#         "host": "db1.example.com",
#         "version": "v1.0.15",
#         "deployments": ["db1", "db2"],
#         "registered_at": "...",
#         "last_heartbeat": "..."
#       }
#     ],
#     "heartbeat_timeout": "30s"
#   }
# }
```

If `agents` is empty after 30 seconds, see Troubleshooting below.

---

## Step 5 — Dispatch a backup

```sh
# Enqueue a backup of `db1`. The control plane queues the job; the
# agent claims it on the next poll (≤5s).
curl -X POST \
     --cacert /etc/pg_hardstorage/server/cert.pem \
     -H "Authorization: Bearer $(cat /etc/pg_hardstorage/server/token)" \
     -H "Content-Type: application/json" \
     -d '{}' \
     https://control.pg-hardstorage.local:8443/v1/deployments/db1/backups
```

The response carries the new Job's ID. Track it:

```sh
JOB_ID=<id-from-above>
curl --cacert /etc/pg_hardstorage/server/cert.pem \
     -H "Authorization: Bearer $(cat /etc/pg_hardstorage/server/token)" \
     https://control.pg-hardstorage.local:8443/v1/jobs/$JOB_ID
```

State transitions:

```
queued      — created by your POST, no agent yet
running     — an agent claimed it; progress events appended
completed   — agent finished successfully
failed      — agent reported failure (see .failure for the message)
```

Job timeout: a job stuck in `running` past `claimDeadline` (default
6h) is automatically transitioned to `failed` with
`Failure="abandoned: agent stopped reporting"`. The deadline is
configurable per registry — wire it via the v0.5 server-config
field once that lands. v0.4 uses the 6h default for everything.

---

## Step 6 — Production hardening

### Egress restrictions

The agent only needs egress to the control plane and to the repo.
Lock down everything else:

```
# Example UFW rules on the agent host:
sudo ufw default deny outgoing
sudo ufw allow out to <control-plane-ip> port 8443 proto tcp
sudo ufw allow out to <s3-endpoint> port 443 proto tcp
sudo ufw allow out to <pg-backup-host> port 5432 proto tcp
```

### Secrets posture

- The bearer token is a bearer credential — don't commit it,
  don't put it in env vars visible to other processes, don't
  log it.
- mTLS client keys deserve the same posture. Rotate annually
  minimum.
- The repository's encryption KEK lives separately on each agent
  (in `/etc/pg_hardstorage/keyring/`). The control plane never
  sees the KEK.

### Multi-AZ availability

With the in-memory backend the control plane is effectively
single-instance. For HA:

- Run two control-plane instances behind a load balancer; agents
  heartbeat the LB hostname. **Caveat (in-memory backend only):**
  with `--coord-backend memory`, job state isn't shared, so a job
  enqueued on instance A is invisible to instance B until A
  processes it. Use sticky sessions on the LB to mitigate, or
  queue outside the control plane (Kafka, Redis) and have the
  control plane be a thin REST adapter.
- The PG-backed JobRegistry (`--coord-backend pg`) removes this
  caveat: any control-plane instance can see + dispatch any job.
  Use it if you need multi-instance dispatch correctness.

### Observability

The control plane logs to stderr (systemd journal). Operator-
visible signals:

- `control plane listening on ...` at startup
- `jobs: sweeper reaped N abandoned job(s)` when the timeout
  sweeper fires
- HTTP error responses are structured JSON (`schema:
  pg_hardstorage.server.v1`, `error.code`, `error.message`)

For Prometheus scraping, point the scraper at `/v1/version` (cheap,
authenticated) for liveness, or at the dedicated `/metrics`
endpoint (unauthenticated, like the health probes) for
control-plane and job gauges.

---

## Troubleshooting

### Agent shows up as "active" then disappears

The agent's heartbeat ticker fires every 10s; the registry marks
agents inactive after missing two heartbeats (30s default). If an
agent flickers active/inactive:

- Check the agent's stderr for `controlplane: heartbeat:` lines.
- Verify the token matches between server and agent
  (`diff <(cat agent.token) <(cat server.token)` — should print
  nothing).
- Confirm clock skew between agent and control-plane is small.
  The heartbeat timestamp is server-side (control plane records
  `last_heartbeat = time.Now().UTC()` at receive time), so clock
  drift on the agent side doesn't matter — but a control plane
  whose clock jumps backwards will misjudge agent activity.

### Job stays `queued` forever

- No agent advertises the job's deployment. Check
  `/v1/agents` and confirm the deployment name appears in some
  agent's `.deployments` list.
- The agent's PollInterval (5s) hasn't elapsed yet. Wait 10s.
- The agent's claim is being refused. Check the agent's stderr
  for `controlplane: claim:` lines.

### Job goes straight to `failed` with "deployment not in local config"

The agent's local `pg_hardstorage.yaml` doesn't list the
deployment. The agent's deployments-list (in the heartbeat) is
derived from this config. If you renamed a deployment, restart the
agent so it re-reads the config and re-heartbeats.

### Job goes `failed` with "doesn't match agent-local repo"

The control plane dispatched a job whose `RepoURL` diverges from
what the agent has in its local config. This is the deliberate
guardrail against control-plane misconfiguration writing into the
wrong bucket. To fix:

- Confirm the control plane's `--repo` and the agent's
  `deployments.<name>.repo` are the same URL string.
- If you intentionally want the agent to write to a different
  repo than the one it has locally configured, that's an
  operator error — fix the local config first.

### Restore-time job shows `pre-condition not met`

Restore and verify jobs are dispatched today: `POST
/v1/deployments/<n>/restores` and `.../verifies` enqueue
`JobRestore` / `JobVerify`, which the agent's RestoreExecutor /
VerifyExecutor run. A `pre-condition not met` result means the
job's guardrails (e.g. repo-URL match, non-empty target) failed —
check the job result body. You can still run restores via
`pg_hardstorage restore` directly on the agent host.

### Control plane refuses agent client cert

mTLS is configured but the agent's cert isn't signed by a CA in
the bundle. Check:

```sh
openssl verify -CAfile /etc/pg_hardstorage/server/client-ca.pem \
               /path/to/agent-cert.pem
```

If `error 20 at 0 depth lookup:unable to get local issuer
certificate`, the agent's cert chain isn't covered. Add the missing
CA to the bundle and restart the control plane.

---

## Already shipped

- PostgreSQL-backed JobRegistry (`--coord-backend pg`) for
  multi-instance HA dispatch
- Dispatch of restore / verify kinds (`JobRestore` / `JobVerify`)
- Dedicated `/metrics` endpoint

## Still on the roadmap

- gRPC alongside REST (same handlers, proto schema)
- pg_timetable integration as the recommended scheduler
- OIDC + multi-token + per-verb RBAC
- Full progress streaming for restore / verify dispatch
- Job result persistence beyond the registry's in-memory window

This runbook is updated alongside each milestone.

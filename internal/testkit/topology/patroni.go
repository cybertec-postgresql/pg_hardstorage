// patroni.go — 3-node Patroni + etcd cluster on the local
// Docker daemon, driven via `docker compose`.
package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

// jsonUnmarshal is aliased so clusterShape can stay small.
var jsonUnmarshal = json.Unmarshal

// patroniLocalDocker brings up a 3-node Patroni cluster + etcd on
// the local Docker daemon via the `docker compose` CLI.  Used by
// scenario authors who need to exercise leader-election / failover
// invariants — the existing `local-docker` topology starts a
// single PG and has no failover surface at all.
//
// Bring-up shape:
//
//	etcd-3.5 (DCS)
//	  ↑ ETCD3_HOSTS, v3 protocol
//	pg-0  Spilo-17 (Zalando)
//	pg-1  Spilo-17
//	pg-2  Spilo-17
//
// Spilo bundles PG + Patroni; the env-var contract bootstraps a
// 3-node cluster against the etcd DCS, elects a primary, and
// streams replicas via pg_basebackup.  No persistent volumes —
// every Up brings up a fresh cluster, every Down nukes it.
//
// Each PG node gets two host-mapped ports: 5432 (PG) and 8008
// (Patroni REST).  The host ports are picked at Up time by
// listening on `:0` and reading the OS-assigned port — small
// race window between port discovery and compose-up actually
// claiming them, but an acceptable trade for not requiring the
// caller to pre-allocate.
//
// ConnString does leader discovery on every call by polling
// `/leader` on each node's :8008 (Patroni returns 200 only on
// the current primary).  After a `patroni_switchover` fault the
// next ConnString call picks up the new primary automatically —
// no separate Refresh API needed.
//
// Targets exposes every PG node as role "patroni" plus the etcd
// as role "etcd", so an `inject` step can dispatch with
// `target=patroni_random` (one of three) or `target=patroni` (all
// three) per the existing inject.TargetSet semantics.
type patroniLocalDocker struct {
	composeDir  string
	composeFile string
	projectName string

	// node[i] holds per-node host port mappings + container name.
	nodes []patroniNode
	// etcd container name + container ID, used for Targets().
	etcdContainer string

	mu sync.Mutex // guards lazy leader cache
}

type patroniNode struct {
	service     string // "pg-0" / "pg-1" / "pg-2"
	container   string // full docker container name after compose up
	pgPort      int    // host-mapped PG port
	patroniPort int    // host-mapped Patroni REST port
}

func newPatroniLocalDocker() *patroniLocalDocker { return &patroniLocalDocker{} }

// Name returns "patroni-local-docker".
func (p *patroniLocalDocker) Name() string { return "patroni-local-docker" }

const (
	// numPatroniNodes is the cluster size.  Three is the smallest
	// majority-quorum cluster and matches the conventional
	// Patroni deployment shape (a primary + 2 replicas).  Larger
	// clusters add coverage but multiply bring-up cost; soak runs
	// that need bigger clusters can fork this provider.
	numPatroniNodes = 3

	// patroniLeaderWaitTimeout caps how long Up blocks waiting
	// for a leader AND at least N-1 replicas to attach.  Spilo's
	// initial bring-up runs initdb on the leader, then each
	// replica streams pg_basebackup from it; on a cold-image
	// first run, full attachment is ~45-60 s.  120 s leaves
	// headroom for a slow CI runner without burying genuine
	// bring-up failures.  Returning earlier (when only the
	// leader is up) makes Up's contract misleading: a scenario
	// that immediately runs `patroni_switchover` would fail
	// because /cluster reports only the leader and there's no
	// replica to promote.
	patroniLeaderWaitTimeout = 120 * time.Second

	// spiloImage / etcdImage pin the upstream tags this provider
	// has been smoke-tested against.  Bumping either is a
	// Topology-package change, not a scenario-author concern.
	spiloImage = "ghcr.io/zalando/spilo-17:4.0-p1"
	etcdImage  = "quay.io/coreos/etcd:v3.5.13"
)

// Up writes the compose YAML, runs `docker compose up -d`, and
// blocks until the Patroni REST API on at least one node returns
// 200 from /leader (the leader-elected signal).  The `opts`
// parameter is currently advisory — Spilo image bundles its own
// PG version, and the Patroni cluster doesn't expose a
// filesystem-tier knob — but UpOptions is honoured for future
// compatibility (e.g. a Spilo image variant per PG version).
func (p *patroniLocalDocker) Up(ctx context.Context, opts UpOptions) error {
	dir, err := os.MkdirTemp("", "pg_hardstorage-patroni-")
	if err != nil {
		return fmt.Errorf("patroni: mkdir tempdir: %w", err)
	}
	p.composeDir = dir
	p.composeFile = filepath.Join(dir, "docker-compose.yaml")
	// Compose project name embeds the tempdir tail so multiple
	// Up calls can co-exist on the same daemon without clashing
	// on container names.
	p.projectName = "pg-hs-patroni-" + filepath.Base(dir)

	// Allocate one (PG, Patroni-REST) port pair per node.  We
	// bind to ":0" and read the OS-assigned port — the kernel
	// recycles those ports promptly after we close the listener,
	// so there's a small TOCTOU window before compose claims
	// them.  In practice the ports are quiet enough that the
	// soak test framework hasn't hit a collision; if we ever do,
	// the fix is to retry Up with fresh allocations.
	p.nodes = make([]patroniNode, numPatroniNodes)
	for i := range p.nodes {
		pg, err := allocateLocalPort()
		if err != nil {
			return fmt.Errorf("patroni: allocate PG port for node %d: %w", i, err)
		}
		rest, err := allocateLocalPort()
		if err != nil {
			return fmt.Errorf("patroni: allocate REST port for node %d: %w", i, err)
		}
		p.nodes[i] = patroniNode{
			service:     fmt.Sprintf("pg-%d", i),
			pgPort:      pg,
			patroniPort: rest,
		}
	}

	if err := p.writeCompose(); err != nil {
		_ = p.Down(context.Background())
		return err
	}

	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-f", p.composeFile, "up", "-d")
	combined, err := cmd.CombinedOutput()
	if err != nil {
		_ = p.Down(context.Background())
		return fmt.Errorf("patroni: docker compose up: %w (output: %s)",
			err, truncateForLog(combined, 512))
	}

	if err := p.resolveContainers(ctx); err != nil {
		_ = p.Down(context.Background())
		return err
	}

	if err := p.waitForLeader(ctx, patroniLeaderWaitTimeout); err != nil {
		// Capture compose logs into the artefact dir before we
		// tear down — the leader-wait timing-out is the single
		// most common bring-up failure and reading
		// /tmp/.../patroni-up.log is the operator's first move.
		_ = p.dumpLogs(filepath.Join(dir, "patroni-up.log"))
		_ = p.Down(context.Background())
		return err
	}
	return nil
}

// ConnString returns the libpq DSN for the CURRENT Patroni
// leader, polled on every call.  After a switchover the next
// call picks up the new leader without any explicit Refresh
// invocation.
//
// Returns the empty string when no leader can be reached — the
// caller's PingContext will then fail with a clear connection
// error.  We deliberately don't return an error from ConnString
// because the Topology contract makes the call total; rare
// transient failures during a switchover would otherwise force
// every caller into error-handling boilerplate.
func (p *patroniLocalDocker) ConnString() string {
	if len(p.nodes) == 0 {
		return ""
	}
	leader := p.findLeader(context.Background(), 5*time.Second)
	if leader == nil {
		return ""
	}
	// User/password/db pinned to the env-var contract written
	// into writeCompose.  sslmode=disable because the Spilo
	// image's default certs are self-signed test material —
	// scenarios can layer TLS on top via the agent's own
	// connection-string knobs if needed.
	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable",
		patroniSuperPassword, leader.pgPort)
}

// Targets surfaces the cluster's mutable surface to the inject
// fault registry.  Three patroni-role targets (one per PG node)
// plus one etcd-role target, all docker-backed so the existing
// inject.DockerTarget plumbing applies.
func (p *patroniLocalDocker) Targets() []inject.Target {
	if len(p.nodes) == 0 {
		return nil
	}
	out := make([]inject.Target, 0, len(p.nodes)+1)
	for _, n := range p.nodes {
		out = append(out, &inject.DockerTarget{
			Container: n.container,
			RoleStr:   "patroni",
		})
	}
	if p.etcdContainer != "" {
		out = append(out, &inject.DockerTarget{
			Container: p.etcdContainer,
			RoleStr:   "etcd",
		})
	}
	return out
}

// Down tears down the compose stack and removes the temp dir.
// Idempotent: safe to call after a partial bring-up failure.
func (p *patroniLocalDocker) Down(ctx context.Context) error {
	if p.composeFile != "" {
		cmd := exec.CommandContext(ctx, "docker", "compose",
			"-f", p.composeFile, "down", "-v")
		// Best-effort — Down should never block on a
		// daemon-side issue; the operator can clean up
		// orphans manually if compose down fails.
		_ = cmd.Run()
	}
	if p.composeDir != "" {
		_ = os.RemoveAll(p.composeDir)
	}
	p.composeDir = ""
	p.composeFile = ""
	p.nodes = nil
	p.etcdContainer = ""
	return nil
}

// patroniSuperPassword is the cluster superuser password baked
// into the compose file.  Test-only — Spilo refuses to bring up
// without one set, and we don't expose the cluster outside the
// scenario runner.
const patroniSuperPassword = "testkit"

// writeCompose materialises the embedded compose YAML at
// p.composeFile.  Pinning service names + port mappings here
// keeps resolveContainers / waitForLeader logic simple — they
// read from p.nodes rather than parse YAML.
func (p *patroniLocalDocker) writeCompose() error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by pg_hardstorage_testkit topology=patroni-local-docker.\n")
	fmt.Fprintf(&b, "# Project: %s | Nodes: %d\n", p.projectName, len(p.nodes))
	fmt.Fprintf(&b, "name: %s\n\n", p.projectName)
	fmt.Fprintln(&b, "services:")

	// etcd — single-node Raft is enough for a 3-node Patroni
	// cluster's DCS needs in a test fixture.
	fmt.Fprintln(&b, "  etcd:")
	fmt.Fprintf(&b, "    image: %s\n", etcdImage)
	fmt.Fprintln(&b, "    command:")
	fmt.Fprintln(&b, "      - etcd")
	fmt.Fprintln(&b, "      - --name=etcd0")
	fmt.Fprintln(&b, "      - --data-dir=/data")
	fmt.Fprintln(&b, "      - --listen-client-urls=http://0.0.0.0:2379")
	fmt.Fprintln(&b, "      - --advertise-client-urls=http://etcd:2379")
	fmt.Fprintln(&b, "      - --listen-peer-urls=http://0.0.0.0:2380")
	fmt.Fprintln(&b, "      - --initial-advertise-peer-urls=http://etcd:2380")
	fmt.Fprintln(&b, "      - --initial-cluster=etcd0=http://etcd:2380")
	fmt.Fprintf(&b, "      - --initial-cluster-token=%s\n", p.projectName)
	fmt.Fprintln(&b, "      - --initial-cluster-state=new")
	fmt.Fprintln(&b, "")

	for _, n := range p.nodes {
		fmt.Fprintf(&b, "  %s:\n", n.service)
		fmt.Fprintf(&b, "    image: %s\n", spiloImage)
		fmt.Fprintln(&b, "    depends_on:")
		fmt.Fprintln(&b, "      - etcd")
		fmt.Fprintln(&b, "    environment:")
		fmt.Fprintf(&b, "      SCOPE: %s\n", "testkit")
		fmt.Fprintln(&b, `      PGVERSION: "17"`)
		// ETCD3_HOSTS uses the v3 protocol path — etcd-3.5
		// dropped v2 server support, so the older ETCD_HOSTS
		// form (which Spilo defaults to) hangs in
		// "waiting on etcd" forever.
		fmt.Fprintln(&b, `      ETCD3_HOSTS: '"etcd:2379"'`)
		fmt.Fprintln(&b, "      ETCD3_PROTOCOL: http")
		fmt.Fprintln(&b, "      PGUSER_SUPERUSER: postgres")
		fmt.Fprintf(&b, "      PGPASSWORD_SUPERUSER: %s\n", patroniSuperPassword)
		fmt.Fprintln(&b, "      PGUSER_ADMIN: postgres")
		fmt.Fprintf(&b, "      PGPASSWORD_ADMIN: %s\n", patroniSuperPassword)
		fmt.Fprintln(&b, "      PGUSER_STANDBY: standby")
		fmt.Fprintf(&b, "      PGPASSWORD_STANDBY: %s\n", patroniSuperPassword)
		// Spilo's default pg_hba only ships hostssl entries —
		// connections from the host ("sslmode=disable" via the
		// docker port mapping) are rejected with
		//   FATAL: pg_hba.conf rejects connection for host ... no encryption
		// ALLOW_NOSSL=1 tells Spilo to add hostnossl lines too.
		// We're inside a test container with self-signed certs;
		// the scenario doesn't audit TLS posture.
		fmt.Fprintln(&b, `      ALLOW_NOSSL: "1"`)
		fmt.Fprintln(&b, "    ports:")
		fmt.Fprintf(&b, "      - %q\n", fmt.Sprintf("127.0.0.1:%d:5432", n.pgPort))
		fmt.Fprintf(&b, "      - %q\n", fmt.Sprintf("127.0.0.1:%d:8008", n.patroniPort))
		fmt.Fprintln(&b, "")
	}

	return os.WriteFile(p.composeFile, []byte(b.String()), 0o644)
}

// resolveContainers populates each node's `container` field with
// the actual docker container name compose assigned.  Used by
// Targets() so DockerTarget can `docker exec` the right
// container.  Compose's name format is
// `<project>-<service>-1` for non-replicated services.
func (p *patroniLocalDocker) resolveContainers(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-f", p.composeFile, "ps", "--format", "{{.Service}} {{.Name}}")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("patroni: docker compose ps: %w", err)
	}
	byService := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			byService[fields[0]] = fields[1]
		}
	}
	if etcdName, ok := byService["etcd"]; ok {
		p.etcdContainer = etcdName
	}
	for i, n := range p.nodes {
		name, ok := byService[n.service]
		if !ok {
			return fmt.Errorf("patroni: docker compose ps did not list service %q (output: %q)",
				n.service, string(out))
		}
		p.nodes[i].container = name
	}
	return nil
}

// waitForLeader polls until the cluster has both an elected
// leader AND at least N-1 attached replicas, or the deadline
// elapses.  Returning earlier (when only the leader is up)
// makes Up's contract misleading — see the patroniLeaderWaitTimeout
// doc.
func (p *patroniLocalDocker) waitForLeader(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wantReplicas := len(p.nodes) - 1
	for {
		if leader, replicaCount := p.clusterShape(ctx, 2*time.Second); leader != nil && replicaCount >= wantReplicas {
			return nil
		}
		if time.Now().After(deadline) {
			leader, replicaCount := p.clusterShape(ctx, 2*time.Second)
			leaderName := "<none>"
			if leader != nil {
				leaderName = leader.service
			}
			return fmt.Errorf("patroni: cluster not ready within %s (leader=%s, attached_replicas=%d, want >=%d, project=%s)",
				timeout, leaderName, replicaCount, wantReplicas, p.projectName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// clusterShape returns (leader-node, attached-replica-count)
// from any reachable node's /cluster endpoint.  An attached
// replica is one that has finished pg_basebackup and is
// streaming (or running) — eligible as a /failover candidate.
func (p *patroniLocalDocker) clusterShape(ctx context.Context, perCallTimeout time.Duration) (*patroniNode, int) {
	p.mu.Lock()
	nodes := append([]patroniNode(nil), p.nodes...)
	p.mu.Unlock()
	client := &http.Client{Timeout: perCallTimeout}
	for i := range nodes {
		url := fmt.Sprintf("http://127.0.0.1:%d/cluster", nodes[i].patroniPort)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		// Same shape pickPatroniFailoverPair parses, kept
		// inlined here to avoid an import-cycle with inject.
		var cluster struct {
			Members []struct {
				Name  string `json:"name"`
				Role  string `json:"role"`
				State string `json:"state"`
			} `json:"members"`
		}
		if err := jsonUnmarshal(body, &cluster); err != nil {
			continue
		}
		var leaderNode *patroniNode
		replicaCount := 0
		for _, m := range cluster.Members {
			if m.Role == "leader" && m.State == "running" {
				// Map back to a patroniNode by polling
				// each node's /leader endpoint to find
				// the one returning 200.  Patroni's
				// member.name is the container hostname,
				// not our service name, so we can't match
				// by name — /leader's HTTP-200 contract
				// is the canonical leader signal.
				leaderNode = p.findLeader(ctx, perCallTimeout)
				continue
			}
			if m.Role == "replica" && (m.State == "streaming" || m.State == "running") {
				replicaCount++
			}
		}
		return leaderNode, replicaCount
	}
	return nil, 0
}

// findLeader returns the patroniNode currently holding leader
// status, or nil if none can be reached within perCallTimeout.
// Patroni's contract: GET /leader returns 200 only on the
// elected primary; replicas / unjoined nodes return 503.
func (p *patroniLocalDocker) findLeader(ctx context.Context, perCallTimeout time.Duration) *patroniNode {
	p.mu.Lock()
	nodes := append([]patroniNode(nil), p.nodes...)
	p.mu.Unlock()
	client := &http.Client{Timeout: perCallTimeout}
	for i := range nodes {
		url := fmt.Sprintf("http://127.0.0.1:%d/leader", nodes[i].patroniPort)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return &nodes[i]
		}
	}
	return nil
}

// dumpLogs writes `docker compose logs` into a path under the
// scenario's artefact dir for post-mortem.
func (p *patroniLocalDocker) dumpLogs(path string) error {
	cmd := exec.Command("docker", "compose", "-f", p.composeFile, "logs", "--no-color", "--tail", "200")
	out, err := cmd.CombinedOutput()
	_ = os.WriteFile(path, out, 0o644)
	return err
}

// allocateLocalPort grabs an OS-assigned free port on 127.0.0.1
// by binding ":0" and reading what the kernel handed back.  The
// listener is closed immediately; there's a small race window
// before compose claims the port.  Used in test infra only —
// production-port-allocation belongs in the soak driver's
// existing compose/ports.go which is fleet-deterministic.
func allocateLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr := l.Addr().(*net.TCPAddr)
	port := addr.Port
	if cerr := l.Close(); cerr != nil {
		return 0, cerr
	}
	return port, nil
}

// truncateForLog mirrors the testkit's other 256-ish-byte
// truncations on diagnostic strings — kept private to topology
// to avoid an import cycle with the testkit/runner truncate.
func truncateForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// _ ensures strconv stays imported — used by ad-hoc
// log-formatting if/when this provider grows port-summary
// helpers.
var _ = strconv.Itoa

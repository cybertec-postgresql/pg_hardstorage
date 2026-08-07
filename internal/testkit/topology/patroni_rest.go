// patroni_rest.go — hand the cluster's Patroni REST endpoints to
// callers that want to drive it through the product's own client.
//
// Existing tests reach Patroni by shelling out to `patronictl` inside a
// container. That is fine for arranging cluster state, but it tests
// patronictl, not us. The game-day scenario drives Patroni through
// internal/patroni — the same client the agent uses — so an integration
// test needs the REST base URLs the compose stack published on the
// host.

package topology

import "fmt"

// PatroniCluster is implemented by topologies that run a real Patroni
// cluster. Optional: callers type-assert and skip when a topology does
// not provide one.
type PatroniCluster interface {
	// PatroniRESTURLs returns one host-reachable base URL per node,
	// e.g. "http://127.0.0.1:32773". Empty before Up.
	PatroniRESTURLs() []string

	// NodeDSNs returns one libpq DSN per node, each pinned to that
	// node specifically. Empty before Up.
	//
	// ConnString() deliberately re-resolves the leader on every call,
	// which is what most tests want. This is for the tests that must
	// NOT follow the leader: pinning a client to one node is how you
	// reproduce an operator whose --pg-connection names a single host
	// rather than a leader-aware DSN, and therefore how you find out
	// what happens when that host is demoted.
	NodeDSNs() []string
}

// NodeDSNs implements PatroniCluster.
func (p *patroniLocalDocker) NodeDSNs() []string {
	out := make([]string, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, fmt.Sprintf(
			"postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable",
			patroniSuperPassword, n.pgPort))
	}
	return out
}

// PatroniRESTURLs implements PatroniCluster.
func (p *patroniLocalDocker) PatroniRESTURLs() []string {
	out := make([]string, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, fmt.Sprintf("http://127.0.0.1:%d", n.patroniPort))
	}
	return out
}

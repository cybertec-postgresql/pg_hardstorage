// ports.go — AllocatePorts: deterministic host-port allocation for compose fleets.
package compose

import (
	"fmt"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// PortMap is the result of AllocatePorts.  Keyed on the
// container name compose produces; the value is the host
// port the compose stack maps to that container's 5432.
//
// For Patroni cells (one cell × N nodes) every node gets its
// own entry: {"<cell>-c0-node0": port, "<cell>-c0-node1": port+1, ...}
type PortMap map[string]int

// AllocatePorts walks the fleet in the same order compose
// generate uses and returns the deterministic host-port
// allocation.  Both compose generate and validate call this so
// the soak driver knows which port hosts which cell without
// parsing the docker-compose YAML back.
//
// The walk order matches compose's: entries sorted by name,
// then unit (count), then node (Patroni nodesPerUnit).
func AllocatePorts(fleet *config.Fleet, base int) PortMap {
	if base == 0 {
		base = 15432
	}
	out := PortMap{}
	port := base
	sorted := append([]config.FleetEntry{}, fleet.Entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, e := range sorted {
		units := e.Count
		nodesPerUnit := 1
		if e.EffectiveRole() == "patroni-cluster" {
			nodesPerUnit = e.Nodes
		}
		for unit := 0; unit < units; unit++ {
			for node := 0; node < nodesPerUnit; node++ {
				name := containerName(e, unit, node, nodesPerUnit)
				out[name] = port
				port++
			}
		}
	}
	return out
}

// FirstContainer returns the canonical "lead" container for a
// cell — for non-Patroni this is the only container of the
// first unit; for Patroni it's node0 of unit0.  This is what
// the soak driver targets when "the cell" needs one entry
// point (DSN, agent dispatch, etc.).
//
// The returned name is the *unprefixed* service-map key (used
// in compose's `services:` map and in port allocation).  Wrap
// it in PrefixedContainerName when you need the global Docker
// container_name for `docker exec` / `docker kill`.
func FirstContainer(e config.FleetEntry) string {
	units := e.Count
	if units < 1 {
		units = 1
	}
	nodesPerUnit := 1
	if e.EffectiveRole() == "patroni-cluster" {
		nodesPerUnit = e.Nodes
	}
	return containerName(e, 0, 0, nodesPerUnit)
}

// PrefixedContainerName returns the global docker container_name
// for a service: "<project>-<service>".  Centralised here so
// the compose generator and every consumer (DockerCellRuntime,
// inject targets, ad-hoc `docker exec` callers) agree on the
// exact same string.
//
// An empty project collapses to just the service name — the
// behaviour previous releases shipped, kept to ease testing
// and to keep one-off invocations readable.
func PrefixedContainerName(project, service string) string {
	if project == "" {
		return service
	}
	return project + "-" + service
}

// PortFor returns the host port for the lead container of e.
// Errors if e isn't in the supplied map (caller bug).
func PortFor(m PortMap, e config.FleetEntry) (int, error) {
	name := FirstContainer(e)
	port, ok := m[name]
	if !ok {
		return 0, fmt.Errorf("compose: no port allocated for cell %q (lead container %q)",
			e.Name, name)
	}
	return port, nil
}

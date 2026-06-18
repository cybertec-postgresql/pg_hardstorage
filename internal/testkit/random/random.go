// Package random produces a diversity-biased random fleet from
// the testkit catalog.
//
// Inputs: catalog + count + seed.  Output: a config.Fleet ready
// to write to disk.  Picks bias toward variety along three
// axes: distinct OS family, distinct PG major, and (where
// supported) at least one Patroni cluster + at least one
// arm64 cell when the count is high enough to afford it.
//
// The "random" is seed-driven so a soak run with seed=42 always
// produces the same fleet; reproducibility is the contract.
package random

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// Options configures the picker.
type Options struct {
	// Count is the number of cells to emit.
	Count int

	// Seed drives the deterministic random.  Same seed →
	// same fleet for the same catalog.
	Seed int64

	// PreferPatroni: when count ≥ 5, force at least one
	// Patroni cluster cell.  Off by default so small fleets
	// stay simple.
	PreferPatroni bool

	// PreferArm64: when count ≥ 4 and the catalog has arm64
	// cells, force at least one arm64 cell.
	PreferArm64 bool

	// FilesystemPool restricts the random FS pick (default
	// catalog.Filesystems).  Operators with a Linux laptop
	// that doesn't have ZFS / Btrfs kernel modules pin to
	// {ext4, xfs} via this.
	FilesystemPool []string

	// ArchPool restricts the random architecture pick.  Empty
	// = use every arch the catalog supports (catalog-driven
	// matrix coverage).  Most operators want to pin to their
	// host arch (no cross-arch buildx setup needed); the CLI
	// `fleet random --arch amd64` flag passes one element here.
	ArchPool []string
}

// Pick returns a Fleet of size opts.Count, biased for diversity
// per the Options.  The catalog is the source-of-truth for
// allowed (OS, PG, arch) combinations; the picker never emits a
// cell the catalog doesn't validate.
func Pick(c *catalog.Catalog, opts Options) (*config.Fleet, error) {
	if opts.Count < 1 {
		return nil, fmt.Errorf("random: count must be ≥1 (got %d)", opts.Count)
	}
	if c == nil || len(c.OSes) == 0 {
		return nil, fmt.Errorf("random: catalog is empty")
	}
	rng := rand.New(rand.NewSource(opts.Seed))

	// Expand the catalog into the set of valid leaves
	// (os, pg, arch), then walk in a diversity-aware order.
	leaves := expandLeavesFiltered(c, opts.ArchPool)
	if len(leaves) == 0 {
		if len(opts.ArchPool) > 0 {
			return nil, fmt.Errorf("random: catalog has no cells matching arch %v", opts.ArchPool)
		}
		return nil, fmt.Errorf("random: catalog has no leaves")
	}

	// Shuffle for deterministic-yet-varied ordering.
	rng.Shuffle(len(leaves), func(i, j int) { leaves[i], leaves[j] = leaves[j], leaves[i] })

	picker := newDiversityPicker(leaves)
	out := &config.Fleet{Schema: config.FleetSchema, Version: 1}

	fsPool := opts.FilesystemPool
	if len(fsPool) == 0 {
		fsPool = c.Filesystems
	}
	if len(fsPool) == 0 {
		fsPool = []string{"ext4"}
	}

	used := map[string]bool{} // names already taken — keeps generated YAML readable
	for i := 0; i < opts.Count; i++ {
		leaf := picker.next(rng)
		if leaf == nil {
			break // catalog exhausted; rare for count ≤ ~20
		}
		entry := config.FleetEntry{
			Name:       uniqueName(leaf, i, used),
			OS:         leaf.OS,
			PG:         leaf.PG,
			Arch:       leaf.Arch,
			Count:      1,
			Filesystem: fsPool[rng.Intn(len(fsPool))],
			StorageGB:  10 + rng.Intn(20)*5, // 10, 15, ..., 100
		}
		out.Entries = append(out.Entries, entry)
	}

	// Force-include features the operator asked for, if not
	// already represented and there's room.
	//
	// PreferPatroni is currently a no-op for the soak fleet
	// generator: `compose generate` for role:patroni-cluster
	// emits N PG nodes from the testbed image but does NOT
	// emit an etcd container, does NOT install Patroni in the
	// image, and does NOT generate Patroni configs.  The
	// resulting compose stack tries to bring up N standalones
	// with no DCS and fails during container creation
	// (regression observed in run-20260506-102722).
	//
	// Real Patroni testing lives in the patroni-local-docker
	// TOPOLOGY consumed by L4 scenarios (Spilo image + etcd +
	// Patroni configs all wired up).  Until the soak runner
	// grows equivalent Patroni support we deliberately ignore
	// PreferPatroni here rather than producing a fleet that
	// fails compose-up.
	_ = opts.PreferPatroni // intentionally unused; see comment above
	// PreferArm64 only applies when the ArchPool actually
	// includes arm64 — otherwise the operator pinned to
	// amd64 and the PreferArm64 flag is a contradiction we
	// silently ignore.
	if opts.PreferArm64 && opts.Count >= 4 && !hasArm64(out) && hasArm64Cells(leaves) {
		for idx := range out.Entries {
			os_, _ := c.FindOS(out.Entries[idx].OS)
			if os_.SupportsArch("arm64") {
				out.Entries[idx].Arch = "arm64"
				break
			}
		}
	}

	// Final round: validate against the catalog so we
	// never emit a fleet our own validator rejects.
	if err := out.Validate(c); err != nil {
		return nil, fmt.Errorf("random: produced an invalid fleet (likely a bug): %w", err)
	}
	return out, nil
}

// leaf is one (os, pg, arch) cell from the catalog cross-product.
type leaf struct {
	OS     string
	PG     string
	Arch   string
	Family string
}

func expandLeaves(c *catalog.Catalog) []*leaf {
	return expandLeavesFiltered(c, nil)
}

// expandLeavesFiltered is expandLeaves but filtered by the
// supplied arch allow-list.  Empty allow-list = all arches.
func expandLeavesFiltered(c *catalog.Catalog, archPool []string) []*leaf {
	allowedArch := func(a string) bool {
		if len(archPool) == 0 {
			return true
		}
		for _, allowed := range archPool {
			if allowed == a {
				return true
			}
		}
		return false
	}
	var out []*leaf
	for _, o := range c.OSes {
		for _, pg := range o.PGVersions {
			// Skip dev versions for soak runs — operators
			// chasing 18-dev wire it explicitly.
			if strings.Contains(pg, "-dev") {
				continue
			}
			for _, arch := range o.Arches {
				if !allowedArch(arch) {
					continue
				}
				out = append(out, &leaf{
					OS:     o.ID,
					PG:     pg,
					Arch:   arch,
					Family: o.Family,
				})
			}
		}
	}
	// Stable order before the rng shuffle so the same seed
	// always sees the same input slice.
	sort.Slice(out, func(i, j int) bool {
		if out[i].OS != out[j].OS {
			return out[i].OS < out[j].OS
		}
		if out[i].PG != out[j].PG {
			return out[i].PG < out[j].PG
		}
		return out[i].Arch < out[j].Arch
	})
	return out
}

// diversityPicker draws from the leaf pool with rotation across
// (family, PG-major) so a 5-cell pick rarely ends up "all
// rocky9 + all PG 17".
type diversityPicker struct {
	pool       []*leaf
	familyHits map[string]int
	pgHits     map[string]int
}

func newDiversityPicker(pool []*leaf) *diversityPicker {
	return &diversityPicker{
		pool:       pool,
		familyHits: map[string]int{},
		pgHits:     map[string]int{},
	}
}

// next returns the next leaf, biased to whichever family / PG
// has been picked least so far.  rng is used to break ties.
func (p *diversityPicker) next(rng *rand.Rand) *leaf {
	if len(p.pool) == 0 {
		return nil
	}
	// Score every remaining leaf by familyHits + pgHits,
	// pick the lowest score.
	bestIdx := 0
	bestScore := score(p.pool[0], p.familyHits, p.pgHits)
	for i := 1; i < len(p.pool); i++ {
		s := score(p.pool[i], p.familyHits, p.pgHits)
		if s < bestScore {
			bestScore = s
			bestIdx = i
			continue
		}
		if s == bestScore && rng.Intn(2) == 0 {
			bestIdx = i
		}
	}
	chosen := p.pool[bestIdx]
	p.familyHits[chosen.Family]++
	p.pgHits[chosen.PG]++
	// Remove from pool so we don't pick the exact same cell
	// twice.
	p.pool = append(p.pool[:bestIdx], p.pool[bestIdx+1:]...)
	return chosen
}

func score(l *leaf, familyHits, pgHits map[string]int) int {
	return familyHits[l.Family]*2 + pgHits[l.PG]
}

func uniqueName(l *leaf, i int, used map[string]bool) string {
	base := fmt.Sprintf("%s-pg%s", sanitizeName(l.OS), l.PG)
	if l.Arch == "arm64" {
		base += "-arm"
	}
	candidate := base
	for n := 1; used[candidate]; n++ {
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
	used[candidate] = true
	return candidate
}

func sanitizeName(s string) string {
	out := strings.ToLower(s)
	out = strings.ReplaceAll(out, ":", "-")
	out = strings.ReplaceAll(out, ".", "")
	return out
}

func hasPatroni(f *config.Fleet) bool {
	for _, e := range f.Entries {
		if e.Role == "patroni-cluster" {
			return true
		}
	}
	return false
}

func hasArm64(f *config.Fleet) bool {
	for _, e := range f.Entries {
		if e.Arch == "arm64" {
			return true
		}
	}
	return false
}

func hasArm64Cells(leaves []*leaf) bool {
	for _, l := range leaves {
		if l.Arch == "arm64" {
			return true
		}
	}
	return false
}

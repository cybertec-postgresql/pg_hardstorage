// count.go — CountPolicy: keep N most-recent full backups.
package retention

import (
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// CountPolicy keeps the most recent KeepFulls full backups; everything
// else is deleted. Incremental backups are deleted unconditionally
// (v0.1 only does fulls anyway; the type filter is forward-compat).
//
// Like the others, the newest backup is always kept regardless.
type CountPolicy struct {
	KeepFulls int
}

// Name implements Policy.
func (CountPolicy) Name() string { return "count" }

// Apply implements Policy.
func (p CountPolicy) Apply(_ time.Time, in []*backup.Manifest) Decision {
	d := Decision{PolicyName: p.Name()}
	sorted := sortByStoppedAtDesc(in)

	picked := 0
	keep := p.KeepFulls
	if keep < 0 {
		keep = 0
	}
	for _, m := range sorted {
		if m.Type != backup.BackupTypeFull && m.Type != "" {
			// Skip incremental/snapshot for the count cap; they get
			// taken care of when their parent full ages out. v0.1
			// makes only fulls so this is a forward-compat branch.
			continue
		}
		if picked >= keep {
			break
		}
		picked++
		d.addReason(m.BackupID, fmt.Sprintf("full-%d", picked))
	}

	finalize(&d, sorted)
	return d
}

// manifest.go — bundleManifest: JSON header for the reproducer tarball (seed/iteration/failure).
package reproducer

import (
	"encoding/json"
	"time"
)

// bundleManifest is the JSON file at the root of every
// reproducer tarball.  Schema-typed so a future tool can
// process bundles deterministically.
type bundleManifest struct {
	Schema         string    `json:"schema"`
	FailingCell    string    `json:"failing_cell"`
	Iteration      int       `json:"iteration"`
	Seed           int64     `json:"seed"`
	FailureMessage string    `json:"failure_message"`
	ProjectName    string    `json:"project_name"`
	CreatedAt      time.Time `json:"created_at"`
}

func (m bundleManifest) encode() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

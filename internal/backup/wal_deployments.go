// wal_deployments.go — which deployments have archived WAL.
package backup

import (
	"context"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// WALSegmentCounts returns the number of committed WAL segment manifests
// per deployment.
//
// ManifestStore.Deployments answers "which deployments have a backup".
// That is the right question for everything that iterates backups, and
// the wrong one for a report on what a repository holds: a deployment
// that only streams or archives WAL has no manifests/ entry at all, so
// a report built on Deployments alone describes a repository full of
// its WAL as empty.
//
// One pass over the wal/ prefix; nothing is read.
func WALSegmentCounts(ctx context.Context, sp storage.StoragePlugin) (map[string]int, error) {
	counts := map[string]int{}
	for info, err := range sp.List(ctx, walManifestPrefix) {
		if err != nil {
			return nil, fmt.Errorf("backup: list WAL segment manifests: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if dep, ok := WALSegmentManifestDeployment(info.Key); ok {
			counts[dep]++
		}
	}
	return counts, nil
}

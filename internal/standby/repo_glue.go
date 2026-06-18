// repo_glue.go — repoSession bundle + side-effect imports registering storage URL schemes.
package standby

import (
	"context"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"

	// Side-effect imports: register URL schemes so a `file://` or
	// `s3://` repo in the standby state file resolves to a concrete
	// backend without the caller having to wire registrations.
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
)

// repoSession bundles a StoragePlugin + ManifestStore so resolveBackup
// can do its single walk, then Close() releases the backend.
type repoSession struct {
	sp    storage.StoragePlugin
	store *backup.ManifestStore
}

func openManifestStore(ctx context.Context, url string) (*repoSession, error) {
	_, sp, err := repo.Open(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("standby: open repo %s: %w", url, err)
	}
	return &repoSession{sp: sp, store: backup.NewManifestStore(sp)}, nil
}

// Close releases the underlying storage plugin.
func (r *repoSession) Close() error { return r.sp.Close() }

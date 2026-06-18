// storage_glue.go — side-effect imports registering Tier-1 storage URL schemes for server.Run.
package server

// Side-effect imports: register URL schemes so server.Run can open
// any repo whose scheme has a Tier-1 plugin without forcing the
// caller to wire the registrations.
import (
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/azblob"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/gcs"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
)

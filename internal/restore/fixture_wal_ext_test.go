package restore_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testfixture"
)

// plantArchivedWAL: see testfixture.PlantArchivedWAL.
func plantArchivedWAL(t *testing.T, sp storage.StoragePlugin, deployment string, timeline uint32) {
	t.Helper()
	testfixture.PlantArchivedWAL(t, sp, deployment, timeline)
}

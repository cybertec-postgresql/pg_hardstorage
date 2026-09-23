package cli_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testfixture"
)

// plantArchivedWAL: see testfixture.PlantArchivedWAL.
func (w *readWorld) plantArchivedWAL(t *testing.T, deployment string, timeline uint32) {
	t.Helper()
	testfixture.PlantArchivedWAL(t, w.sp, deployment, timeline)
}

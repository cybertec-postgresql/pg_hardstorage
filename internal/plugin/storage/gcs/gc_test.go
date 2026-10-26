package gcs_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract/gccontract"
)

func TestGCS_RepoGC(t *testing.T) {
	requireDocker(t)
	gccontract.Run(t, openGCSOnFreshFake(t))
}

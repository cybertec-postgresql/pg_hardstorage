package scp_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract/gccontract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
)

func TestSCP_RepoGC(t *testing.T) {
	gccontract.Run(t, scp.OpenInprocForTest(t))
}

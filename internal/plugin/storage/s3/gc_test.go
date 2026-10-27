package s3_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract/gccontract"
)

func TestS3_RepoGC(t *testing.T) {
	p, _ := mustOpen(t, "")
	gccontract.Run(t, p)
}

package azblob_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract/gccontract"
)

func TestAzblob_RepoGC(t *testing.T) {
	gccontract.Run(t, openAzblobOnFreshAzurite(t))
}

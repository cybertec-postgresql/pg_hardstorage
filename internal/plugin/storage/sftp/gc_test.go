package sftp_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract/gccontract"
)

func TestSFTP_RepoGC(t *testing.T) {
	requireDocker(t)
	gccontract.Run(t, openSFTPOnFresh(t))
}

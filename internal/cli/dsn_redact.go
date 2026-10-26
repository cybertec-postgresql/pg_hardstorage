// dsn_redact.go — thin wrappers over internal/pg/dsnredact.
package cli

import "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/dsnredact"

func redactDSN(dsn string) string { return dsnredact.Redact(dsn) }

func splitDSNPassword(dsn string) (stripped, password string, ok bool) {
	return dsnredact.SplitPassword(dsn)
}

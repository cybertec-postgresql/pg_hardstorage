// configfiles.go — query the PG server for the on-disk paths of its
// data directory and configuration files.

package pg

import (
	"context"
	"fmt"
)

// ConfigFileLocations carries the four PG settings that decide
// whether the configuration files are inside or outside PGDATA.
//
// PG's standard setup (initdb-default, RHEL/Rocky packaging) puts
// postgresql.conf, pg_hba.conf, and pg_ident.conf INSIDE the data
// directory.  Debian / Ubuntu packaging puts them under
// /etc/postgresql/<ver>/<cluster>/, OUTSIDE.  Whether a missing
// config file in a base backup is a problem therefore depends on
// the source PG's layout.
//
// All paths are reported as the server resolves them — absolute,
// without trailing slash for the data directory.
type ConfigFileLocations struct {
	// DataDirectory is `SHOW data_directory` / `current_setting`.
	// Empty only on a server with the rare configuration where the
	// setting is null, which we treat as a query failure.
	DataDirectory string

	// ConfigFile is the resolved path to postgresql.conf as PG sees it.
	ConfigFile string

	// HbaFile is the resolved path to pg_hba.conf.
	HbaFile string

	// IdentFile is the resolved path to pg_ident.conf.
	IdentFile string
}

// QueryConfigFileLocations runs four current_setting() queries on c
// and returns the resolved paths.  c must be a regular-mode
// connection — replication-mode connections do not accept SELECT.
//
// Returns a non-nil error if any of the four queries fails.  We
// treat "no rows" / NULL as an error rather than an empty string
// because every running PG sets all four.
func QueryConfigFileLocations(ctx context.Context, c *Conn) (ConfigFileLocations, error) {
	if c == nil {
		return ConfigFileLocations{}, fmt.Errorf("pg: nil connection")
	}
	if c.Mode() != ModeRegular {
		return ConfigFileLocations{}, fmt.Errorf("pg: QueryConfigFileLocations requires ModeRegular; got %s", c.Mode())
	}
	out := ConfigFileLocations{}
	type setting struct {
		name string
		dst  *string
	}
	for _, s := range []setting{
		{"data_directory", &out.DataDirectory},
		{"config_file", &out.ConfigFile},
		{"hba_file", &out.HbaFile},
		{"ident_file", &out.IdentFile},
	} {
		v, err := readSetting(ctx, c, s.name)
		if err != nil {
			return ConfigFileLocations{}, fmt.Errorf("pg: read %s: %w", s.name, err)
		}
		if v == "" {
			return ConfigFileLocations{}, fmt.Errorf("pg: %s is empty", s.name)
		}
		*s.dst = v
	}
	return out, nil
}

// readSetting issues `SELECT current_setting($1)`.  Internal helper
// used by QueryConfigFileLocations; replication-mode connections
// can't run this so callers must pass a ModeRegular conn.
func readSetting(ctx context.Context, c *Conn, name string) (string, error) {
	rows, err := c.PgConn().Exec(ctx, "SELECT current_setting('"+name+"')").ReadAll()
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0].Rows) == 0 || len(rows[0].Rows[0]) == 0 {
		return "", fmt.Errorf("pg: current_setting(%q) returned no rows", name)
	}
	return string(rows[0].Rows[0][0]), nil
}

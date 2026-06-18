// essentialfiles.go — post-BASE_BACKUP check that the manifest
// includes every file PG requires to start.
//
// Why this exists (issue #84): PG's BASE_BACKUP streams whatever it
// finds while walking the data directory.  If postgresql.conf has
// been removed from PGDATA on a running server, PG logs a warning
// and streams the rest — the resulting backup is shape-valid,
// pg_verifybackup reports OK, but pg_ctl start fails on restore.
//
// pg_hardstorage's job at backup commit time is to assert "this
// backup is restorable".  This file's check is that gate: it
// inspects the manifest's file list against the PG-required set
// and refuses to commit a manifest missing any of them.

package backup

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// alwaysRequiredFiles is the set every PG data directory must
// carry, regardless of how the server is configured.  PG_VERSION
// is written by initdb and never absent from a healthy PGDATA;
// postgresql.auto.conf is created by initdb and managed by ALTER
// SYSTEM, also never absent.
//
// backup_label is written by BASE_BACKUP itself into the streamed
// tarball, so its presence is enforced by Manifest.BackupLabel
// rather than by the manifest file list.
var alwaysRequiredFiles = []string{
	"PG_VERSION",
	"postgresql.auto.conf",
}

// optionalIfExternalFiles is the set that lives inside PGDATA in
// most distributions (RHEL / Rocky / initdb-default) but may be
// configured to live elsewhere (Debian / Ubuntu packaging).  We
// require each only when the corresponding `*_file` setting on the
// source PG resolves INSIDE the data_directory — i.e. the operator
// expects pg_basebackup to capture it.
//
// The mapping (basename in PGDATA → which GUC's path to compare):
//
//	postgresql.conf  → config_file
//	pg_hba.conf      → hba_file
//	pg_ident.conf    → ident_file
type optionalCheck struct {
	BaseName string // file we expect to find in the manifest
	GUC      string // human-readable GUC name for the error message
	Resolved string // the GUC's resolved path; populated by the caller
}

// MissingEssentialFilesError is returned by CheckEssentialFiles
// when the manifest is missing one or more PG-required files.
// The structured field surfaces the full list so the operator can
// see every gap at once.
type MissingEssentialFilesError struct {
	// AlwaysRequired lists basenames in alwaysRequiredFiles that
	// did not appear in the manifest.
	AlwaysRequired []string
	// InternalConfigs lists basenames from optionalIfExternalFiles
	// whose GUC pointed inside PGDATA AND that did not appear in
	// the manifest.  These are the issue #84 case.
	InternalConfigs []string
}

// Error implements the error interface.  Phrased so the message
// reads naturally inside an output.NewError wrap.
func (e *MissingEssentialFilesError) Error() string {
	var parts []string
	if len(e.AlwaysRequired) > 0 {
		parts = append(parts,
			fmt.Sprintf("PG-required files absent from backup: %s",
				strings.Join(e.AlwaysRequired, ", ")))
	}
	if len(e.InternalConfigs) > 0 {
		parts = append(parts,
			fmt.Sprintf("config files that live inside PGDATA but are absent from backup: %s "+
				"(check the source server — were they deleted while PG was running?)",
				strings.Join(e.InternalConfigs, ", ")))
	}
	return strings.Join(parts, "; ")
}

// CheckEssentialFiles verifies that m.Files contains every PG file
// the source server requires for a successful restore.
//
//   - alwaysRequiredFiles must appear unconditionally.  PG_VERSION
//     missing implies a fundamentally broken data directory.
//   - For each entry in optionalIfExternalFiles, the corresponding
//     GUC's resolved path is compared to the source PG's
//     data_directory.  If the resolved path is INSIDE data_directory,
//     the basename must appear in the manifest.
//
// dataDir, configFile, hbaFile, identFile are taken from
// pg.ConfigFileLocations — passed as separate strings so this
// function has no dependency on internal/pg (keeps the test surface
// small).
//
// Returns a *MissingEssentialFilesError when at least one expected
// file is missing.  Returns nil on success.  Returns a plain error
// for malformed input (empty manifest file list, empty dataDir).
func CheckEssentialFiles(m *Manifest, dataDir, configFile, hbaFile, identFile string) error {
	if m == nil {
		return errors.New("CheckEssentialFiles: nil manifest")
	}
	if dataDir == "" {
		return errors.New("CheckEssentialFiles: empty data_directory; cannot reason about external configs")
	}
	dataDir = filepath.Clean(dataDir)

	// Index the manifest's file list by basename.  PG's
	// backup_manifest paths are relative to the data directory, so
	// the basename is the right key for the top-level files we
	// check.  This also dedups — duplicate paths would have failed
	// Manifest.Validate already.
	present := make(map[string]bool, len(m.Files))
	for i := range m.Files {
		// We only care about top-level entries; nested paths like
		// "base/16384/..." cannot match a basename in our required
		// set without an accident, but filepath.Base normalises
		// either way.
		p := m.Files[i].Path
		if strings.ContainsAny(p, "/\\") {
			// Top-level files have no path separator.  Skip nested
			// entries to keep the lookup O(top-level-files) and
			// avoid a spurious match if a nested directory shares a
			// basename with a config file.
			continue
		}
		present[p] = true
	}

	miss := &MissingEssentialFilesError{}
	for _, base := range alwaysRequiredFiles {
		if !present[base] {
			miss.AlwaysRequired = append(miss.AlwaysRequired, base)
		}
	}

	for _, oc := range []optionalCheck{
		{BaseName: "postgresql.conf", GUC: "config_file", Resolved: configFile},
		{BaseName: "pg_hba.conf", GUC: "hba_file", Resolved: hbaFile},
		{BaseName: "pg_ident.conf", GUC: "ident_file", Resolved: identFile},
	} {
		if oc.Resolved == "" {
			// Caller passed nothing for this GUC.  Skip — we
			// don't know whether the file is inside or outside
			// PGDATA, so we can't meaningfully require it.
			continue
		}
		if !insideDataDir(oc.Resolved, dataDir) {
			// External config; PG never streamed it; not our gap.
			continue
		}
		if !present[oc.BaseName] {
			miss.InternalConfigs = append(miss.InternalConfigs, oc.BaseName)
		}
	}

	if len(miss.AlwaysRequired) == 0 && len(miss.InternalConfigs) == 0 {
		return nil
	}
	// Sort for deterministic ordering across runs — easier for
	// operators reading a log + easier on test assertions.
	sort.Strings(miss.AlwaysRequired)
	sort.Strings(miss.InternalConfigs)
	return miss
}

// insideDataDir reports whether path resolves inside dataDir on the
// SAME filesystem.  Symlinks are not chased — PG resolves the path
// itself and emits the resolved form via current_setting.
//
// Both arguments are filepath.Clean'd; we then test that path
// shares a common prefix with dataDir followed by a path separator
// (or is dataDir exactly, which would be malformed input but
// reported as "inside" for safety).
func insideDataDir(path, dataDir string) bool {
	p := filepath.Clean(path)
	d := filepath.Clean(dataDir)
	if p == d {
		return true
	}
	prefix := d + string(filepath.Separator)
	return strings.HasPrefix(p, prefix)
}

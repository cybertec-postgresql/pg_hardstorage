// mapping.go — Barman INI → pg_hardstorage YAML key-translation table (one row per Barman setting).
package translate

import (
	"fmt"
	"strings"
)

// mappingRow describes how to translate one Barman key.
//
// `render` returns the YAML snippet (without indentation — caller
// indents under `deployments:`).  If `drop` is true, the renderer
// emits a comment line instead of YAML and surfaces the reason in
// the Unmapped summary.
type mappingRow struct {
	render func(v string) (yaml string, drop bool, reason string)
}

// mappingByKey is the Barman-key → renderer table.  Keep this list
// alphabetically sorted so adding entries is mechanical.
var mappingByKey = map[string]mappingRow{
	// Connection.  Barman uses libpq DSN strings; native uses the
	// same wire format under deployment.connection.
	"conninfo": {render: func(v string) (string, bool, string) {
		return fmt.Sprintf("connection: %s", yamlString(v)), false, ""
	}},
	"streaming_conninfo": {render: func(v string) (string, bool, string) {
		// Native uses one connection field; if both are set we
		// prefer streaming_conninfo (it's the slot conn) — but emit
		// a comment so the operator notices the collapse.
		return fmt.Sprintf("connection: %s  # collapsed from streaming_conninfo", yamlString(v)), false, ""
	}},

	// Description / display name.
	"description": {render: func(v string) (string, bool, string) {
		return fmt.Sprintf("description: %s", yamlString(v)), false, ""
	}},

	// Backup method.  rsync / postgres / snapshot — only `postgres`
	// (streaming) is the native default; rsync / snapshot are not
	// in v1.1.
	"backup_method": {render: func(v string) (string, bool, string) {
		v = strings.ToLower(strings.TrimSpace(v))
		switch v {
		case "postgres", "streaming":
			return "# backup_method: postgres (native default; flag elided)", false, ""
		case "rsync":
			return "", true, "rsync backup method not supported in v1.1; use streaming"
		case "snapshot":
			return "", true, "snapshot backup method not supported in v1.1; use streaming"
		default:
			return "", true, fmt.Sprintf("unknown backup method %q", v)
		}
	}},

	// Retention.
	"retention_policy": {render: func(v string) (string, bool, string) {
		// Barman: "RECOVERY WINDOW OF 7 DAYS" or "REDUNDANCY 4".
		// Native YAML uses retention.{keep_full,keep_for}.
		up := strings.ToUpper(strings.TrimSpace(v))
		switch {
		case strings.HasPrefix(up, "RECOVERY WINDOW OF "):
			window := strings.TrimSpace(strings.TrimPrefix(up, "RECOVERY WINDOW OF "))
			return fmt.Sprintf("retention:\n  keep_for: %s", strings.ToLower(window)), false, ""
		case strings.HasPrefix(up, "REDUNDANCY "):
			n := strings.TrimSpace(strings.TrimPrefix(up, "REDUNDANCY "))
			return fmt.Sprintf("retention:\n  keep_full: %s", n), false, ""
		default:
			return "", true, fmt.Sprintf("unrecognised retention_policy %q", v)
		}
	}},
	"minimum_redundancy": {render: func(v string) (string, bool, string) {
		return fmt.Sprintf("retention:\n  minimum_redundancy: %s", v), false, ""
	}},

	// WAL / archive_command knobs.  Native streams from a slot by
	// default, so most of these are informational comments.
	"archive_mode": {render: func(_ string) (string, bool, string) {
		return "", true, "PG-side setting; managed by the DBA, not the backup tool"
	}},
	"archive_command": {render: func(_ string) (string, bool, string) {
		return "", true, "PG-side setting; native uses streaming slots by default"
	}},

	// Compression.  Native picks zstd by default; surface the choice
	// only if Barman explicitly asks for something else.
	"compression": {render: func(v string) (string, bool, string) {
		v = strings.ToLower(strings.TrimSpace(v))
		switch v {
		case "", "none":
			return "compression:\n  algorithm: none", false, ""
		case "gzip":
			return "compression:\n  algorithm: gzip", false, ""
		case "bzip2":
			return "", true, "bzip2 compression not supported in v1.1 (use gzip or zstd)"
		case "pigz", "pbzip2":
			return "", true, "parallel-coder compression names not exposed in v1.1; native zstd is parallel by default"
		case "zstd":
			return "compression:\n  algorithm: zstd", false, ""
		default:
			return "", true, fmt.Sprintf("unknown compression %q", v)
		}
	}},

	// Slot.
	"slot_name": {render: func(v string) (string, bool, string) {
		return fmt.Sprintf("slot:\n  name: %s", yamlString(v)), false, ""
	}},
	"streaming_archiver": {render: func(_ string) (string, bool, string) {
		return "", true, "native is streaming-first by default; explicit flag not required"
	}},
	"create_slot": {render: func(v string) (string, bool, string) {
		v = strings.ToLower(strings.TrimSpace(v))
		switch v {
		case "auto":
			return "slot:\n  create: auto", false, ""
		case "manual":
			return "slot:\n  create: manual", false, ""
		default:
			return "", true, fmt.Sprintf("unknown create_slot %q", v)
		}
	}},

	// Path.  Barman's `backup_directory` is the closest analogue to
	// the native `repo:` URL; we render it as a file:// URL so the
	// operator gets a working starting point.
	"backup_directory": {render: func(v string) (string, bool, string) {
		v = strings.TrimSpace(v)
		if !strings.Contains(v, "://") {
			v = "file://" + v
		}
		return fmt.Sprintf("repo: %s", yamlString(v)), false, ""
	}},

	// Parallelism.
	"parallel_jobs": {render: func(v string) (string, bool, string) {
		return fmt.Sprintf("parallelism: %s", v), false, ""
	}},

	// Checks.
	"check_timeout": {render: func(v string) (string, bool, string) {
		// Native doctor doesn't have a per-deployment override;
		// surface as a comment with the value preserved.
		return fmt.Sprintf("# check_timeout: %s  (native doctor uses a global timeout)", v), false, ""
	}},
	"immediate_checkpoint": {render: func(v string) (string, bool, string) {
		v = strings.ToLower(strings.TrimSpace(v))
		switch v {
		case "true", "on", "1":
			return "backup:\n  fast: true", false, ""
		case "false", "off", "0", "":
			return "backup:\n  fast: false", false, ""
		default:
			return "", true, fmt.Sprintf("unknown immediate_checkpoint %q", v)
		}
	}},

	// Retry knobs — drop with note (native has built-in backoff).
	"retry_times": {render: func(_ string) (string, bool, string) {
		return "", true, "native has built-in exponential backoff"
	}},
	"retry_sleep": {render: func(_ string) (string, bool, string) {
		return "", true, "native has built-in exponential backoff"
	}},

	// Pre/post hooks.  These are operator-defined shell commands;
	// surface them as comments and point at the native notify hooks.
	"pre_backup_script": {render: func(v string) (string, bool, string) {
		return "", true, fmt.Sprintf("pre/post scripts not auto-translated; wire %q via `notify` hooks", v)
	}},
	"post_backup_script": {render: func(v string) (string, bool, string) {
		return "", true, fmt.Sprintf("pre/post scripts not auto-translated; wire %q via `notify` hooks", v)
	}},
	"pre_archive_script": {render: func(v string) (string, bool, string) {
		return "", true, fmt.Sprintf("pre/post scripts not auto-translated; wire %q via `notify` hooks", v)
	}},
	"post_archive_script": {render: func(v string) (string, bool, string) {
		return "", true, fmt.Sprintf("pre/post scripts not auto-translated; wire %q via `notify` hooks", v)
	}},
}

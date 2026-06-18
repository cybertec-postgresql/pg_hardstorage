// Package translate converts a Barman INI configuration file into
// a pg_hardstorage YAML deployment file.
//
// Barman's config has a `[barman]` global section plus one
// `[<server>]` section per cluster — typical layout is one file
// per server in /etc/barman.d/.  We accept either a single multi-
// section file or a glob of per-server files (the caller resolves
// the glob; we read what's handed to us).
//
// Settings with a clean native equivalent translate directly.
// Settings without an equivalent emit as YAML comments and feed
// into a stderr summary so operators see what didn't transfer.
package translate

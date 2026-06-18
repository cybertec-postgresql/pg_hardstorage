// Package repo defines the on-disk layout and high-level operations of
// a pg_hardstorage repository.
//
// Slice 3 exposes only Init (write the HSREPO magic file) and Open (read
// it back). The CAS, manifest, GC, scrub, and replication primitives land
// in later slices and live in this package.
package repo

import "fmt"

// Repository file naming. Stable across versions; bumping any of these
// would be a breaking change requiring a major version of the repo schema.
const (
	// HSREPOFilename is the magic-file name at the repo root. Its presence
	// identifies a directory / bucket prefix as a pg_hardstorage repo.
	HSREPOFilename = "HSREPO"

	// SchemaRepo is the Schema field in the HSREPO body.
	SchemaRepo = "pg_hardstorage.repo.v1"

	// RepoVersionFilename is the on-disk-format marker.  Sits alongside
	// HSREPO and advertises the repo's storage-layout version
	// independently of the manifest schema.  Read on every Open and
	// gated through the SupportedRepoFormats allowlist below — a
	// future-format repo (e.g. "v1.1" when this binary only knows
	// "v1.0") is refused with ErrRepoFormatUnsupported instead of
	// silently partial-reading.
	//
	// The file is OPTIONAL: repos created pre-v0.10 do not have it;
	// Open treats absence as RepoFormatV1_0 for back-compat.  Init
	// writes it on every fresh repo so going forward the file is
	// always present.
	RepoVersionFilename = "_repo_version.json"

	// RepoFormatV1_0 is the canonical "current" on-disk format value
	// the binary writes into _repo_version.json at init time.  Bumped
	// when the storage layout changes in a way that breaks read-back
	// for older binaries; SupportedRepoFormats below lists every
	// format value THIS binary can safely read.
	RepoFormatV1_0 = "v1.0"
)

// SupportedRepoFormats is the allowlist of _repo_version.json
// `format` values this binary can safely operate against.  Open
// refuses if the marker file's format is not in this list — see
// the test scenario at test/scenarios/L4_repo_format_forward_check
// for the canonical regression for this property.
var SupportedRepoFormats = []string{RepoFormatV1_0}

// RepoVersion is the JSON body of _repo_version.json.  Held
// deliberately small; the marker is the canary, not the catalog.
type RepoVersion struct {
	Format    string `json:"format"`
	WrittenBy string `json:"written_by,omitempty"`
	WrittenAt string `json:"written_at,omitempty"`
}

// Metadata is the JSON body of HSREPO. Optional fields are added over
// time; missing fields default to their zero values when reading older
// repos.
type Metadata struct {
	Schema      string `json:"schema"`
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`
	ToolVersion string `json:"tool_version,omitempty"`

	// Mode is the repository's write-access posture. Empty on repos
	// created pre-v0.2; read as ModeReadWrite for back-compat. Set
	// via `pg_hardstorage repo set-mode <url> read-only|read-write`.
	Mode Mode `json:"mode,omitempty"`

	// UpdatedAt records the most recent HSREPO rewrite (set-mode is
	// the only writer today). Empty on repos that haven't been
	// rewritten since init.
	UpdatedAt string `json:"updated_at,omitempty"`

	// WORM is the repository's write-once-read-many policy. When
	// non-nil + non-zero, every committed object (chunks, manifests,
	// replicas, audit events) gets a retention deadline propagated
	// to the storage backend at PUT time. Set at init time only —
	// flipping WORM on an existing repo would create a mixed-fleet
	// situation operators can't reason about.
	WORM *WORMPolicy `json:"worm,omitempty"`

	// Compression selects the zstd encoder level for new chunks.
	// One of {fast, balanced, max}; empty means "balanced" for
	// repos created pre-v0.10 (back-compat).  Set at repo init
	// time and not changed after — a repo holding a mix of
	// levels still reads back fine (the decoder handles every
	// level), but the operator's "what does my CPU/disk
	// trade-off look like?" answer is more legible when the
	// level is stable across the repo's chunks.
	//
	// Trade-off: profiling under a 10-GB-WAL workload showed
	// the previous default ("balanced", ~ klauspost
	// SpeedBetterCompression / zstd level 7) burned ~40% of
	// pg_hardstorage CPU.  "fast" (~ SpeedDefault / level 3)
	// roughly halves that for ~10-15% larger on-disk size;
	// "max" (~ SpeedBestCompression / level 11) trades 2-3×
	// CPU for ~5% smaller on-disk size.
	Compression CompressionLevel `json:"compression,omitempty"`
}

// CompressionLevel is the operator-facing zstd level preset.
// We expose three named tiers rather than the raw klauspost
// encoder-level enum: operators don't need to learn another
// library's vocabulary, and the named tiers stay stable if a
// future release retunes the underlying integer.
type CompressionLevel string

const (
	// CompressionUnset means "use the default for the
	// repo's tool_version".  Today that's CompressionBalanced;
	// pre-v0.10 repos lived on the same default with no
	// explicit field, so reading "" should map to balanced.
	CompressionUnset CompressionLevel = ""

	// CompressionFast — klauspost SpeedDefault (~zstd
	// level 3).  Recommended for write-heavy clusters
	// where wal-stream CPU matters more than disk bytes.
	CompressionFast CompressionLevel = "fast"

	// CompressionBalanced — klauspost SpeedBetterCompression
	// (~zstd level 7).  v0.1..default; the sweet
	// spot for the median operator.
	CompressionBalanced CompressionLevel = "balanced"

	// CompressionMax — klauspost SpeedBestCompression
	// (~zstd level 11).  Archive-tier backups that
	// won't be re-read often.
	CompressionMax CompressionLevel = "max"
)

// Resolved returns the level to actually use, mapping the
// unset default to CompressionBalanced for back-compat with
// pre-v0.10 repos.  All other inputs pass through.
func (l CompressionLevel) Resolved() CompressionLevel {
	if l == CompressionUnset {
		return CompressionBalanced
	}
	return l
}

// Validate rejects unknown values.  Empty / "fast" /
// "balanced" / "max" pass; anything else is a typo we want
// to catch at init time, not at the first chunk write.
func (l CompressionLevel) Validate() error {
	switch l {
	case CompressionUnset, CompressionFast, CompressionBalanced, CompressionMax:
		return nil
	}
	return fmt.Errorf("unknown compression level %q (want one of: fast, balanced, max)", string(l))
}

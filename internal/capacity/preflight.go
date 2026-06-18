// preflight.go — free-space pre-flight verdict (pass / insufficient / unsupported) for backups.
package capacity

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// PreflightVerdict is the structured outcome of a capacity
// pre-flight. Three operationally-distinct cases:
//
//   - Pass: free space ≥ projected × safety. The backup can
//     start.
//   - InsufficientSpace: free space < projected × safety. The
//     backup is refused (or the operator passes
//     --ignore-capacity).
//   - Unsupported: backend can't probe (object stores).
//     Pass-through; we don't refuse what we can't measure.
type PreflightVerdict string

const (
	// PreflightPass means free space comfortably exceeds the
	// projected footprint after the safety margin.
	PreflightPass PreflightVerdict = "pass"
	// PreflightInsufficientSpace means projected size × safety
	// margin exceeds available free space; the backup is refused
	// unless the operator overrides.
	PreflightInsufficientSpace PreflightVerdict = "insufficient_space"
	// PreflightUnsupported means the backend cannot report free
	// space (object stores); the check is a no-op pass-through.
	PreflightUnsupported PreflightVerdict = "unsupported"
)

// PreflightResult records the verdict + the inputs that drove
// it. Surfaced via the result body of `capacity preflight` and
// embedded in the structured error returned when the backup
// orchestrator refuses.
type PreflightResult struct {
	Verdict        PreflightVerdict `json:"verdict"`
	ProjectedBytes int64            `json:"projected_bytes"`
	RequiredBytes  int64            `json:"required_bytes"` // ProjectedBytes × SafetyFactor
	TotalBytes     int64            `json:"total_bytes,omitempty"`
	AvailableBytes int64            `json:"available_bytes,omitempty"`
	SafetyFactor   float64          `json:"safety_factor"`
	Note           string           `json:"note,omitempty"` // operator-readable explanation when Unsupported
}

// PreflightOptions configures a Preflight call.
type PreflightOptions struct {
	// ProjectedBytes is the operator-supplied or auto-derived
	// estimate of how many bytes the upcoming backup will
	// land in the repo. Zero is rejected as a programmer
	// error — Preflight has no opinion on what's "big enough"
	// without an actual number to compare against.
	ProjectedBytes int64

	// SafetyFactor multiplies ProjectedBytes to derive
	// RequiredBytes. The plan's resilience design calls for
	// "at least 110% of the projected backup size" —
	// SafetyFactor=1.1 is the default. Operators with very
	// tight repos can set this lower; safety-conscious ops
	// can crank it higher.
	SafetyFactor float64
}

// DefaultSafetyFactor is the documented "110% of projected"
// margin from the resilience design.
const DefaultSafetyFactor = 1.1

// Preflight asks "would a backup of ProjectedBytes succeed
// against this repo's current free space?". Branches:
//
//   - sp doesn't implement FreeSpaceAware → Unsupported
//     (object stores; pass-through).
//   - FreeSpace probe fails → returns the error verbatim.
//     Caller decides fail-open vs fail-closed; the CLI
//     today fails-open (probe failures don't refuse).
//   - AvailableBytes < ProjectedBytes × SafetyFactor → Insufficient.
//   - Otherwise → Pass.
//
// SafetyFactor ≤ 0 falls back to DefaultSafetyFactor so
// callers can leave it zero in opts.
func Preflight(ctx context.Context, sp storage.StoragePlugin, opts PreflightOptions) (*PreflightResult, error) {
	if sp == nil {
		return nil, errors.New("capacity: Preflight requires a non-nil StoragePlugin")
	}
	if opts.ProjectedBytes <= 0 {
		return nil, errors.New("capacity: Preflight requires ProjectedBytes > 0")
	}
	safety := opts.SafetyFactor
	if safety <= 0 {
		safety = DefaultSafetyFactor
	}
	required := int64(float64(opts.ProjectedBytes) * safety)

	free, err := storage.FreeSpaceOf(ctx, sp)
	if err != nil {
		// Probe failure — surface to the caller. The CLI
		// gate decides fail-open. Don't fabricate a verdict.
		return nil, fmt.Errorf("capacity: probe free space: %w", err)
	}
	if free.Unsupported {
		return &PreflightResult{
			Verdict:        PreflightUnsupported,
			ProjectedBytes: opts.ProjectedBytes,
			RequiredBytes:  required,
			SafetyFactor:   safety,
			Note:           "backend does not expose free-space probe; capacity pre-flight is skipped (object-store quotas are out-of-band)",
		}, nil
	}

	res := &PreflightResult{
		ProjectedBytes: opts.ProjectedBytes,
		RequiredBytes:  required,
		TotalBytes:     free.TotalBytes,
		AvailableBytes: free.AvailableBytes,
		SafetyFactor:   safety,
	}
	if free.AvailableBytes < required {
		res.Verdict = PreflightInsufficientSpace
		res.Note = fmt.Sprintf("repo has %d bytes available; backup projected at %d bytes, required %d (with %.0f%% safety margin)",
			free.AvailableBytes, opts.ProjectedBytes, required, (safety-1.0)*100)
		return res, nil
	}
	res.Verdict = PreflightPass
	return res, nil
}

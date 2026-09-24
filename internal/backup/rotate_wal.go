// rotate_wal.go — KEK rotation of the per-segment WAL manifest envelopes.
package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// walManifestPrefix is the root every WAL segment manifest lives under
// (walsink.SegmentPath: wal/<deployment>/<TTTTTTTT>/<segment>.json).
const walManifestPrefix = "wal/"

// rotateWALSegments rewraps the DEK envelope carried by every encrypted
// WAL segment manifest from (OldKEKRef, OldKEK) to (NewKEKRef, NewKEK).
//
// Why WAL is walked separately from backups: segment manifests are not
// reached from any backup manifest (a WAL-first deployment has none at
// all), and `wal fetch` unwraps each segment's OWN envelope — it never
// consults the shared-DEK slot or a backup manifest when one is present.
//
// The walk is repo-wide on the wal/ prefix rather than per backup
// deployment for the same reason. Keys are collected before any rewrite
// so a backend whose List walks the live tree (fs) never sees its own
// tmp files.
//
// Idempotent and resumable exactly like the manifest pass: a segment
// already on NewKEKRef — or on OldKEKRef but already wrapped under the
// new key, which is what a segment pushed after the operator installed
// the new kek.bin looks like (local custody stamps the fixed
// local:default ref) — counts as already rotated. Segment manifests are
// unsigned, so the rewrite is a plain overwriting Put of the re-encoded
// body; everything but the envelope stays identical, so the push path's
// idempotent re-archive check (which compares sysid + chunk list) still
// accepts a later retry of the same segment.
//
// WORM: the rewritten body is re-locked with the repo policy. A backend
// that refuses the overwrite (an un-versioned locked object) surfaces as
// a per-segment failure, which keeps the rotation INCOMPLETE — the old
// KEK must then be kept for as long as those segments are retained.
func rotateWALSegments(ctx context.Context, sp storage.StoragePlugin, opts RotateKEKOptions, res *RotateKEKResult) error {
	var keys []string
	for info, err := range sp.List(ctx, walManifestPrefix) {
		if err != nil {
			return fmt.Errorf("list WAL segment manifests: %w", err)
		}
		if _, ok := walSegmentManifestDeployment(info.Key); ok {
			keys = append(keys, info.Key)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		deployment, _ := walSegmentManifestDeployment(key)
		res.WALConsidered++
		outcome, err := rotateOneWALSegment(ctx, sp, key, opts)
		switch outcome {
		case rotateOutcomeRotated, rotateOutcomeWouldRotate:
			res.WALRotated++
		case rotateOutcomeAlreadyRotated:
			res.WALAlreadyRotated++
		case rotateOutcomeSkippedUnencrypted:
			res.WALSkippedUnencrypted++
		case rotateOutcomeSkippedDifferentKEK:
			res.WALSkippedDifferentKEK++
		default:
			res.WALFailed++
			if err == nil {
				err = errors.New("unknown failure")
			}
			if len(res.Failures) < maxRotateKEKFailures {
				res.Failures = append(res.Failures, RotateKEKFailure{
					Deployment: deployment, Key: key, Err: err.Error(),
				})
			}
		}
		if opts.OnProgress != nil {
			ev := RotateKEKProgress{Deployment: deployment, Key: key, Outcome: walOutcomeName(outcome)}
			if err != nil {
				ev.Reason = err.Error()
			}
			opts.OnProgress(ev)
		}
	}
	return nil
}

func rotateOneWALSegment(ctx context.Context, sp storage.StoragePlugin, key string, opts RotateKEKOptions) (rotateOutcome, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("read WAL segment manifest: %w", err)
	}
	raw, err := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
	_ = rc.Close()
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("read WAL segment manifest: %w", err)
	}
	m, err := walsink.ParseSegmentManifest(raw)
	if err != nil {
		return rotateOutcomeFailed, err
	}
	if m.Encryption == nil {
		return rotateOutcomeSkippedUnencrypted, nil
	}
	wrapped, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("decode wrapped_dek: %w", err)
	}
	if m.Encryption.KEKRef == opts.NewKEKRef {
		return rotateOutcomeAlreadyRotated, nil
	}
	if m.Encryption.KEKRef != opts.OldKEKRef {
		return rotateOutcomeSkippedDifferentKEK, nil
	}
	dek, err := encryption.Unwrap(opts.OldKEK, wrapped)
	if err != nil {
		if _, nerr := encryption.Unwrap(opts.NewKEK, wrapped); nerr == nil {
			return rotateOutcomeAlreadyRotated, nil
		}
		return rotateOutcomeFailed, fmt.Errorf("unwrap with OldKEK: %w (neither the old nor the new KEK opens this segment's wrapped_dek)", err)
	}
	wrappedNew, err := encryption.Wrap(opts.NewKEK, dek)
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("wrap with NewKEK: %w", err)
	}
	if opts.DryRun {
		return rotateOutcomeWouldRotate, nil
	}
	m.Encryption.WrappedDEK = base64.StdEncoding.EncodeToString(wrappedNew)
	m.Encryption.KEKRef = opts.NewKEKRef
	body, err := m.MarshalToBytes()
	if err != nil {
		return rotateOutcomeFailed, err
	}
	if err := overwriteManifest(ctx, sp, key, body, opts.RetainUntil, opts.RetentionMode); err != nil {
		return rotateOutcomeFailed, fmt.Errorf("rewrite WAL segment manifest (a WORM lock that forbids overwrite keeps this segment on the old KEK — do not retire it): %w", err)
	}
	return rotateOutcomeRotated, nil
}

// walSegmentManifestDeployment reports whether key is a committed WAL
// segment manifest — exactly wal/<dep>/<8 hex>/<24 hex>.json — and
// returns its deployment. Everything else under wal/ (history and
// .backup/.partial auxiliaries, gap records, commit tmp files) is not
// an envelope carrier and is ignored.
func walSegmentManifestDeployment(key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "wal" || parts[1] == "" {
		return "", false
	}
	name, ok := strings.CutSuffix(parts[3], ".json")
	if !ok || !isUpperHex(parts[2], 8) || !isUpperHex(name, 24) {
		return "", false
	}
	return parts[1], true
}

func isUpperHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func walOutcomeName(o rotateOutcome) string {
	switch o {
	case rotateOutcomeRotated:
		return "rotated"
	case rotateOutcomeWouldRotate:
		return "would_rotate"
	case rotateOutcomeAlreadyRotated:
		return "already_rotated"
	case rotateOutcomeSkippedUnencrypted:
		return "skipped_unencrypted"
	case rotateOutcomeSkippedDifferentKEK:
		return "skipped_different_kek"
	default:
		return "failed"
	}
}

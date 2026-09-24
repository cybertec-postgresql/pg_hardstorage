// repair_scrub_wal.go — scrub helpers: the WAL segment CAS (decrypting,
// from each segment's own envelope) and the coverage verdict shared by
// `repair scrub` and `repo scrub`.
package cli

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// walScrubCASCache resolves the CAS that can read a WAL segment's
// chunks, from the segment manifest's OWN encryption envelope.
//
// WAL has been encrypted since issue #106, and the scrubs read every
// segment through a plain casdefault CAS: each encrypted WAL chunk failed
// to decrypt and was reported as BIT ROT — exit 9, an audit event, and an
// operator told their storage is corrupting data it is not. `wal fetch`
// already builds the right CAS from the envelope
// (decryptingCASFromEnvelope); scrub now does the same.
//
// Segments of one stream share one wrapped DEK, so the resolved CAS is
// cached per envelope: one unwrap (possibly a KMS round trip) per
// distinct DEK, not per segment. A failed resolution is cached too, so a
// missing keyring is reported once per envelope rather than retried for
// every segment.
type walScrubCASCache struct {
	sp      storage.StoragePlugin
	plain   *repo.CAS
	byEnv   map[string]*repo.CAS
	failed  map[string]string
	resolve func(ctx context.Context, sp storage.StoragePlugin, scheme, kekRef, wrappedB64 string) (*repo.CAS, string)
}

func newWALScrubCASCache(sp storage.StoragePlugin) *walScrubCASCache {
	return &walScrubCASCache{
		sp:      sp,
		plain:   casdefault.New(sp),
		byEnv:   map[string]*repo.CAS{},
		failed:  map[string]string{},
		resolve: decryptingCASFromEnvelope,
	}
}

// casFor returns the CAS for m, or ("", reason) when m is encrypted and
// its DEK cannot be resolved on this host.
func (c *walScrubCASCache) casFor(ctx context.Context, m *walsink.SegmentManifest) (*repo.CAS, string) {
	if m.Encryption == nil {
		return c.plain, ""
	}
	k := m.Encryption.Scheme + "\x00" + m.Encryption.KEKRef + "\x00" + m.Encryption.WrappedDEK
	if cas, ok := c.byEnv[k]; ok {
		return cas, ""
	}
	if reason, ok := c.failed[k]; ok {
		return nil, reason
	}
	cas, reason := c.resolve(ctx, c.sp, m.Encryption.Scheme, m.Encryption.KEKRef, m.Encryption.WrappedDEK)
	if cas == nil {
		c.failed[k] = reason
		return nil, reason
	}
	c.byEnv[k] = cas
	return cas, ""
}

// readWALSegmentManifest reads and parses the segment manifest at key.
func readWALSegmentManifest(ctx context.Context, sp storage.StoragePlugin, key string) (*walsink.SegmentManifest, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
	if err != nil {
		return nil, err
	}
	return walsink.ParseSegmentManifest(body)
}

// isKeyUnavailableErr reports whether a chunk read failed because this
// CAS holds no decryptor for the chunk's envelope — a chunk written under
// a key this manifest's CAS does not carry (plaintext-era WAL deduped
// against an encrypted backup chunk). That is a key-coverage gap, not
// evidence the stored bytes are damaged.
func isKeyUnavailableErr(err error) bool {
	return errors.Is(err, encryption.ErrUnknownAlgorithm)
}

// maxKeyUnavailableListed bounds the per-manifest detail a scrub keeps
// for key-unavailable manifests; the count is unbounded.
const maxKeyUnavailableListed = 16

// scrubCoverageError is the non-zero verdict for a scrub that found no
// mismatch but could not look at everything it was asked to.
//
// Two distinct gaps, reported separately because the fixes differ:
//
//   - key_unavailable: an ENCRYPTED manifest (backup or WAL segment)
//     whose DEK this host cannot resolve (keyring absent, wrong KEK, KMS
//     unreachable). Its chunks are unscrubbed. `repair scrub` used to
//     push an all-zero hash into its mismatch list for this: the text
//     said "chunk 000…0 failed integrity check", and `--heal` then
//     skipped the fake hash, healed nothing and exited 0 "all mismatches
//     healed" — a clean bill of health over chunks nobody could read.
//   - unverifiable: a manifest that would not verify or parse.
//
// Returns nil when coverage is complete.
func scrubCoverageError(cmdName, repoURL string, agg scrubResultAgg, refsTotal int) error {
	if agg.KeyUnavailableManifests > 0 {
		return output.NewError("verify.scrub_key_unavailable",
			fmt.Sprintf("%s: %d encrypted manifest(s) could not be decrypted on this host, so their chunks were not scrubbed (sampled %d of %d referenced)%s",
				cmdName, agg.KeyUnavailableManifests, agg.Sampled, refsTotal, keyUnavailableDetail(agg.KeyUnavailable))).
			WithSuggestion(&output.Suggestion{
				Human: "run the scrub where the KEK is available (the keyring holding kek.bin for local:* refs, or credentials and a kms.providers entry for cloud KMS refs). This is a key-access gap, not bit rot — nothing was found wrong with the stored bytes.",
			})
	}
	if agg.UnverifiableManifests > 0 {
		return output.NewError("verify.scrub_unverifiable_manifests",
			fmt.Sprintf("%s: %d manifest(s) could not be verified or read, so their chunks were not scrubbed (sampled %d of %d referenced)",
				cmdName, agg.UnverifiableManifests, agg.Sampled, refsTotal)).
			WithSuggestion(&output.Suggestion{
				Human:   "a manifest that fails Ed25519 verification is potential tampering, not bit rot; the chunks behind it are unscrubbed either way. Investigate with `pg_hardstorage repo check` and the audit chain.",
				Command: fmt.Sprintf("pg_hardstorage repo check --repo %s", repoURL),
			})
	}
	return nil
}

// isWALSegmentKey matches committed WAL segment manifests,
// wal/<dep>/<TLI>/<24-hex>.json — never gap-state records
// (wal/<dep>/gaps/*.json) or staging temps, which are not segment
// manifests and would otherwise be counted as unreadable ones.
func isWALSegmentKey(key string) bool {
	if !strings.HasSuffix(key, ".json") || segmentKeyBasenameIsTemp(key) {
		return false
	}
	base := path.Base(strings.TrimSuffix(key, ".json"))
	if len(base) != 24 {
		return false
	}
	for i := 0; i < len(base); i++ {
		c := base[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func keyUnavailableDetail(list []string) string {
	if len(list) == 0 {
		return ""
	}
	out := ": "
	for i, s := range list {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

package cli

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// storedManifest is a manifest object the repair commands inspect as a
// stream. A backup manifest scales with the database (a multi-TB cluster
// has a multi-GB one), so the repair paths never hold one in memory and
// never apply a read cap: each question (valid under this key? parseable?
// self-consistent?) is one streaming pass over the stored object.
type storedManifest struct {
	ctx context.Context
	sp  storage.StoragePlugin
	key string
}

// storedReadError marks a failure to open the object (as opposed to a
// failure to parse or verify what was read).
type storedReadError struct{ err error }

func (e *storedReadError) Error() string { return e.err.Error() }
func (e *storedReadError) Unwrap() error { return e.err }

func (s storedManifest) open() (io.ReadCloser, error) { return s.sp.Get(s.ctx, s.key) }

func (s storedManifest) stream(fn func(io.Reader) (*backup.StreamInfo, error)) (*backup.StreamInfo, error) {
	rc, err := s.open()
	if err != nil {
		return nil, &storedReadError{err}
	}
	defer rc.Close()
	return fn(rc)
}

// verifies reports whether the object carries a valid signature under v.
func (s storedManifest) verifies(v *backup.Verifier) bool {
	_, err := s.stream(func(r io.Reader) (*backup.StreamInfo, error) { return backup.VerifyStream(r, v) })
	return err == nil
}

// verify is verifies with the reason and the verified header.
func (s storedManifest) verify(v *backup.Verifier) (*backup.StreamInfo, error) {
	return s.stream(func(r io.Reader) (*backup.StreamInfo, error) { return backup.VerifyStream(r, v) })
}

// header parses the object without verifying it.
func (s storedManifest) header() (*backup.StreamInfo, error) {
	return s.stream(backup.ParseHeaderStream)
}

// embeddedErr checks the signature against the manifest's own key.
func (s storedManifest) embeddedErr() error {
	_, err := s.stream(backup.VerifyEmbeddedStream)
	return err
}

// attestationFingerprint is extractAttestationFingerprint for a parsed header.
func attestationFingerprint(m *backup.Manifest) string {
	if m == nil || m.Attestation == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(m.Attestation.PublicKey))
	return fmt.Sprintf("%x", sum[:8])
}

// installManifestFromReader is installManifestOverwrite for a streamed
// body of exactly size bytes.
func installManifestFromReader(ctx context.Context, sp storage.StoragePlugin, key string, size int64, r io.Reader, retainUntil time.Time, mode storage.WORMMode) error {
	if _, err := sp.Put(ctx, key, r, storage.PutOptions{ContentLength: size}); err != nil {
		return fmt.Errorf("install %q: %w", key, err)
	}
	if !retainUntil.IsZero() {
		m := mode
		if m == "" {
			m = storage.WORMCompliance
		}
		if err := sp.SetRetention(ctx, key, retainUntil, m); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			return fmt.Errorf("lock %q: %w", key, err)
		}
	}
	return nil
}

// installManifestCopy overwrites dstKey with the bytes at srcKey, streamed.
// wantSHA is the digest of the source as the caller verified it; the copy
// fails (and the destination write is aborted at EOF, before the backend
// commits it) if the source no longer hashes to it.
func installManifestCopy(ctx context.Context, sp storage.StoragePlugin, dstKey, srcKey string, wantSHA [32]byte, retainUntil time.Time, mode storage.WORMMode) error {
	st, err := sp.Stat(ctx, srcKey)
	if err != nil {
		return err
	}
	rc, err := sp.Get(ctx, srcKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	return installManifestFromReader(ctx, sp, dstKey, st.Size, &shaCheckedReader{r: rc, h: sha256.New(), want: wantSHA}, retainUntil, mode)
}

// shaCheckedReader fails the read at EOF when the bytes it delivered do
// not hash to want, so an upload fed by it is never completed with
// unverified content.
type shaCheckedReader struct {
	r io.Reader
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
	want [32]byte
}

func (c *shaCheckedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	_, _ = c.h.Write(p[:n])
	if errors.Is(err, io.EOF) {
		if subtle.ConstantTimeCompare(c.h.Sum(nil), c.want[:]) != 1 {
			return n, errors.New("manifest changed after it was verified; refusing to install unverified bytes")
		}
	}
	return n, err
}

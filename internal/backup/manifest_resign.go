package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// validateAndSign checks hdr (with the spool's entries) against the
// manifest invariants, then assembles and signs the canonical document on
// disk. The caller owns the returned file (close + remove it).
func (s *ManifestSpool) validateAndSign(hdr *Manifest, signer *Signer) (*os.File, int64, error) {
	if err := s.ValidateManifest(hdr); err != nil {
		return nil, 0, fmt.Errorf("manifest invalid: %w", err)
	}
	if err := s.w.Flush(); err != nil {
		return nil, 0, err
	}
	if err := s.fileSet.Err(); err != nil {
		return nil, 0, err
	}
	return s.assembleAndSign(hdr, signer)
}

// ResignedManifest is a manifest document re-signed from a streamed
// source and staged in a temp file, so a manifest far larger than memory
// can be re-signed (`repair attestation`).
type ResignedManifest struct {
	// Header is the re-signed manifest's header (Files == nil) with the
	// new Attestation.
	Header *Manifest
	// SourceSHA256 is the SHA-256 of the exact source bytes read; a
	// caller that verified the source in an earlier pass compares it to
	// detect a swap in between.
	SourceSHA256 [32]byte

	canon *os.File
	body  manifestBody
}

// Size is the byte length of the re-signed document.
func (r *ResignedManifest) Size() int64 { return r.body.size }

// Open returns a fresh reader of the whole re-signed document.
func (r *ResignedManifest) Open() (io.ReadCloser, error) { return r.body.open() }

// Close removes the staging file.
func (r *ResignedManifest) Close() {
	if r.canon != nil {
		_ = r.canon.Close()
		_ = os.Remove(r.canon.Name())
		r.canon = nil
	}
}

// ResignStreamed reads one manifest document from src in a single
// streaming pass and stages a copy re-signed by signer. Memory is
// independent of the manifest size. The source is NOT verified here: the
// caller decides whether it is trusted (and, with SourceSHA256, that it is
// the same bytes it verified).
func ResignStreamed(src io.Reader, signer *Signer) (*ResignedManifest, error) {
	if signer == nil {
		return nil, errors.New("backup: ResignStreamed needs a Signer")
	}
	spool, err := NewManifestSpool("", 0)
	if err != nil {
		return nil, err
	}
	defer spool.Close()
	info, err := StreamManifest(src, StreamOptions{}, spool.Add)
	if err != nil {
		return nil, fmt.Errorf("copy entries of streamed manifest: %w", err)
	}
	hdr := *info.Manifest
	hdr.stream, hdr.Files, hdr.Attestation = nil, nil, nil
	canon, size, err := spool.validateAndSign(&hdr, signer)
	if err != nil {
		return nil, err
	}
	body, err := spooledManifestBody(canon, size, hdr.Attestation)
	if err != nil {
		_ = canon.Close()
		_ = os.Remove(canon.Name())
		return nil, err
	}
	return &ResignedManifest{Header: &hdr, SourceSHA256: info.RawSHA256, canon: canon, body: body}, nil
}

// VerifyStream reports whether the manifest document read from r carries
// a valid signature under v (ParseAndVerify's verdict), in one streaming
// pass. The returned header (Files == nil) is non-nil only on success.
func VerifyStream(r io.Reader, v *Verifier) (*StreamInfo, error) {
	return StreamManifest(r, StreamOptions{Verifier: v}, nil)
}

// ParseHeaderStream decodes just the header of the manifest document read
// from r, without verifying anything (ParseAttestationless' verdict).
func ParseHeaderStream(r io.Reader) (*StreamInfo, error) {
	return StreamManifest(r, StreamOptions{}, nil)
}

// VerifyEmbeddedStream is VerifyEmbedded for a streamed document: the
// signature must hold under the key embedded in the manifest itself.
func VerifyEmbeddedStream(r io.Reader) (*StreamInfo, error) {
	return StreamManifest(r, StreamOptions{VerifyEmbedded: true}, nil)
}

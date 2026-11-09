// bundle_spool.go — disk-backed building blocks of ExportBundle.
package audit

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// maxHeldBundleSignBytes bounds the signing input on platforms without
// mmap (windows), where it has to be held in RAM. VerifyBundle refuses a
// bundle that decompresses to more than MaxBundleBytes, so nothing a
// verifier would accept is refused here.
// A variable so tests can lower it.
var maxHeldBundleSignBytes = MaxBundleBytes

// bundleSpool is a bundle's canonical signing input — the exact bytes
// canonicalBundleBytes returns — written to a temp file instead of built
// as a []byte.
//
// Ed25519 needs the whole message addressable, and the message contains
// every event of the window, so a []byte made the export's memory grow
// with the window twice over (the NDJSON body, then the canonical copy
// of it). On disk the same bytes are memory-mapped for signing and then
// read back, member by member, into the archive: the canonical form
// already holds every signed file's body verbatim, so no second spool of
// events.ndjson is needed.
type bundleSpool struct {
	f        *os.File
	w        *bufio.Writer
	size     int64 // bytes written so far
	declared int   // file count written in the header
	members  []spoolMember
	scratch  []byte
}

// spoolMember locates one signed file's body inside the spool.
type spoolMember struct {
	name string
	off  int64
	size int64
}

// newBundleSpool creates the spool in dir ("" = os.TempDir()) for a
// bundle of fileCount signed files. The caller must Close it.
func newBundleSpool(dir string, fileCount int) (*bundleSpool, error) {
	f, err := os.CreateTemp(dir, "pghs-audit-bundle-*.canon")
	if err != nil {
		return nil, err
	}
	s := &bundleSpool{f: f, w: bufio.NewWriterSize(f, 1<<20), declared: fileCount}
	s.frame(appendU64BE(nil, uint64(fileCount)))
	return s, nil
}

// frame writes canonical framing bytes. bufio.Writer errors are sticky:
// seal's Flush reports any of them.
func (s *bundleSpool) frame(p []byte) {
	n, _ := s.w.Write(p)
	s.size += int64(n)
}

// begin starts the next signed file; its body is everything written
// until the next begin (or seal).
//
// The canonical form puts each body's length BEFORE the body, and a
// streamed body's length is only known once it has been written, so a
// zero is written here and seal fills in the real value.
func (s *bundleSpool) begin(name string) {
	hdr := appendU32BE(s.scratch[:0], uint32(len(name)))
	hdr = append(hdr, name...)
	hdr = appendU64BE(hdr, 0)
	s.frame(hdr)
	s.scratch = hdr
	s.members = append(s.members, spoolMember{name: name, off: s.size})
}

// Write appends to the body of the file begun last.
func (s *bundleSpool) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.size += int64(n)
	s.members[len(s.members)-1].size += int64(n)
	return n, err
}

// add appends a signed file whose body is already in memory.
func (s *bundleSpool) add(name string, body []byte) {
	s.begin(name)
	_, _ = s.Write(body) // sticky, see frame
}

// seal completes the canonical form: everything buffered reaches the
// file and every body length is patched into its slot. After seal the
// spool is read-only.
func (s *bundleSpool) seal() error {
	if len(s.members) != s.declared {
		return fmt.Errorf("canonical bundle declares %d files but holds %d", s.declared, len(s.members))
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	for _, m := range s.members {
		if _, err := s.f.WriteAt(appendU64BE(nil, uint64(m.size)), m.off-8); err != nil {
			return err
		}
	}
	return nil
}

// sign returns the signer's signature over the sealed canonical form,
// memory-mapped (held in RAM where mmap is unavailable).
func (s *bundleSpool) sign(signer EventSigner) ([]byte, error) {
	msg, release, err := spill.MapReadOnly(s.f, s.size, maxHeldBundleSignBytes)
	if errors.Is(err, spill.ErrTooLargeToHold) {
		return nil, fmt.Errorf("bundle signing input is %d bytes, over the %d-byte limit of "+
			"platforms without mmap: an Ed25519 signature covers the whole message and cannot be "+
			"computed in a stream; narrow the window or export on a unix host", s.size, maxHeldBundleSignBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("map canonical bundle: %w", err)
	}
	defer func() { _ = release() }()
	return signer.Sign(msg), nil
}

// body returns a reader over one signed file's bytes.
func (s *bundleSpool) body(m spoolMember) io.Reader {
	return io.NewSectionReader(s.f, m.off, m.size)
}

// Close removes the spool file. Safe to call more than once.
func (s *bundleSpool) Close() error {
	if s.f == nil {
		return nil
	}
	name := s.f.Name()
	_ = s.f.Close()
	s.f = nil
	return os.Remove(name)
}

// writeEventsNDJSON streams the matched events into w, one JSON document
// per line in Search order, and returns their number with the first and
// last of them (nil when there are none): the two the chain proof and
// the bundle's head record need.
func writeEventsNDJSON(ctx context.Context, m *sortedMatches, w io.Writer) (n int, first, last *Event, err error) {
	var firstLine, lastLine []byte
	nl := []byte{'\n'}
	err = m.each(ctx, func(line []byte) error {
		if n == 0 {
			firstLine = append(firstLine, line...)
		}
		// Only the final line is needed, but which one is final is not
		// known until the stream ends; a reused buffer keeps this a copy
		// per event rather than an allocation.
		lastLine = append(lastLine[:0], line...)
		n++
		if _, err := w.Write(line); err != nil {
			return err
		}
		_, err := w.Write(nl)
		return err
	})
	if err != nil || n == 0 {
		return n, nil, nil, err
	}
	first, last = new(Event), new(Event)
	if err := json.Unmarshal(firstLine, first); err != nil {
		return n, nil, nil, err
	}
	if err := json.Unmarshal(lastLine, last); err != nil {
		return n, nil, nil, err
	}
	return n, first, last, nil
}

// writeAnchorsNDJSON streams every anchor whose AnchoredAt falls within
// the filter window into w, one JSON document per line in storage-key
// order, and returns their number.
//
// The keys are ordered by an external sorter rather than a sorted slice
// for the same reason the events are: the listing is not ordered on every
// backend, and an anchor history is as unbounded as the chain it
// witnesses.
func writeAnchorsNDJSON(ctx context.Context, sp storage.StoragePlugin, f ListFilters, spoolDir string, w io.Writer) (int, error) {
	sorter := spill.NewSorter(spill.SorterOptions{MemBytes: searchSortMemBytes, Dir: spoolDir})
	defer sorter.Close()
	for info, err := range sp.List(ctx, AnchorPrefix) {
		if err != nil {
			return 0, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		// An unreadable or undecodable anchor is skipped, as it always
		// has been. A cancelled context must not be mistaken for one:
		// every remaining Get would fail, be skipped, and the walk would
		// end "successfully" with the anchors missing.
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rc, err := sp.Get(ctx, info.Key)
		if err != nil {
			continue
		}
		body, err := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
		_ = rc.Close()
		if err != nil {
			continue
		}
		var a Anchor
		if err := json.Unmarshal(body, &a); err != nil {
			continue
		}
		if !f.Since.IsZero() && a.AnchoredAt.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !a.AnchoredAt.Before(f.Until) {
			continue
		}
		line, err := json.Marshal(&a)
		if err != nil {
			return 0, err
		}
		if err := sorter.Add([]byte(info.Key), line); err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	it, err := sorter.Iter()
	if err != nil {
		return 0, err
	}
	defer it.Close()
	n := 0
	nl := []byte{'\n'}
	for {
		_, line, ok, err := it.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			return n, nil
		}
		if _, err := w.Write(line); err != nil {
			return n, err
		}
		if _, err := w.Write(nl); err != nil {
			return n, err
		}
		n++
	}
}

// writeBundleArchive writes the gzipped tar archive to w — the signed
// files in spool order, then signature.sig — and returns the SHA-256 of
// the gzipped bytes (for the operator's record) with their count.
//
// The tar's mtime fields are set to `now` (single value across
// every file) so a re-build of the same input yields the same
// bytes — useful for reproducible bundles.
func writeBundleArchive(ctx context.Context, w io.Writer, spool *bundleSpool, signature []byte, now time.Time) (string, int64, error) {
	hasher := sha256.New()
	mw := io.MultiWriter(w, hasher)
	counter := &writeCounter{w: mw}
	gw := gzip.NewWriter(counter)
	tw := tar.NewWriter(gw)

	member := func(name string, size int64, body io.Reader) error {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    size,
			ModTime: now,
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := io.Copy(tw, body)
		return err
	}
	for _, m := range spool.members {
		if err := member(m.name, m.size, ctxReader{ctx: ctx, r: spool.body(m)}); err != nil {
			return "", 0, err
		}
	}
	if err := member("signature.sig", int64(len(signature)), bytes.NewReader(signature)); err != nil {
		return "", 0, err
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gw.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), counter.n, nil
}

// ctxReader fails a read once ctx is done, so copying a large spooled
// member into the archive stops at the next buffer instead of running
// to the end of a cancelled export.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

// Read reads from the wrapped reader unless ctx is done.
func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

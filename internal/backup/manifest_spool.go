package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"os"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// ManifestSpool accumulates the file entries of a manifest on disk as
// they are produced, so a backup of a 200 TB database never holds its
// own ~380 GB file list in memory. Each entry is validated and
// de-duplicated on the way in; CommitSpooled then assembles, signs and
// publishes the same document Commit would have written.
type ManifestSpool struct {
	f       *os.File
	w       *bufio.Writer
	fileSet *spill.HashSet
	enc     bytes.Buffer

	files, bytes, chunkRefs int64
	path                    string

	// chunkSet counts distinct chunks so a backup's unique-chunk figures
	// need no second pass (and no map the size of the backup).
	chunkSet                  *spill.HashSet
	uniqueChunks, uniqueBytes int64
}

// NewManifestSpool creates a spool whose temp files live in dir
// (empty = os.TempDir()). memEntries bounds the heap used by the
// path-uniqueness set (0 = spill.DefaultMemEntries).
func NewManifestSpool(dir string, memEntries int) (*ManifestSpool, error) {
	f, err := os.CreateTemp(dir, "pghs-manifest-*.entries")
	if err != nil {
		return nil, fmt.Errorf("manifest spool: %w", err)
	}
	return &ManifestSpool{
		f: f, w: bufio.NewWriterSize(f, 1<<20), path: f.Name(),
		fileSet:  spill.NewHashSet(spill.Options{Dir: dir, MemEntries: memEntries}),
		chunkSet: spill.NewHashSet(spill.Options{Dir: dir, MemEntries: memEntries}),
	}, nil
}

// Add validates fe and appends it. The entry is not retained.
func (s *ManifestSpool) Add(fe *FileEntry) error {
	if err := validateFileEntry(int(s.files), fe); err != nil {
		return err
	}
	if !s.fileSet.Add(sha256.Sum256([]byte(fileIdentity(fe)))) {
		// A damaged spill file answers "present" (fail closed); that is an
		// I/O failure, not a duplicate path, and must be reported as one.
		if err := s.fileSet.Err(); err != nil {
			return fmt.Errorf("manifest spool: path set: %w", err)
		}
		return fmt.Errorf("manifest: duplicate file path %q in tablespace %d", fe.Path, fe.TablespaceOID)
	}
	s.enc.Reset()
	e := json.NewEncoder(&s.enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(fe); err != nil {
		return fmt.Errorf("manifest spool: encode %q: %w", fe.Path, err)
	}
	if s.files > 0 {
		if err := s.w.WriteByte(','); err != nil {
			return err
		}
	}
	if _, err := s.w.Write(bytes.TrimSuffix(s.enc.Bytes(), []byte("\n"))); err != nil {
		return err
	}
	s.files++
	s.bytes += fe.Size
	s.chunkRefs += int64(len(fe.Chunks))
	for _, c := range fe.Chunks {
		if s.chunkSet.Add(c.Hash) {
			s.uniqueChunks++
			s.uniqueBytes += c.Len
		}
	}
	return nil
}

// UniqueChunks and UniqueChunkBytes count each distinct chunk once.
func (s *ManifestSpool) UniqueChunks() int64     { return s.uniqueChunks }
func (s *ManifestSpool) UniqueChunkBytes() int64 { return s.uniqueBytes }

// HasFile reports whether a file with this tablespace OID and path was
// added. Used for the essential-files gate before commit.
func (s *ManifestSpool) HasFile(tablespaceOID uint32, path string) bool {
	return s.fileSet.Has(sha256.Sum256([]byte(fileIdentity(&FileEntry{Path: path, TablespaceOID: tablespaceOID}))))
}

// ValidateManifest applies the manifest-level invariants (header,
// directories, encryption) to m, a header-only manifest whose file
// entries are in this spool; the per-entry and uniqueness checks ran in
// Add. It is Validate for a manifest that does not hold its files.
func (s *ManifestSpool) ValidateManifest(m *Manifest) error {
	if err := m.validateHeader(); err != nil {
		return err
	}
	return m.validateTail(func(key string) bool { return s.fileSet.Has(sha256.Sum256([]byte(key))) })
}

// NumFiles, TotalFileBytes and NumChunkRefs are the running totals.
func (s *ManifestSpool) NumFiles() int64       { return s.files }
func (s *ManifestSpool) TotalFileBytes() int64 { return s.bytes }
func (s *ManifestSpool) NumChunkRefs() int64   { return s.chunkRefs }

// Close removes the spool's temp files. Safe to call more than once.
func (s *ManifestSpool) Close() error {
	if s.f == nil {
		return nil
	}
	_ = s.f.Close()
	_ = os.Remove(s.path)
	s.f = nil
	_ = s.chunkSet.Close()
	return s.fileSet.Close()
}

// CommitSpooled is Commit for a manifest whose file entries live in sp.
// m carries every field except Files (which must be empty). It runs the
// same gates as Commit — reference and header/tail invariants, the
// per-entry and uniqueness checks already applied by sp.Add — signs the
// canonical document with signer, and publishes it through the same
// primary/replica/chain-safety path. The signature is computed over a
// memory-mapped canonical spool, so signing needs no more RAM than the
// manifest is small.
//
// m is left holding its header and attestation; its Files stay nil.
func (ms *ManifestStore) CommitSpooled(ctx context.Context, m *Manifest, sp *ManifestSpool, signer *Signer, opts CommitOptions) error {
	if m == nil || sp == nil || sp.f == nil {
		return errors.New("backup: CommitSpooled needs a manifest and an open spool")
	}
	if len(m.Files) != 0 || m.stream != nil {
		return errors.New("backup: CommitSpooled: Manifest.Files must be empty; the entries are in the spool")
	}
	if m.Attestation != nil {
		return errors.New("backup: CommitSpooled signs the manifest itself; Manifest.Attestation must be nil")
	}
	if signer == nil {
		return errors.New("backup: CommitSpooled needs a Signer")
	}
	if m.Deployment == "" || m.BackupID == "" {
		return errors.New("backup: Commit requires Manifest.Deployment and Manifest.BackupID")
	}
	if err := validateRef(m.Deployment, m.BackupID); err != nil {
		return err
	}
	if err := sp.w.Flush(); err != nil {
		return fmt.Errorf("backup: flush manifest spool: %w", err)
	}
	if err := sp.fileSet.Err(); err != nil {
		return fmt.Errorf("backup: manifest path set: %w", err)
	}
	if err := sp.chunkSet.Err(); err != nil {
		return fmt.Errorf("backup: manifest unique-chunk counters: %w", err)
	}
	if err := m.validateHeader(); err != nil {
		return fmt.Errorf("backup: Commit refuses invalid manifest %s: %w", m.BackupID, err)
	}
	if err := m.validateTail(func(key string) bool { return sp.fileSet.Has(sha256.Sum256([]byte(key))) }); err != nil {
		return fmt.Errorf("backup: Commit refuses invalid manifest %s: %w", m.BackupID, err)
	}

	prefix, suffix, err := canonicalSplit(m)
	if err != nil {
		return err
	}
	canon, err := os.CreateTemp(sp.dir(), "pghs-manifest-*.canon")
	if err != nil {
		return fmt.Errorf("backup: canonical spool: %w", err)
	}
	defer func() { _ = canon.Close(); _ = os.Remove(canon.Name()) }()
	// bufio.Writer errors are sticky: the Flush below reports any of them.
	w := bufio.NewWriterSize(canon, 1<<20)
	_, _ = w.Write(prefix)
	if sp.files == 0 {
		_, _ = w.WriteString("[]")
	} else {
		_ = w.WriteByte('[')
		if _, err := sp.f.Seek(0, stdio.SeekStart); err != nil {
			return err
		}
		if _, err := stdio.Copy(w, sp.f); err != nil {
			return fmt.Errorf("backup: assemble canonical manifest: %w", err)
		}
		_ = w.WriteByte(']')
	}
	_, _ = w.Write(suffix)
	if err := w.Flush(); err != nil {
		return fmt.Errorf("backup: assemble canonical manifest: %w", err)
	}
	st, err := canon.Stat()
	if err != nil {
		return err
	}
	msg, unmap, err := mapReadOnly(canon, st.Size())
	if err != nil {
		return fmt.Errorf("backup: map canonical manifest: %w", err)
	}
	pubPEM, perr := signer.PublicKeyPEM()
	if perr != nil {
		_ = unmap()
		return perr
	}
	sig := signer.Sign(msg)
	_ = unmap()
	m.Attestation = &Attestation{
		Scheme:    SchemeEd25519,
		PublicKey: string(pubPEM),
		Signature: base64.StdEncoding.EncodeToString(sig),
	}

	// On-disk form = canonical bytes with the attestation appended as the
	// last field, exactly what MarshalToBytes produces.
	var att bytes.Buffer
	ae := json.NewEncoder(&att)
	ae.SetEscapeHTML(false)
	if err := ae.Encode(m.Attestation); err != nil {
		return err
	}
	tail := `,"attestation":` + strings.TrimSuffix(att.String(), "\n") + "}"
	head := st.Size() - 1 // everything but the closing '}'
	body := manifestBody{
		size: head + int64(len(tail)),
		open: func() (stdio.ReadCloser, error) {
			return &spoolBody{head: stdio.NewSectionReader(canon, 0, head), headLen: head, tail: strings.NewReader(tail)}, nil
		},
	}
	return ms.commitBody(ctx, m, body, opts)
}

func (s *ManifestSpool) dir() string {
	if s.f == nil {
		return ""
	}
	i := strings.LastIndexByte(s.path, os.PathSeparator)
	if i < 0 {
		return ""
	}
	return s.path[:i]
}

// spoolBody is the on-disk canonical bytes followed by the attestation
// tail, readable and seekable as one stream (object-store clients rewind
// the body to sign it).
type spoolBody struct {
	head    *stdio.SectionReader
	headLen int64
	tail    *strings.Reader
	pos     int64
}

func (b *spoolBody) Read(p []byte) (int, error) {
	if b.pos < b.headLen {
		n, err := b.head.Read(p)
		b.pos += int64(n)
		if err == stdio.EOF && b.pos >= b.headLen {
			err = nil
		}
		return n, err
	}
	n, err := b.tail.Read(p)
	b.pos += int64(n)
	return n, err
}

func (b *spoolBody) Seek(off int64, whence int) (int64, error) {
	total := b.headLen + b.tail.Size()
	var abs int64
	switch whence {
	case stdio.SeekStart:
		abs = off
	case stdio.SeekCurrent:
		abs = b.pos + off
	case stdio.SeekEnd:
		abs = total + off
	default:
		return 0, errors.New("spoolBody: bad whence")
	}
	if abs < 0 {
		return 0, errors.New("spoolBody: negative position")
	}
	if abs > total {
		abs = total
	}
	if _, err := b.head.Seek(minInt64(abs, b.headLen), stdio.SeekStart); err != nil {
		return 0, err
	}
	if _, err := b.tail.Seek(maxInt64(abs-b.headLen, 0), stdio.SeekStart); err != nil {
		return 0, err
	}
	b.pos = abs
	return abs, nil
}

func (b *spoolBody) Close() error { return nil }

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

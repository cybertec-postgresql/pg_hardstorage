// manifest_stream.go — constant-memory reading and verification of a
// manifest document.
//
// A 200 TB database at the default 64 KiB average chunk size has about
// 3.3 billion chunks, so its manifest is hundreds of GB. ParseAndVerify
// and friends hold the document, the parsed struct and the re-serialised
// canonical form in memory at once; StreamManifest holds one file entry.
// The on-disk format is unchanged: it reads exactly what Commit writes.
package backup

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// MaxStreamedManifestBytes bounds how much of one manifest StreamManifest
// consumes (16 TiB, ~40x a 200 TB database's manifest). It limits how
// long a corrupt or hostile object can occupy a reader, not memory.
const MaxStreamedManifestBytes int64 = 16 << 40

// MaxStreamedFileEntryBytes bounds ONE file entry. Entries are decoded
// whole, so this is the real memory bound of the streaming reader. A
// PostgreSQL relation segment is 1 GiB = 16K chunks = ~2 MB of JSON.
const MaxStreamedFileEntryBytes = 256 << 20

// StreamOptions selects what StreamManifest checks.
type StreamOptions struct {
	// Verifier, when non-nil, requires an attestation and verifies it
	// under this key (ParseAndVerify semantics: the embedded key must
	// match the verifier's). Entries are delivered to the callback BEFORE
	// the signature is checked — it can only be checked after the last
	// byte — so a callback must not act on an entry until StreamManifest
	// returns nil, or must have verified in an earlier pass.
	Verifier *Verifier

	// VerifyEmbedded verifies the signature under the key embedded in
	// the manifest (VerifyEmbedded semantics). Ignored if Verifier is set.
	VerifyEmbedded bool

	// Validate applies the same invariants as Manifest.Validate, one
	// entry at a time.
	Validate bool

	// PathSetMemEntries is the heap budget of the path-uniqueness set
	// used by Validate (0 = spill.DefaultMemEntries).
	PathSetMemEntries int

	// SpoolDir is where the canonical form and the path-uniqueness set
	// are spilled while verifying/validating. Empty = os.TempDir().
	SpoolDir string
}

// StreamInfo is what a completed stream learned about the document.
type StreamInfo struct {
	// Manifest holds every field except Files (nil).
	Manifest *Manifest
	// Files is the number of file entries; Bytes their summed Size;
	// ChunkRefs the summed chunk-reference count.
	Files     int64
	Bytes     int64
	ChunkRefs int64
	// RawSHA256 is the SHA-256 of the exact bytes read. A second pass
	// over the same object compares it to detect a swap in between.
	RawSHA256 [32]byte
	// Verified is true when a signature was checked and held.
	Verified bool
	// Key is the storage key the document was read from (set by the
	// ManifestStore readers, empty for a bare StreamManifest).
	Key string
}

// StreamManifest reads one manifest document from r, calling fn (if
// non-nil) for every file entry in document order. The entry is only
// valid during the call. See StreamOptions.Verifier for the ordering
// caveat with signatures.
func StreamManifest(r io.Reader, opts StreamOptions, fn func(*FileEntry) error) (*StreamInfo, error) {
	h := sha256.New()
	lr := io.LimitReader(io.TeeReader(r, h), MaxStreamedManifestBytes)
	dec := json.NewDecoder(lr)

	sw := &streamState{opts: opts, fn: fn}
	defer sw.cleanup()
	if opts.Validate {
		sw.fileSet = spill.NewHashSet(spill.Options{Dir: opts.SpoolDir, MemEntries: opts.PathSetMemEntries})
	}
	verify := opts.Verifier != nil || opts.VerifyEmbedded

	if tok, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("manifest: parse: document is not a JSON object")
	}

	fields := map[string]json.RawMessage{}
	seen := map[string]string{}
	var spoolOpen bool
	var p0 []byte
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("manifest: parse: %w", err)
		}
		key, _ := kt.(string)
		if !isSchemaKey(key) {
			return nil, fmt.Errorf("%w: object $ has key %q, which is not lowercase-ASCII "+
				"snake_case; encoding/json would still bind it to a field by case/Unicode "+
				"folding while other readers would not, so the file does not have one "+
				"meaning and must not be trusted or re-signed", ErrAmbiguousManifest, key)
		}
		folded := jsonFoldKey(key)
		if prev, dup := seen[folded]; dup {
			return nil, fmt.Errorf("%w: object $ repeats key %q (also present as %q); "+
				"this decoder binds the last occurrence and other JSON readers bind the "+
				"first, so the file does not have one meaning and must not be trusted "+
				"or re-signed", ErrAmbiguousManifest, key, prev)
		}
		seen[folded] = key

		if folded != foldedFilesKey {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, fmt.Errorf("manifest: parse: %w", err)
			}
			if err := rejectDuplicateKeys(raw); err != nil {
				return nil, err
			}
			fields[key] = raw
			continue
		}

		// files: stream the array.
		sw.sawFiles = true
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("manifest: parse: %w", err)
		}
		if tok == nil {
			sw.filesNull = true
			continue
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return nil, errors.New("manifest: parse: files is not an array")
		}
		if verify {
			// Optimistic prefix: the canonical bytes before the files
			// array, from the fields seen so far. Right whenever the
			// document is in canonical field order, which Commit writes.
			hdr, herr := buildHeader(fields)
			if herr != nil {
				return nil, herr
			}
			pre, _, perr := canonicalSplit(hdr)
			if perr != nil {
				return nil, perr
			}
			p0 = pre
			if err := sw.openSpool(pre); err != nil {
				return nil, err
			}
			spoolOpen = true
			if _, err := sw.spoolW.WriteString("["); err != nil {
				return nil, err
			}
		}
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, fmt.Errorf("manifest: parse: %w", err)
			}
			if len(raw) > MaxStreamedFileEntryBytes {
				return nil, fmt.Errorf("manifest: files[%d] is %d bytes, over the %d-byte per-entry cap", sw.info.Files, len(raw), MaxStreamedFileEntryBytes)
			}
			if err := sw.entry(raw, verify); err != nil {
				return nil, err
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			return nil, fmt.Errorf("manifest: parse: %w", err)
		}
	}
	if _, err := dec.Token(); err != nil { // '}'
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}

	m, err := buildHeader(fields)
	if err != nil {
		return nil, err
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("manifest: schema %q is not supported; expected %q", m.Schema, Schema)
	}
	if opts.Validate {
		if err := m.validateHeader(); err != nil {
			return nil, err
		}
		if err := sw.fileSet.Err(); err != nil {
			return nil, fmt.Errorf("manifest: path-uniqueness set: %w", err)
		}
		if err := m.validateTail(func(key string) bool { return sw.fileSet.Has(sha256.Sum256([]byte(key))) }); err != nil {
			return nil, err
		}
	}
	sw.info.Manifest = m
	sw.info.RawSHA256 = [32]byte(h.Sum(nil))

	if verify {
		if err := sw.verify(m, p0, spoolOpen); err != nil {
			return nil, err
		}
		sw.info.Verified = true
	}
	info := sw.info
	return &info, nil
}

type streamState struct {
	opts StreamOptions
	fn   func(*FileEntry) error
	info StreamInfo

	sawFiles, filesNull bool
	fileSet             *spill.HashSet

	spool  *os.File
	spoolW *bufio.Writer
	first  bool
	enc    bytes.Buffer
}

func (s *streamState) cleanup() {
	if s.fileSet != nil {
		_ = s.fileSet.Close()
	}
	if s.spool != nil {
		name := s.spool.Name()
		_ = s.spool.Close()
		_ = os.Remove(name)
	}
}

func (s *streamState) openSpool(prefix []byte) error {
	f, err := os.CreateTemp(s.opts.SpoolDir, "pghs-manifest-*.canon")
	if err != nil {
		return fmt.Errorf("manifest: open canonical spool: %w", err)
	}
	s.spool = f
	s.spoolW = bufio.NewWriterSize(f, 1<<20)
	s.first = true
	_, err = s.spoolW.Write(prefix)
	return err
}

// entry handles one raw file entry.
func (s *streamState) entry(raw json.RawMessage, verify bool) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	var fe FileEntry
	if err := json.Unmarshal(raw, &fe); err != nil {
		return fmt.Errorf("manifest: parse files[%d]: %w", s.info.Files, err)
	}
	if s.opts.Validate {
		if err := validateFileEntry(int(s.info.Files), &fe); err != nil {
			return err
		}
		if !s.fileSet.Add(sha256.Sum256([]byte(fileIdentity(&fe)))) {
			return fmt.Errorf("manifest: duplicate file path %q in tablespace %d", fe.Path, fe.TablespaceOID)
		}
	}
	if verify {
		if !s.first {
			if err := s.spoolW.WriteByte(','); err != nil {
				return err
			}
		}
		s.first = false
		s.enc.Reset()
		e := json.NewEncoder(&s.enc)
		e.SetEscapeHTML(false)
		if err := e.Encode(&fe); err != nil {
			return fmt.Errorf("manifest: canonicalize files[%d]: %w", s.info.Files, err)
		}
		if _, err := s.spoolW.Write(bytes.TrimSuffix(s.enc.Bytes(), []byte("\n"))); err != nil {
			return err
		}
	}
	s.info.Files++
	s.info.Bytes += fe.Size
	s.info.ChunkRefs += int64(len(fe.Chunks))
	if s.fn != nil {
		return s.fn(&fe)
	}
	return nil
}

// verify finishes the canonical message and checks the signature.
func (s *streamState) verify(m *Manifest, p0 []byte, spoolOpen bool) error {
	if m.Attestation == nil {
		return ErrUnsigned
	}
	if m.Attestation.Scheme != SchemeEd25519 {
		return fmt.Errorf("manifest: unsupported signature scheme %q", m.Attestation.Scheme)
	}
	var v *Verifier
	if s.opts.Verifier != nil {
		v = s.opts.Verifier
		if err := matchPublicKeys(m.Attestation.PublicKey, v); err != nil {
			return err
		}
	} else {
		emb, err := LoadVerifier([]byte(m.Attestation.PublicKey))
		if err != nil {
			return fmt.Errorf("manifest: parse embedded public key: %w", err)
		}
		v = emb
	}
	sig, err := base64.StdEncoding.DecodeString(m.Attestation.Signature)
	if err != nil {
		return fmt.Errorf("manifest: decode signature: %w", err)
	}
	prefix, suffix, err := canonicalSplit(m)
	if err != nil {
		return err
	}

	var f *os.File
	switch {
	case !spoolOpen:
		// No (or null) files array: the whole message is small.
		body := "null"
		if s.sawFiles && !s.filesNull {
			body = "[]"
		}
		msg := append(append(append([]byte{}, prefix...), body...), suffix...)
		if err := v.Verify(msg, sig); err != nil {
			return s.sigErr(m, err)
		}
		return nil
	case bytes.Equal(prefix, p0):
		if _, err := s.spoolW.WriteString("]"); err != nil {
			return err
		}
		if _, err := s.spoolW.Write(suffix); err != nil {
			return err
		}
		if err := s.spoolW.Flush(); err != nil {
			return err
		}
		f = s.spool
	default:
		// Header fields followed the files array (non-canonical field
		// order): rebuild the message with the right prefix. Rare — the
		// writer always emits canonical order — and costs one extra copy.
		if err := s.spoolW.Flush(); err != nil {
			return err
		}
		g, err := os.CreateTemp(s.opts.SpoolDir, "pghs-manifest-*.canon2")
		if err != nil {
			return err
		}
		defer func() { name := g.Name(); _ = g.Close(); _ = os.Remove(name) }()
		w := bufio.NewWriterSize(g, 1<<20)
		w.Write(prefix)
		if _, err := s.spool.Seek(int64(len(p0)), io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(w, s.spool); err != nil {
			return err
		}
		// The copy carried "[entries" and the first-pass "]"+suffix was
		// never written (we flushed before writing them).
		w.WriteString("]")
		w.Write(suffix)
		if err := w.Flush(); err != nil {
			return err
		}
		f = g
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	msg, unmap, err := mapReadOnly(f, st.Size())
	if err != nil {
		return fmt.Errorf("manifest: map canonical form: %w", err)
	}
	defer unmap()
	if err := v.Verify(msg, sig); err != nil {
		return s.sigErr(m, err)
	}
	return nil
}

func (s *streamState) sigErr(m *Manifest, err error) error {
	if s.opts.Verifier == nil {
		return fmt.Errorf("manifest %s: signature does not match its embedded key (content tampered after signing): %w", m.BackupID, err)
	}
	return fmt.Errorf("manifest %s: %w", m.BackupID, err)
}

// buildHeader decodes every non-files field into a Manifest.
func buildHeader(fields map[string]json.RawMessage) (*Manifest, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for k, v := range fields {
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	var m Manifest
	if err := json.Unmarshal(b.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	return &m, nil
}

// canonicalSplit returns the canonical bytes before and after the files
// array's value. The full canonical message is
// prefix + <files value> + suffix, where the value is "[e1,e2,...]" or
// "null".
func canonicalSplit(m *Manifest) (prefix, suffix []byte, err error) {
	cp := *m
	cp.Attestation = nil
	cp.Files = []FileEntry{}
	var buf canonicalBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&cp); err != nil {
		return nil, nil, fmt.Errorf("manifest: canonicalize: %w", err)
	}
	b := buf.TrimTrailingNewline()
	marker := []byte(`"files":[]`)
	i := bytes.Index(b, marker)
	if i < 0 || i != bytes.LastIndex(b, marker) {
		return nil, nil, errors.New("manifest: canonical form has no unique files marker")
	}
	cut := i + len(`"files":`)
	return b[:cut:cut], b[i+len(marker):], nil
}

// foldedFilesKey is "files" under the same folding rejectDuplicateKeys uses.
var foldedFilesKey = jsonFoldKey("files")

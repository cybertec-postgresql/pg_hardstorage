// manifeststream.go — constant-memory chunk-reference extraction from a
// backup manifest.
package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxStreamedMetadataBytes bounds how much of one manifest a streaming
// walker will consume. Streaming holds one chunk reference in memory at
// a time, so unlike MaxMetadataBytes (which bounds an allocation) this
// only bounds how long a corrupt or hostile object can keep a walker
// busy. 16 TiB is ~40x the manifest of a 200 TB database at the default
// chunk size.
const MaxStreamedMetadataBytes int64 = 16 << 40

// streamBackupChunkRefs walks a backup manifest and calls visit with the
// hex hash of every files[].chunks[] reference, without ever holding the
// document, a file entry or a chunk list in memory.
//
// Keys are matched with strings.EqualFold, mirroring encoding/json's
// case folding: a walker that matched case-sensitively would skip a
// "Files" key that the manifest reader still binds to the files field,
// under-count references, and let GC delete live chunks. A repeated
// "files" or "chunks" key is walked every time, which can only add
// references.
func streamBackupChunkRefs(r io.Reader, visit func(hash string) error) error {
	dec := json.NewDecoder(io.LimitReader(r, MaxStreamedMetadataBytes))
	if err := msExpectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		key, err := msReadKey(dec)
		if err != nil {
			return err
		}
		if !strings.EqualFold(key, "files") {
			if err := msSkipValue(dec); err != nil {
				return err
			}
			continue
		}
		if err := streamFilesArray(dec, visit); err != nil {
			return err
		}
	}
	return msExpectDelim(dec, '}')
}

func streamFilesArray(dec *json.Decoder, visit func(string) error) error {
	// "files": null is a valid empty value for a Go slice.
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("manifest: files is not an array")
	}
	for dec.More() {
		if err := msExpectDelim(dec, '{'); err != nil {
			return err
		}
		for dec.More() {
			key, err := msReadKey(dec)
			if err != nil {
				return err
			}
			if !strings.EqualFold(key, "chunks") {
				if err := msSkipValue(dec); err != nil {
					return err
				}
				continue
			}
			if err := streamChunksArray(dec, visit); err != nil {
				return err
			}
		}
		if err := msExpectDelim(dec, '}'); err != nil {
			return err
		}
	}
	return msExpectDelim(dec, ']')
}

// streamChunksArray consumes one "chunks" value (an array of chunk
// references, or null) and calls visit with each reference's hash.
func streamChunksArray(dec *json.Decoder, visit func(string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("manifest: chunks is not an array")
	}
	for dec.More() {
		var c chunkRef
		if err := dec.Decode(&c); err != nil {
			return err
		}
		if err := visit(c.Hash); err != nil {
			return err
		}
	}
	return msExpectDelim(dec, ']')
}

// streamSegmentChunkRefs is streamBackupChunkRefs for a WAL or
// logical-stream segment manifest, whose references sit in a top-level
// "chunks" array. Same case folding and repeated-key rules, so the set of
// references it yields is a superset of what json.Unmarshal would bind.
func streamSegmentChunkRefs(r io.Reader, visit func(hash string) error) error {
	dec := json.NewDecoder(io.LimitReader(r, MaxStreamedMetadataBytes))
	if err := msExpectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		key, err := msReadKey(dec)
		if err != nil {
			return err
		}
		if !strings.EqualFold(key, "chunks") {
			if err := msSkipValue(dec); err != nil {
				return err
			}
			continue
		}
		if err := streamChunksArray(dec, visit); err != nil {
			return err
		}
	}
	return msExpectDelim(dec, '}')
}

func msExpectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("manifest: expected %q, got %v", string(want), tok)
	}
	return nil
}

func msReadKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	k, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("manifest: expected object key, got %v", tok)
	}
	return k, nil
}

func msSkipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}

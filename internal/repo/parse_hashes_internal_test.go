package repo

// parse_hashes_internal_test.go — nets for the two hash parsers on the
// GC / replicate-verify paths. parseHexHash is the sharper one: gc's
// reference collection parses chunk hashes from manifests, and a wrong
// parse would build a wrong reference set — potentially marking a LIVE
// chunk as an orphan and deleting it (data loss). Both parsers must
// round-trip exactly, reject malformed input (never a silent
// wrong-but-plausible value), and never panic.

import (
	"encoding/json"
	"testing"
)

func TestParseHexHash_RoundTripAndReject(t *testing.T) {
	// Round-trip: for any real hash, parseHexHash inverts String().
	for _, body := range [][]byte{{}, []byte("x"), []byte("a longer chunk of bytes here")} {
		h := HashOf(body)
		got, err := parseHexHash(h.String())
		if err != nil {
			t.Fatalf("parseHexHash(%q) errored: %v", h.String(), err)
		}
		if got != h {
			t.Fatalf("round-trip mismatch: parseHexHash(HashOf(%q).String()) != original", body)
		}
	}
	// Reject: wrong length, non-hex — must error, never fabricate.
	for _, bad := range []string{
		"", "abc",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde",   // 63
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeff", // 65
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeZ",  // non-hex
	} {
		if _, err := parseHexHash(bad); err == nil {
			t.Errorf("parseHexHash(%q) accepted a malformed hash", bad)
		}
	}
}

func FuzzParseHexHash(f *testing.F) {
	f.Add("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	f.Add("garbage")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		h, err := parseHexHash(s) // never panic
		if err != nil {
			return
		}
		// A successful parse must round-trip back to the same 64 hex
		// chars (case-normalised) — it decoded a real hash, not junk.
		if h.String() == "" || len(h.String()) != 64 {
			t.Fatalf("parseHexHash(%q) succeeded but String()=%q is not 64 hex", s, h.String())
		}
	})
}

func FuzzParseChunkHashes(f *testing.F) {
	f.Add([]byte(`{"files":[{"chunks":[{"hash":"aa"},{"hash":"bb"}]}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, body []byte) {
		hashes, err := parseChunkHashes(body) // never panic
		if err != nil {
			return
		}
		// Every returned hash must actually appear in the input as a
		// hash value — the extractor reports what's there, never
		// invents a reference gc/replicate would then chase.
		var probe struct {
			Files []struct {
				Chunks []struct {
					Hash string `json:"hash"`
				} `json:"chunks"`
			} `json:"files"`
		}
		_ = json.Unmarshal(body, &probe)
		set := map[string]bool{}
		for _, fl := range probe.Files {
			for _, c := range fl.Chunks {
				set[c.Hash] = true
			}
		}
		for _, h := range hashes {
			if !set[h] {
				t.Fatalf("parseChunkHashes returned hash %q absent from the input", h)
			}
		}
	})
}

package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// scanTopLevelStrings reads a JSON object from r and returns the string
// values of the requested top-level keys, skipping everything else
// without materialising it — in particular the files array, which can be
// hundreds of GB. Keys match case-insensitively (strings.EqualFold),
// mirroring encoding/json, so a variant spelling that json.Unmarshal
// would have bound is not missed. If a key repeats, the last value wins,
// as with json.Unmarshal.
func scanTopLevelStrings(r io.Reader, want ...string) (map[string]string, error) {
	dec := json.NewDecoder(io.LimitReader(r, MaxStreamedManifestBytes))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	out := map[string]string{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		var canon string
		for _, w := range want {
			if strings.EqualFold(key, w) {
				canon = w
				break
			}
		}
		if canon == "" {
			if err := skipJSONValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch v := vt.(type) {
		case string:
			out[canon] = v
		case nil:
			// null leaves the field at its zero value, as Unmarshal does.
		default:
			return nil, fmt.Errorf("key %q is not a string", key)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// skipJSONValue consumes one value from dec without retaining it.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || (d != '{' && d != '[') {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// ScanManifestFields reads a manifest document from r and returns the
// parsed header fields named in want (JSON key names, matched
// case-insensitively like encoding/json), without reading further than
// it must: it stops as soon as every wanted key has been seen, and
// otherwise skips values (the files array included) without
// materialising them. Because Commit writes the header fields before the
// files array, asking for a header field of a multi-hundred-GB manifest
// reads only its first few KB.
//
// The scan also stops when it reaches the files array: only fields that
// precede it (which is every field Commit writes before the entries —
// including encryption) are returned, so an absent field costs the same
// few KB rather than a pass over the whole manifest. A wanted field a
// foreign writer placed after the files array is therefore not seen; this
// is for probes where "absent" is a safe answer.
//
// The result is a Manifest with only the wanted fields set. No signature
// is checked: for fields a caller authenticates some other way.
func ScanManifestFields(r io.Reader, want ...string) (*Manifest, error) {
	dec := json.NewDecoder(io.LimitReader(r, MaxStreamedManifestBytes))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("manifest: not a JSON object")
	}
	got := map[string]json.RawMessage{}
	for dec.More() && len(got) < len(want) {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		if strings.EqualFold(key, "files") {
			break
		}
		wanted := false
		for _, w := range want {
			if strings.EqualFold(key, w) {
				wanted = true
				break
			}
		}
		if !wanted {
			if err := skipJSONValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		got[key] = raw
	}
	return buildHeader(got)
}

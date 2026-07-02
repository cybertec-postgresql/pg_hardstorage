package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Bug 31: putOverwrite's staging temps leaked into List and were never
// reaped. The fix stages at the RESERVED "<key>.hstmp-<rand>" marker and
// hides exactly that — while caller-created keys containing ".tmp."
// (the repo layer's manifest commit temps, "<name>.json.tmp.<rand>")
// stay visible so GC's FindStaleTempManifests can reap them.
func TestIsFSStagingName_MatchesOverwriteTemp(t *testing.T) {
	cases := map[string]bool{
		"chunks/aa/obj.hstmp-0123456789abcdef": true,  // putOverwrite pattern
		"obj.tmp":                              true,  // bare suffix (legacy)
		"obj.deferred-deadbeef":                true,  // deferred stage
		"obj.excl-cafebabe":                    true,  // exclusive stage
		"chunks/aa/realkey":                    false, // real object
		"manifest.json":                        false, // real object
		"manifest.json.tmp.0123456789abcdef":   false, // repo-layer commit temp: GC must see it
		"0001.history.tmp.0123456789abcdef":    false, // repo-layer history temp: GC must see it
	}
	for name, want := range cases {
		base := filepath.Base(name)
		if got := isFSStagingName(base); got != want {
			t.Errorf("isFSStagingName(%q) = %v, want %v", base, got, want)
		}
	}
}

// A stray "<key>.hstmp-<hex>" staging temp on disk (e.g. left by a
// crashed overwrite) must NOT surface as a key from List — while a
// repo-layer ".json.tmp.<rand>" commit temp MUST (GC reaps those).
func TestList_HidesOverwriteStagingTemp(t *testing.T) {
	p := openTestPlugin(t)
	ctx := context.Background()

	// A committed, real object that List MUST return.
	if _, err := p.PutBytes(ctx, "chunks/real", []byte("real"), storage.PutOptions{}); err != nil {
		t.Fatalf("put real: %v", err)
	}
	// A leaked overwrite staging temp on disk (fs-internal → hidden).
	leaked := filepath.Join(p.root, "chunks", "real.hstmp-0123456789abcdef")
	if err := os.WriteFile(leaked, []byte("torn"), 0o600); err != nil {
		t.Fatalf("write leaked temp: %v", err)
	}
	// A crashed repo-layer manifest commit temp (caller key → visible).
	reapable := filepath.Join(p.root, "chunks", "manifest.json.tmp.0123456789abcdef")
	if err := os.WriteFile(reapable, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write reapable temp: %v", err)
	}

	var keys []string
	for obj, err := range p.List(ctx, "") {
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		keys = append(keys, obj.Key)
	}
	sawReal, sawLeak, sawReapable := false, false, false
	for _, k := range keys {
		switch k {
		case "chunks/real":
			sawReal = true
		case "chunks/real.hstmp-0123456789abcdef":
			sawLeak = true
		case "chunks/manifest.json.tmp.0123456789abcdef":
			sawReapable = true
		}
	}
	if !sawReal {
		t.Errorf("List dropped the real object; keys=%v", keys)
	}
	if sawLeak {
		t.Errorf("List leaked an fs-internal staging temp as a key; keys=%v", keys)
	}
	if !sawReapable {
		t.Errorf("List hid a repo-layer .json.tmp. commit temp — GC could never reap it; keys=%v", keys)
	}
}

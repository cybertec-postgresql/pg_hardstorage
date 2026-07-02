package sftp

import "testing"

// Bug 32: sftp List yielded Put's staging files as real objects. The
// fix stages at the RESERVED "<key>.hstmp-<rand>" marker and filters
// exactly that — caller-created keys containing ".tmp." (the repo
// layer's manifest commit temps, which GC's FindStaleTempManifests
// must List to reap) are NOT filtered.
func TestIsStagingName(t *testing.T) {
	cases := map[string]bool{
		"chunks/aa/obj.hstmp-0123456789abcdef": true,  // Put staging pattern
		"obj.hstmp-deadbeefdeadbeef":           true,  // staging
		"chunks/aa/realkey":                    false, // real object
		"manifest.json":                        false, // real object
		"manifest.json.tmp.0123456789abcdef":   false, // repo-layer commit temp: GC must see it
		"obj.tmpfile":                          false, // not our pattern
	}
	for name, want := range cases {
		if got := isStagingName(name); got != want {
			t.Errorf("isStagingName(%q) = %v, want %v", name, got, want)
		}
	}
}

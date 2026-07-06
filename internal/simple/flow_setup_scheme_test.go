package simple

import "testing"

// validateRepoURL must accept exactly the schemes the storage registry
// registers (internal/plugin/storage/*): file, s3, gcs, azblob, sftp,
// scp. It previously accepted gs:// and azure:// — schemes with no
// registered backend — while rejecting the real gcs:// and azblob://.
func TestValidateRepoURL_Schemes(t *testing.T) {
	accept := []string{
		"file:///srv/backups",
		"s3://bucket/prefix",
		"gcs://bucket/prefix",
		"azblob://acct.blob.core.windows.net/container/",
		"sftp://host/srv/repo",
		"scp://host/srv/repo",
	}
	reject := []string{
		"gs://bucket/prefix",   // not a registered scheme
		"azure://container/",   // not a registered scheme
		"http://example.com",   // unsupported
		"file://relative/path", // file:// must be absolute
		"",                     // empty
	}
	for _, u := range accept {
		if err := validateRepoURL(u); err != nil {
			t.Errorf("validateRepoURL(%q) = %v, want accepted", u, err)
		}
	}
	for _, u := range reject {
		if err := validateRepoURL(u); err == nil {
			t.Errorf("validateRepoURL(%q) = nil, want rejected", u)
		}
	}
}

package storage

import "testing"

// Windows repository paths arrive in three spellings; all three are
// file URLs, and none of them may reach url.Parse (which reads "C:" as
// host:port and rejects the backslashes). Platform-neutral: this is
// parsing, not file access.
func TestParseStorageURL_WindowsDrivePaths(t *testing.T) {
	for _, tc := range []struct{ raw, wantPath string }{
		{`C:\backups\repo`, `C:\backups\repo`},
		{`c:/backups/repo`, `c:/backups/repo`},
		{`file://C:\backups\repo`, `C:\backups\repo`},
		{`file://C:/backups/repo`, `C:/backups/repo`},
		{`file:///C:/backups/repo`, `/C:/backups/repo`}, // url.Parse's own result; fs.Open drops the slash on Windows
	} {
		u, err := parseStorageURL(tc.raw)
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if u.Scheme != "file" || u.Host != "" || u.Path != tc.wantPath {
			t.Errorf("%q: scheme=%q host=%q path=%q, want file/\"\"/%q", tc.raw, u.Scheme, u.Host, u.Path, tc.wantPath)
		}
	}

	// Not drive paths: a scheme that happens to be one letter long is
	// still a scheme, and a host is still a host.
	for _, raw := range []string{"s3://bucket/x", "file://host/share", "x:y"} {
		u, err := parseStorageURL(raw)
		if err == nil && u.Scheme == "file" && u.Host == "" {
			t.Errorf("%q was taken for a drive path: %+v", raw, u)
		}
	}
}

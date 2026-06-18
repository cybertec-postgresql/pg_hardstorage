package pgbackrest

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildPGConnection(t *testing.T) {
	tests := []struct {
		name string
		in   pgbackrestArgs
		want string
	}{
		{
			name: "host only",
			in:   pgbackrestArgs{pg1Host: "db.example.com"},
			want: "postgres://postgres@db.example.com/postgres",
		},
		{
			name: "host+port+user+db",
			in: pgbackrestArgs{
				pg1Host: "db.example.com", pg1Port: 5432,
				pg1User: "pgbackup", pg1Database: "warehouse",
			},
			want: "postgres://pgbackup@db.example.com:5432/warehouse",
		},
		{
			name: "no host -> empty",
			in:   pgbackrestArgs{pg1User: "pgbackup"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildPGConnection(tt.in); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestBuildRepoURL(t *testing.T) {
	tests := []struct {
		name        string
		in          pgbackrestArgs
		wantURL     string
		wantWarnSub string
		wantErrSub  string
	}{
		{
			name:    "posix",
			in:      pgbackrestArgs{repo1Type: "posix", repo1Path: "/var/lib/pgbackrest"},
			wantURL: "file:///var/lib/pgbackrest",
		},
		{
			name:       "posix relative path -> error",
			in:         pgbackrestArgs{repo1Type: "posix", repo1Path: "relative/path"},
			wantErrSub: "must be absolute",
		},
		{
			name:        "s3 minimal",
			in:          pgbackrestArgs{repo1Type: "s3", repo1S3Bucket: "acme-pg-backups"},
			wantURL:     "s3://acme-pg-backups",
			wantWarnSub: "credentials must be supplied",
		},
		{
			name: "s3 with prefix from repo1-path",
			in: pgbackrestArgs{
				repo1Type: "s3", repo1S3Bucket: "acme-pg-backups",
				repo1Path: "/db2",
			},
			wantURL: "s3://acme-pg-backups/db2",
		},
		{
			name:       "s3 missing bucket",
			in:         pgbackrestArgs{repo1Type: "s3"},
			wantErrSub: "--repo1-s3-bucket required",
		},
		{
			name: "s3 with endpoint forces path_style",
			in: pgbackrestArgs{
				repo1Type: "s3", repo1S3Bucket: "acme",
				repo1S3Endpoint: "https://minio.local:9000",
				repo1S3Region:   "eu-west-1",
			},
			wantURL: "s3://acme?endpoint=https://minio.local:9000&path_style=true&region=eu-west-1",
		},
		{
			name: "s3 region-only (real AWS)",
			in: pgbackrestArgs{
				repo1Type: "s3", repo1S3Bucket: "acme",
				repo1S3Region: "us-east-1",
			},
			wantURL: "s3://acme?region=us-east-1",
		},
		{
			name:       "unsupported type",
			in:         pgbackrestArgs{repo1Type: "azure"},
			wantErrSub: "unsupported --repo1-type",
		},
		{
			name:    "no type, no path -> empty (caller decides)",
			in:      pgbackrestArgs{},
			wantURL: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, warns, err := buildRepoURL(tt.in)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("err: got %v want substr %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotURL != tt.wantURL {
				t.Errorf("url: got %q want %q", gotURL, tt.wantURL)
			}
			if tt.wantWarnSub != "" {
				if !containsSubstring(warns, tt.wantWarnSub) {
					t.Errorf("warnings %v missing %q", warns, tt.wantWarnSub)
				}
			}
		})
	}
}

func TestMapToNativeArgs_RequiresStanza(t *testing.T) {
	_, _, err := mapToNativeArgs("backup", pgbackrestArgs{})
	if err == nil || !strings.Contains(err.Error(), "--stanza is required") {
		t.Fatalf("expected stanza error, got %v", err)
	}
}

func TestMapToNativeArgs_HappyPath(t *testing.T) {
	got, warns, err := mapToNativeArgs("backup", pgbackrestArgs{
		stanza:        "db1",
		pg1Host:       "db.example.com",
		pg1Port:       5432,
		pg1User:       "pgbackup",
		pg1Database:   "postgres",
		repo1Type:     "s3",
		repo1S3Bucket: "acme",
		compressType:  "gzip",
		archiveAsync:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"backup",
		"--pg-connection", "postgres://pgbackup@db.example.com:5432/postgres",
		"--repo", "s3://acme",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("native args:\n got %v\nwant %v", got, want)
	}
	// gzip + archive-async + s3 each contribute a warning.
	if len(warns) < 3 {
		t.Errorf("expected >=3 warnings, got %v", warns)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

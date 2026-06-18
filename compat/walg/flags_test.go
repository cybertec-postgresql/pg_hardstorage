package walg

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildPGConnection(t *testing.T) {
	tests := []struct {
		name string
		in   walgEnv
		want string
	}{
		{
			name: "host only",
			in:   walgEnv{pgHost: "db.example.com"},
			want: "postgres://postgres@db.example.com/postgres",
		},
		{
			name: "host+port+user+db",
			in: walgEnv{
				pgHost: "db.example.com", pgPort: "5432",
				pgUser: "pgbackup", pgDatabase: "warehouse",
			},
			want: "postgres://pgbackup@db.example.com:5432/warehouse",
		},
		{
			name: "no host -> empty",
			in:   walgEnv{pgUser: "pgbackup"},
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
		in          walgEnv
		wantURL     string
		wantWarnSub string
		wantErrSub  string
	}{
		{
			name:        "s3",
			in:          walgEnv{s3Prefix: "s3://acme/wal-g"},
			wantURL:     "s3://acme/wal-g",
			wantWarnSub: "credentials must be supplied",
		},
		{
			name: "s3 with endpoint forces path_style",
			in: walgEnv{
				s3Prefix:    "s3://acme/wal-g",
				awsEndpoint: "https://minio.local:9000",
				awsRegion:   "eu-west-1",
			},
			wantURL: "s3://acme/wal-g?endpoint=https://minio.local:9000&path_style=true&region=eu-west-1",
		},
		{
			name: "s3 with AWS_S3_FORCE_PATH_STYLE only",
			in: walgEnv{
				s3Prefix:          "s3://acme/wal-g",
				awsForcePathStyle: "true",
			},
			wantURL: "s3://acme/wal-g?path_style=true",
		},
		{
			name: "s3 region only (real AWS)",
			in: walgEnv{
				s3Prefix:  "s3://acme/wal-g",
				awsRegion: "us-east-1",
			},
			wantURL: "s3://acme/wal-g?region=us-east-1",
		},
		{
			name:    "gs",
			in:      walgEnv{gsPrefix: "gs://acme/wal-g"},
			wantURL: "gs://acme/wal-g",
		},
		{
			name:    "azure",
			in:      walgEnv{azurePrefix: "azure://container/path"},
			wantURL: "azure://container/path",
		},
		{
			name:    "file absolute",
			in:      walgEnv{filePrefix: "/srv/wal-g"},
			wantURL: "file:///srv/wal-g",
		},
		{
			name:       "file relative -> error",
			in:         walgEnv{filePrefix: "relative/path"},
			wantErrSub: "must be absolute",
		},
		{
			name:        "ssh",
			in:          walgEnv{sshPrefix: "ssh://user@host:/path"},
			wantURL:     "sftp://user@host:/path",
			wantWarnSub: "mapped to sftp://",
		},
		{
			name:    "no prefix",
			in:      walgEnv{},
			wantURL: "",
		},
		{
			name: "two prefixes -> error",
			in: walgEnv{
				s3Prefix: "s3://x", filePrefix: "/y",
			},
			wantErrSub: "multiple WALG_*_PREFIX",
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

func TestDeploymentName(t *testing.T) {
	tests := []struct {
		name string
		in   walgEnv
		want string
	}{
		{
			name: "explicit override wins",
			in:   walgEnv{deployment: "prod-db", pgHost: "db.example.com"},
			want: "prod-db",
		},
		{
			name: "PGHOST is the fallback",
			in:   walgEnv{pgHost: "db.example.com"},
			want: "db.example.com",
		},
		{
			name: "PGHOST with port artefact strips it",
			in:   walgEnv{pgHost: "db.example.com:5432"},
			want: "db.example.com",
		},
		{
			name: "no host -> default",
			in:   walgEnv{},
			want: "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.deploymentName(); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestMapEnvToNativeArgs_HappyPath(t *testing.T) {
	got, warns, err := mapEnvToNativeArgs("backup", walgEnv{
		s3Prefix:          "s3://acme/wal-g",
		pgHost:            "db.example.com",
		pgPort:            "5432",
		pgUser:            "pgbackup",
		pgDatabase:        "postgres",
		compressionMethod: "lz4",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"backup",
		"--pg-connection", "postgres://pgbackup@db.example.com:5432/postgres",
		"--repo", "s3://acme/wal-g",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("native args:\n got %v\nwant %v", got, want)
	}
	// AWS creds + lz4 compression each contribute a warning.
	if len(warns) < 2 {
		t.Errorf("expected >=2 warnings, got %v", warns)
	}
}

func TestMapEnvToNativeArgs_LibsodiumRefused(t *testing.T) {
	_, _, err := mapEnvToNativeArgs("backup", walgEnv{
		s3Prefix:     "s3://acme/wal-g",
		libsodiumKey: "<base64>",
	})
	if err == nil || !strings.Contains(err.Error(), "WALG_LIBSODIUM_KEY") {
		t.Errorf("expected libsodium refusal; got %v", err)
	}
}

func TestMapEnvToNativeArgs_GPGRefused(t *testing.T) {
	_, _, err := mapEnvToNativeArgs("backup", walgEnv{
		s3Prefix: "s3://acme/wal-g",
		gpgKeyID: "DEADBEEF",
	})
	if err == nil || !strings.Contains(err.Error(), "GPG/PGP envelope") {
		t.Errorf("expected GPG/PGP refusal; got %v", err)
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

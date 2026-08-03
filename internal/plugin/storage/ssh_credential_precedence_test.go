// ssh_credential_precedence_test.go — where the SSH plugins get their
// credentials from, and in what order.
//
// scp and sftp both resolve four settings as Extras-then-environment.
// In production only the environment half is ever populated: nothing
// fills StorageConfig.Extras (wiring_e2e_test.go pins that), so
// PG_HARDSTORAGE_{SCP,SFTP}_* IS the configuration surface for a real
// deployment. It had no test.
//
// That matters beyond "a knob is untested". known_hosts is mandatory
// precisely so the plugins never fall back to a
// StrictHostKeyChecking=no posture; if its resolution silently
// preferred the wrong source, an operator's pinned host keys would be
// quietly replaced by whatever the other source named, and the failure
// mode is accepting a host you did not intend to trust.
//
// Both plugins are driven from ONE table. They are meant to be
// identical here, and a shared table is what makes a divergence show
// up as a failing case rather than as a file nobody added.
//
// No container: resolution happens before any dial, and both plugins
// name the path they resolved in the resulting error, so the error
// text is the observation.

package storage_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp"
)

// sshPlugin describes one plugin's credential surface.
type sshPlugin struct {
	name   string
	scheme string
	prefix string // env var prefix, e.g. "PG_HARDSTORAGE_SCP"
	open   func(ctx context.Context, cfg storage.StorageConfig) error
}

func sshPlugins() []sshPlugin {
	return []sshPlugin{
		{
			name: "scp", scheme: "scp", prefix: "PG_HARDSTORAGE_SCP",
			open: func(ctx context.Context, cfg storage.StorageConfig) error {
				return (&scp.Plugin{}).Open(ctx, cfg)
			},
		},
		{
			name: "sftp", scheme: "sftp", prefix: "PG_HARDSTORAGE_SFTP",
			open: func(ctx context.Context, cfg storage.StorageConfig) error {
				return (&sftp.Plugin{}).Open(ctx, cfg)
			},
		},
	}
}

func sshURL(t *testing.T, scheme string) *url.URL {
	t.Helper()
	u, err := url.Parse(scheme + "://user@127.0.0.1:1/srv/repo")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// clearSSHEnv unsets every credential variable for BOTH plugins, so a
// developer's own PG_HARDSTORAGE_* exports cannot make these pass or
// fail. t.Setenv restores on cleanup and fails the test if it is ever
// called from a parallel test, which is what keeps this safe.
func clearSSHEnv(t *testing.T) {
	t.Helper()
	for _, p := range sshPlugins() {
		for _, k := range []string{
			"_IDENTITY_FILE", "_IDENTITY_PASSPHRASE", "_PASSWORD", "_KNOWN_HOSTS",
		} {
			t.Setenv(p.prefix+k, "")
		}
	}
}

// TestSSHCredentials_KnownHostsPrecedence drives the mandatory
// setting. Extras and the environment are pointed at DIFFERENT
// non-existent paths, and the error names whichever one won.
func TestSSHCredentials_KnownHostsPrecedence(t *testing.T) {
	for _, p := range sshPlugins() {
		t.Run(p.name, func(t *testing.T) {
			cases := []struct {
				name       string
				extras     map[string]string // nil / absent / present-but-empty all differ
				env        string
				wantInErr  string
				wantAbsent string
			}{
				{
					name: "extras_beats_env",
					// Extras is documented as taking precedence. It is
					// unreachable in production today, but that is a
					// property of the callers, not of this contract.
					extras:    map[string]string{"known_hosts": "/nonexistent/from-extras"},
					env:       "/nonexistent/from-env",
					wantInErr: "from-extras", wantAbsent: "from-env",
				},
				{
					name:      "env_used_when_extras_nil",
					extras:    nil,
					env:       "/nonexistent/from-env",
					wantInErr: "from-env",
				},
				{
					name:      "env_used_when_key_absent",
					extras:    map[string]string{"password": "unrelated"},
					env:       "/nonexistent/from-env",
					wantInErr: "from-env",
				},
				{
					name: "empty_extras_value_falls_through_to_env",
					// A key present but EMPTY must be treated as unset,
					// not as an explicit empty path. Otherwise a caller
					// that always populates the map — with "" for what it
					// does not know — would shadow the environment and
					// strand a correctly configured host.
					extras:    map[string]string{"known_hosts": ""},
					env:       "/nonexistent/from-env",
					wantInErr: "from-env",
				},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					clearSSHEnv(t)
					t.Setenv(p.prefix+"_KNOWN_HOSTS", tc.env)

					err := p.open(context.Background(), storage.StorageConfig{
						URL:    sshURL(t, p.scheme),
						Extras: tc.extras,
					})
					if err == nil {
						t.Fatal("Open succeeded against a non-existent known_hosts file")
					}
					if !strings.Contains(err.Error(), tc.wantInErr) {
						t.Errorf("resolved the wrong known_hosts source.\n got: %v\nwant it "+
							"to name %q", err, tc.wantInErr)
					}
					if tc.wantAbsent != "" && strings.Contains(err.Error(), tc.wantAbsent) {
						t.Errorf("resolved %q, but Extras must win: %v", tc.wantAbsent, err)
					}
				})
			}
		})
	}
}

// TestSSHCredentials_KnownHostsRequired pins the refusal itself.
//
// With neither source set the plugins must fail rather than connect,
// and the message must name BOTH ways to fix it — an operator who only
// learns about `extras.known_hosts` cannot act on it, because nothing
// in production populates Extras.
func TestSSHCredentials_KnownHostsRequired(t *testing.T) {
	for _, p := range sshPlugins() {
		t.Run(p.name, func(t *testing.T) {
			clearSSHEnv(t)
			err := p.open(context.Background(), storage.StorageConfig{URL: sshURL(t, p.scheme)})
			if err == nil {
				t.Fatal("Open succeeded with no known_hosts — the plugin would be free to " +
					"accept any host key, which is the posture this check exists to refuse")
			}
			msg := err.Error()
			for _, want := range []string{"known_hosts required", p.prefix + "_KNOWN_HOSTS"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal must mention %q; got: %v", want, err)
				}
			}
		})
	}
}

// writeKnownHosts writes a known_hosts file with one real, parseable
// entry and returns its path.
//
// The key is generated rather than hard-coded: a hand-written
// placeholder is not valid SSH wire format, so loading fails before
// identity resolution is ever reached and the test measures the wrong
// step. It needs to PARSE, not to match any host.
func writeKnownHosts(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := append([]byte("127.0.0.1 "), ssh.MarshalAuthorizedKey(pub)...)
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSSHCredentials_IdentityFilePrecedence does the same for the
// private key, which resolves only after known_hosts succeeds — so
// this case needs a real known_hosts file to reach it.
func TestSSHCredentials_IdentityFilePrecedence(t *testing.T) {
	for _, p := range sshPlugins() {
		t.Run(p.name, func(t *testing.T) {
			kh := writeKnownHosts(t)

			for _, tc := range []struct {
				name              string
				extras, env       string
				wantIn, wantNotIn string
			}{
				{
					name:   "extras_beats_env",
					extras: "/nonexistent/id-from-extras", env: "/nonexistent/id-from-env",
					wantIn: "id-from-extras", wantNotIn: "id-from-env",
				},
				{
					name:   "env_used_when_extras_absent",
					extras: "", env: "/nonexistent/id-from-env",
					wantIn: "id-from-env",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					clearSSHEnv(t)
					t.Setenv(p.prefix+"_KNOWN_HOSTS", kh)
					t.Setenv(p.prefix+"_IDENTITY_FILE", tc.env)

					err := p.open(context.Background(), storage.StorageConfig{
						URL:    sshURL(t, p.scheme),
						Extras: map[string]string{"identity_file": tc.extras},
					})
					if err == nil {
						t.Fatal("Open succeeded with a non-existent identity file")
					}
					if !strings.Contains(err.Error(), tc.wantIn) {
						t.Errorf("resolved the wrong identity_file source.\n got: %v\nwant "+
							"it to name %q", err, tc.wantIn)
					}
					if tc.wantNotIn != "" && strings.Contains(err.Error(), tc.wantNotIn) {
						t.Errorf("resolved %q, but Extras must win: %v", tc.wantNotIn, err)
					}
				})
			}
		})
	}
}

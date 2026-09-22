package sink

import "testing"

// A fixture image that publishes no manifest for this host is a real
// problem: its container dies at startup with "exec format error" and
// the backend it fronts silently loses every real-server test. That is
// what VerifyAllImageArch exists to catch, and it caught it once for
// real (atmoz/sftp:alpine-3.7 on aarch64).
//
// Being unable to ASK is a different outcome, and the guard used to
// report it as the first one. Docker Hub refuses anonymous manifest
// queries, so on a host without credentials the check announced
//
//	a pinned fixture image cannot run on linux/arm64
//
// about images sitting in the local daemon, built for linux/arm64,
// working perfectly. The message named an architecture problem that
// did not exist. A reader trusting it would go looking for a
// replacement pin for an image that needs none.
func TestRegistryUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name: "docker hub, unauthenticated — the exact text that caused the misdiagnosis",
			stderr: "errors:\ndenied: requested access to the resource is denied\n" +
				"unauthorized: authentication required\n",
			want: true,
		},
		{name: "rate limited", stderr: "toomanyrequests: You have reached your pull rate limit", want: true},
		{name: "air-gapped / DNS", stderr: "dial tcp: lookup registry-1.docker.io: no such host", want: true},
		{name: "proxy refuses", stderr: "connection refused", want: true},
		{name: "slow network", stderr: "net/http: TLS handshake timeout", want: true},

		// The real finding must still get through. If these ever
		// classify as "unavailable", the guard goes quiet on exactly
		// the bug it was written for.
		{
			name:   "genuinely no manifest for this platform",
			stderr: "no matching manifest for linux/arm64/v8 in the manifest list entries",
			want:   false,
		},
		{name: "tag does not exist", stderr: "manifest unknown", want: false},
		{name: "malformed reference", stderr: "invalid reference format", want: false},
		{name: "empty", stderr: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryUnavailable(tc.stderr); got != tc.want {
				t.Errorf("registryUnavailable(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestImagePlatformString pins the spelling the fallback compares on:
// LocalImagePlatform parses `docker image inspect --format
// {{.Os}}/{{.Architecture}}`, and the comparison against the host is a
// struct equality, so a drift in either direction would silently stop
// matching.
func TestImagePlatformString(t *testing.T) {
	p := ImagePlatform{OS: "linux", Arch: "arm64"}
	if p.String() != "linux/arm64" {
		t.Errorf("String() = %q, want linux/arm64", p.String())
	}
}

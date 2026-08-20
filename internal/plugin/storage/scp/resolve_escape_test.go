package scp

import (
	"strings"
	"testing"
)

// TestResolvePrefix_Containment pins the F-0002 escape-resistance
// contract and its precision:
//
//   - every key that resolves MUST stay inside p.root (an escape is a
//     security failure — the audit's "bypassable" claim);
//   - the check must not false-positive on legitimate keys that merely
//     CONTAIN ".." as a substring. That matters because
//     validateStorageID permits dots in deployment names, so
//     `manifests/db..prod/backups/...` is a legal key shape that the
//     old raw ".." substring ban refused — breaking List/Delete on
//     every scp:// repo for such a deployment.
//
// The implementation mirrors restore.safeJoinTarget's containment
// posture: join (which cleans), then verify the result cannot be
// anything but the root itself or a path beneath it.
func TestResolvePrefix_Containment(t *testing.T) {
	p := &Plugin{root: "/srv/repo"}

	inside := func(full string) bool {
		return full == p.root || strings.HasPrefix(full, p.root+"/")
	}

	t.Run("escapes are refused", func(t *testing.T) {
		attacks := []string{
			"../etc/passwd",
			"../../etc/cron.d/x",
			"a/../../../etc",
			"/etc/passwd", // absolute
			"..",
			"a/../..",
			"foo/../../../..",
			"../.",
			".//..",
			"a/b/../../../../c",
		}
		for _, k := range attacks {
			if got, err := p.resolvePrefix(k); err == nil {
				if !inside(got) {
					t.Errorf("key %q ESCAPED root: %q", k, got)
				}
				t.Errorf("key %q: expected refusal, resolved to %q", k, got)
			}
		}
	})

	t.Run("legitimate keys resolve inside root", func(t *testing.T) {
		cases := map[string]string{
			"":                           "/srv/repo",
			"chunks/ab/cd":               "/srv/repo/chunks/ab/cd",
			"weird..name":                "/srv/repo/weird..name",
			"manifests/db..prod/x":       "/srv/repo/manifests/db..prod/x",
			"./a":                        "/srv/repo/a",
			"a/./b":                      "/srv/repo/a/b",
			"a/b/../c":                   "/srv/repo/a/c",
			"...":                        "/srv/repo/...",
			"manifests/db..prod/backups": "/srv/repo/manifests/db..prod/backups",
			"manifests/db..prod/":        "/srv/repo/manifests/db..prod",
			"a/..":                       "/srv/repo",
		}
		for k, want := range cases {
			got, err := p.resolvePrefix(k)
			if err != nil {
				t.Errorf("key %q: unexpected refusal: %v", k, err)
				continue
			}
			if got != want {
				t.Errorf("key %q = %q, want %q", k, got, want)
			}
			// Invariant: anything that resolves is inside root.
			if !inside(got) {
				t.Errorf("key %q resolved OUTSIDE root: %q", k, got)
			}
		}
	})
}

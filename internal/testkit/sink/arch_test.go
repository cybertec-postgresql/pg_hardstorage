package sink

// arch_test.go — the fixture-image architecture guard.
//
// The bug this guards against cost the SFTP backend every one of its
// real-server tests on aarch64 — the contract suite, the lease
// exclusion tests, and all four storage soaks — because a pinned tag
// published only linux/amd64 and the container died at startup. Nothing
// in the suite noticed the coverage was gone; the tests simply failed
// with an error that named neither the cause nor the fix.
//
// These tests run offline against a stubbed docker so the guard itself
// is always covered. TestSinkImages_PublishForHostArch in
// arch_registry_test.go does the live registry check.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubDockerStdout swaps execCommand so `docker manifest inspect`
// returns a canned body. It writes the body to a file and cats it, the
// same trick stubDocker uses, so no shell quoting is involved.
func stubDockerStdout(t *testing.T, body string) (restore func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "docker" && len(args) > 0 && args[0] == "manifest" {
			return exec.Command("cat", path)
		}
		return exec.Command(name, args...)
	}
	return func() { execCommand = orig }
}

// multiArchManifest is the `docker manifest inspect --verbose` shape
// for an image published as an OCI index, including the
// "unknown/unknown" attestation entries buildx attaches — those must
// not be counted as platforms.
const multiArchManifest = `[
  {"Descriptor":{"platform":{"architecture":"amd64","os":"linux"}}},
  {"Descriptor":{"platform":{"architecture":"unknown","os":"unknown"}}},
  {"Descriptor":{"platform":{"architecture":"arm64","os":"linux","variant":"v8"}}},
  {"Descriptor":{"platform":{"architecture":"unknown","os":"unknown"}}}
]`

// singleArchManifest is the shape for a plain v2 manifest: one object,
// not an array. atmoz/sftp:alpine-3.7 looked exactly like this.
const singleArchManifest = `{"Descriptor":{"platform":{"architecture":"amd64","os":"linux"}}}`

func TestImagePlatforms_ManifestListDropsAttestationEntries(t *testing.T) {
	defer stubDockerStdout(t, multiArchManifest)()

	got, err := ImagePlatforms(context.Background(), "example:multi")
	if err != nil {
		t.Fatalf("ImagePlatforms: %v", err)
	}
	want := []string{"linux/amd64", "linux/arm64"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (attestation entries must not count as platforms)", got, want)
	}
	for i, p := range got {
		if p.String() != want[i] {
			t.Errorf("platform[%d] = %s, want %s", i, p, want[i])
		}
	}
}

func TestImagePlatforms_SingleManifestIsNotAnArray(t *testing.T) {
	defer stubDockerStdout(t, singleArchManifest)()

	got, err := ImagePlatforms(context.Background(), "example:single")
	if err != nil {
		t.Fatalf("ImagePlatforms: %v", err)
	}
	if len(got) != 1 || got[0].String() != "linux/amd64" {
		t.Fatalf("got %v, want [linux/amd64]", got)
	}
}

// The regression proper: an amd64-only image on an arm64 host must be
// refused, and the refusal must carry enough to act on.
func TestVerifyImageArch_RefusesAmd64OnlyImageOnArm64(t *testing.T) {
	defer stubDockerStdout(t, singleArchManifest)()

	err := VerifyImageArch(context.Background(), "sftp", "linux", "arm64")
	if err == nil {
		t.Fatal("amd64-only image accepted on an arm64 host; this is exactly the gap " +
			"that left the SFTP backend untested on aarch64")
	}
	for _, want := range []string{"linux/arm64", "linux/amd64", SinkImages["sftp"]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q — an operator cannot act on it:\n%s", want, err)
		}
	}
}

func TestVerifyImageArch_AcceptsMultiArchImage(t *testing.T) {
	defer stubDockerStdout(t, multiArchManifest)()

	for _, arch := range []string{"amd64", "arm64"} {
		if err := VerifyImageArch(context.Background(), "sftp", "linux", arch); err != nil {
			t.Errorf("linux/%s rejected for a multi-arch image: %v", arch, err)
		}
	}
}

func TestVerifyImageArch_UnknownKind(t *testing.T) {
	if err := VerifyImageArch(context.Background(), "nope", "linux", "arm64"); err == nil {
		t.Fatal("unknown sink kind accepted")
	}
}

// VerifyAllImageArch must report every offender, not just the first:
// someone porting the suite to a new architecture wants the full list
// in one pass.
func TestVerifyAllImageArch_ReportsEveryOffender(t *testing.T) {
	defer stubDockerStdout(t, singleArchManifest)()

	err := VerifyAllImageArch(context.Background())
	if err == nil {
		t.Skip("host is linux/amd64; this test needs a host the stub manifest does not satisfy")
	}
	// Distinct images, not distinct kinds: s3-minio and tls-minio share
	// one pin and are deliberately checked once.
	distinct := map[string]bool{}
	for _, img := range SinkImages {
		distinct[img] = true
	}
	if got := strings.Count(err.Error(), "\n  - "); got != len(distinct) {
		t.Errorf("reported %d offending images, want %d (one per distinct pin):\n%s",
			got, len(distinct), err)
	}
}

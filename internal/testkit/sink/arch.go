// arch.go — "does this fixture image exist for the machine we are on?"
//
// Why this file exists
// --------------------
// Every image in SinkImages is pulled from a registry, base images
// included: a locally-built fixture still needs its FROM line to
// resolve for the host's architecture. If a pinned tag publishes only
// linux/amd64, then on an arm64 host the container starts and dies
// milliseconds later with
//
//	exec /entrypoint: exec format error
//
// and the backend it fronts silently loses ALL real-server coverage.
// That is not hypothetical: atmoz/sftp:alpine-3.7 was amd64-only, and
// nine tests across internal/backup and internal/plugin/storage —
// including the storage soaks, the ones that exercise the plugin under
// concurrency and fault injection — failed for that single reason on
// aarch64, a platform this project ships binaries and a container
// image for.
//
// A pinned tag is a promise about content, not about architecture. The
// tag can be perfectly reproducible and still be unrunnable here. So
// the architecture has to be checked, and checked where the operator
// can act on it — at pre-fetch time, with the image name and the
// platforms it does publish — rather than discovered as a dead
// container mid-suite.
package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// ImagePlatform is one os/architecture pair an image publishes.
type ImagePlatform struct {
	OS   string
	Arch string
}

// String renders the platform the way docker spells it ("linux/arm64").
func (p ImagePlatform) String() string { return p.OS + "/" + p.Arch }

// dockerManifestEntry is the slice of `docker manifest inspect
// --verbose` output we care about. The command returns a single object
// for a plain (single-architecture) manifest and an array of them for a
// manifest list / OCI index; both shapes carry Descriptor.Platform, so
// decoding through this one type covers both.
type dockerManifestEntry struct {
	Descriptor struct {
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"Descriptor"`
}

// ImagePlatforms returns every platform image advertises in its
// registry manifest.
//
// Attestation manifests are skipped. A buildx-produced index carries
// one "unknown/unknown" entry per real platform (the SBOM/provenance
// attachments); counting those as platforms would make any such image
// appear to support everything.
//
// This reaches the registry, so it needs network and, for private
// images, credentials. Callers that must work offline should use
// PreflightAirgap instead — that asks the local daemon.
func ImagePlatforms(ctx context.Context, image string) ([]ImagePlatform, error) {
	out, err := execCommand(ctx, "docker", "manifest", "inspect", "--verbose", image).Output()
	if err != nil {
		return nil, fmt.Errorf("sink: docker manifest inspect %s: %w", image, err)
	}
	// Try the manifest-list shape first, then the single-manifest one.
	var entries []dockerManifestEntry
	if jerr := json.Unmarshal(out, &entries); jerr != nil {
		var one dockerManifestEntry
		if oerr := json.Unmarshal(out, &one); oerr != nil {
			return nil, fmt.Errorf("sink: parse manifest for %s: %w", image, oerr)
		}
		entries = []dockerManifestEntry{one}
	}
	var platforms []ImagePlatform
	seen := map[string]bool{}
	for _, e := range entries {
		p := ImagePlatform{OS: e.Descriptor.Platform.OS, Arch: e.Descriptor.Platform.Architecture}
		if p.OS == "" || p.Arch == "" || p.OS == "unknown" || p.Arch == "unknown" {
			continue
		}
		if seen[p.String()] {
			continue
		}
		seen[p.String()] = true
		platforms = append(platforms, p)
	}
	if len(platforms) == 0 {
		return nil, fmt.Errorf("sink: image %s advertises no usable platform", image)
	}
	sort.Slice(platforms, func(i, j int) bool { return platforms[i].String() < platforms[j].String() })
	return platforms, nil
}

// VerifyImageArch reports whether the image behind one sink kind
// publishes a manifest for goos/goarch.
//
// The error names the image, the platform we needed and the platforms
// it actually has, because the fix is a judgement call the operator has
// to make with that information in hand: bump to a multi-arch tag,
// switch to a different upstream, or build the fixture locally from a
// multi-arch base the way the sftp and ssh-exec runtimes do.
func VerifyImageArch(ctx context.Context, kind, goos, goarch string) error {
	img, ok := SinkImages[kind]
	if !ok {
		return fmt.Errorf("sink: unknown sink kind %q (known: %v)", kind, KnownKinds())
	}
	platforms, err := ImagePlatforms(ctx, img)
	if err != nil {
		return err
	}
	want := ImagePlatform{OS: goos, Arch: goarch}
	for _, p := range platforms {
		if p == want {
			return nil
		}
	}
	have := make([]string, 0, len(platforms))
	for _, p := range platforms {
		have = append(have, p.String())
	}
	return fmt.Errorf("sink %q: image %s publishes no %s manifest (has: %s) — "+
		"on this host its container dies at startup with \"exec format error\" and the backend "+
		"loses all real-server coverage; bump to a multi-arch tag or build the fixture locally "+
		"from a multi-arch base (see internal/testkit/sink/sftp.go)",
		kind, img, want, strings.Join(have, ", "))
}

// VerifyAllImageArch checks every sink image against the platform the
// current process runs on, and reports EVERY failure rather than only
// the first: an operator moving the suite to a new architecture wants
// the whole list in one pass, not one image per run.
func VerifyAllImageArch(ctx context.Context) error {
	var problems []string
	// Dedupe by image: s3-minio and tls-minio share one pin, and a
	// registry round-trip per kind would query it twice for no gain.
	checked := map[string]bool{}
	for _, k := range KnownKinds() {
		if img := SinkImages[k]; checked[img] {
			continue
		} else {
			checked[img] = true
		}
		if err := VerifyImageArch(ctx, k, runtime.GOOS, runtime.GOARCH); err != nil {
			problems = append(problems, "  - "+err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("sink images unusable on %s/%s:\n%s",
			runtime.GOOS, runtime.GOARCH, strings.Join(problems, "\n"))
	}
	return nil
}

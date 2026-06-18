// Package imagetag computes the deterministic registry tag
// for one (OS × PG × arch) testbed image cell.
//
// Both cmd/pg_hardstorage_testkit/image.go (the build driver)
// and internal/testkit/compose (the docker-compose generator)
// import this package, so the tag a `compose generate` writes
// always matches the tag `image build` would have produced.
//
// Tag scheme:
//
//	<repo>:<os>-pg<version>-<arch>-<short-sha>[-<recipe-sha>]
//
// where short-sha = first 8 hex chars of
// sha256(os|pg|arch|family|packages); recipe-sha (when supplied)
// is 8 hex chars of sha256 over the family Dockerfile + the
// shared entrypoint-pg.sh.
//
// Why the recipe-sha matters: without it, fixing a bug in
// entrypoint-pg.sh produces an image with the SAME tag as the
// pre-fix one.  `docker build -t TAG` rewrites the local tag
// to the rebuilt image, so in a clean shop the fix lands.  In
// shops where Docker's cache layers came from a remote source,
// or where the previous broken image is still around with the
// same tag, compose's `pull_policy: missing` happily reuses
// the stale image and the bug looks unfixed.  Putting recipe
// content into the tag itself sidesteps the whole problem:
// new content → new tag → old image cannot satisfy it.
package imagetag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// For returns the tag for the supplied cell.  Identical inputs
// always produce identical tags so a CI rebuild reuses cached
// layers.
func For(repo, osID, pg, arch, family, packages string) string {
	return ForWithRecipe(repo, osID, pg, arch, family, packages, "")
}

// ForWithRecipe is For + an optional recipe content digest.
// An empty recipe collapses to the legacy For() tag, so old
// call sites and tests stay byte-for-byte compatible.
func ForWithRecipe(repo, osID, pg, arch, family, packages, recipe string) string {
	tagPart := fmt.Sprintf("%s-pg%s-%s",
		strings.ReplaceAll(osID, ":", "-"), pg, arch)
	h := sha256.Sum256([]byte(osID + "|" + pg + "|" + arch + "|" + family + "|" + packages))
	short := hex.EncodeToString(h[:4])
	if recipe == "" {
		return fmt.Sprintf("%s:%s-%s", repo, tagPart, short)
	}
	return fmt.Sprintf("%s:%s-%s-%s", repo, tagPart, short, recipe)
}

// RecipeDigest computes a short content-fingerprint for the
// build recipe of `family` — the Dockerfile.<family>-family
// + the shared entrypoint-pg.sh.  Both files live under
// dockerfileDir.
//
// Failure modes (file not found, unreadable, dockerfileDir
// empty) all collapse to "" rather than an error: callers
// pass the result straight to ForWithRecipe, which treats ""
// as "use the legacy tag scheme".  This keeps the function
// usable from contexts where the dockerfile dir might not
// exist (e.g. unit tests against a synthetic cell list) while
// still pinning the tag to recipe content whenever the files
// are reachable.
func RecipeDigest(family, dockerfileDir string) string {
	if family == "" || dockerfileDir == "" {
		return ""
	}
	files := []string{
		filepath.Join(dockerfileDir, "Dockerfile."+family+"-family"),
		filepath.Join(dockerfileDir, "entrypoint-pg.sh"),
	}
	h := sha256.New()
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		// NUL separator so concatenation can't smuggle a
		// hash collision via boundary movement.
		h.Write(b)
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

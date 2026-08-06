package cli_test

// docs_chart_test.go — a path the chart documentation says is mounted
// must actually be mounted by the rendered chart.
//
// Seventh surface in the docs-truthfulness family, and the first one
// added because a user hit it rather than because a sweep found it.
// Issue #46: helm-sidecar-chart.md stated that "the keyring directory
// /etc/pg_hardstorage/keyring/ mounts as part of the ConfigMap".
// configmap.yaml never templated a keyring, and the StatefulSet had
// three fixed mounts with no way to add a fourth — so there was no way
// at all to persist a keyring for `kek_ref: local:default` on the
// chart. The page had described the feature since it was written.
//
// The other six guards read source or introspect the binary. A chart
// cannot be checked that way: its templates are Go-templated YAML, and
// what matters is the OUTPUT. So this renders it with `helm template`
// and reads the result.
//
// helm is a hard requirement rather than a skip. The chart is a
// shipped artefact; a machine without helm cannot verify it, and
// reporting ok would be the "a skip is not a pass" failure that this
// repository has been bitten by more than once. CI installs helm in
// the packaging-lint job.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
)

const chartDir = "charts/pg-hardstorage-sidecar"

// chartDocs are the pages that make claims about the chart.
var chartDocs = []string{
	"docs/how-to/kubernetes/helm-sidecar-chart.md",
}

// renderChart runs `helm template` with the given --set overrides and
// returns the manifests.
func renderChart(t *testing.T, sets ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("helm is not installed: %v\n\n"+
			"The chart is a shipped artefact and this test verifies it against its own "+
			"documentation. Skipping here would report ok for an unverified chart, which "+
			"is how issue #46 survived: the docs described a keyring mount that the "+
			"templates never had.", err)
	}
	root := repoRootFromTest(t)
	args := []string{"template", "probe", filepath.Join(root, chartDir)}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %v failed: %v\n%s", sets, err, out)
	}
	return string(out)
}

// mountPathRe finds the paths a rendered manifest actually mounts.
var mountPathRe = regexp.MustCompile(`mountPath:\s*(\S+)`)

func mountedPaths(manifests string) map[string]bool {
	out := map[string]bool{}
	for _, m := range mountPathRe.FindAllStringSubmatch(manifests, -1) {
		out[strings.Trim(m[1], `"'`)] = true
	}
	return out
}

// TestChartMountsEveryDocumentedPath is the guard.
//
// It reads every `/etc/...` or `/var/...` path the chart docs present
// as mounted, and requires the rendered chart to mount it under some
// configuration. The keyring is only mounted when configured, so the
// render enables the optional features the docs describe — a claim
// that holds only in a configuration nobody can reach is still false.
func TestChartMountsEveryDocumentedPath(t *testing.T) {
	root := repoRootFromTest(t)

	// Render with the optional features the docs describe turned on.
	manifests := renderChart(t,
		"keyring.existingSecret=probe-keyring",
	)
	mounted := mountedPaths(manifests)
	if len(mounted) == 0 {
		t.Fatal("rendered chart mounts nothing at all — the render or the regex broke, " +
			"and every assertion below would hold vacuously")
	}

	// Paths the docs describe as mounted. Matches the sentence shape
	// the pages use: a backticked absolute path near the word "mount".
	claimRe := regexp.MustCompile("`(/(?:etc|var)/[a-z_/]+)/?`[^`\\n]{0,80}?mount")
	var claimed []string
	seen := map[string]bool{}
	forEachChartDoc(t, root, func(rel string, body string) {
		flat := regexp.MustCompile(`\s+`).ReplaceAllString(body, " ")
		for _, m := range claimRe.FindAllStringSubmatch(flat, -1) {
			p := strings.TrimRight(m[1], "/")
			if !seen[p] {
				seen[p] = true
				claimed = append(claimed, p)
			}
		}
	})
	if len(claimed) == 0 {
		t.Fatal("no mount claims parsed from the chart docs — the phrasing changed and " +
			"this guard is no longer reading them")
	}
	sort.Strings(claimed)
	t.Logf("checked %d documented mount path(s) against the rendered chart: %v",
		len(claimed), claimed)

	var missing []string
	for _, p := range claimed {
		if mounted[p] || mounted[p+"/"] {
			continue
		}
		missing = append(missing, p)
	}
	if len(missing) > 0 {
		var have []string
		for p := range mounted {
			have = append(have, p)
		}
		sort.Strings(have)
		t.Errorf("%d path(s) the chart docs say are mounted are not mounted by the "+
			"rendered chart: %v\n\nthe chart mounts: %v\n\n"+
			"That is issue #46 exactly: the keyring was documented as mounted, no template "+
			"provided it, and there was no way to persist a local KEK on the chart at all.",
			len(missing), missing, have)
	}
}

// TestChartKeyringIsASecretNotAConfigMap pins the thing the fix
// deliberately changed. The docs used to say the keyring rides in the
// ConfigMap; key material in a ConfigMap is readable by anyone with
// `get configmap`, a lower bar than `get secret` wherever the two are
// separated by RBAC.
func TestChartKeyringIsASecretNotAConfigMap(t *testing.T) {
	manifests := renderChart(t, `keyring.files.kek\.bin=3q2+7w==`)

	if !strings.Contains(manifests, "kind: Secret") {
		t.Fatal("an inline keyring rendered no Secret")
	}

	// Walk the ConfigMap document specifically: the key must not be there.
	for _, doc := range strings.Split(manifests, "\n---") {
		if !strings.Contains(doc, "kind: ConfigMap") {
			continue
		}
		if strings.Contains(doc, "kek.bin") || strings.Contains(doc, "keyring") {
			t.Errorf("keyring material appears in a ConfigMap:\n%s\n\n"+
				"A ConfigMap is readable by anyone with `get configmap`. Key material "+
				"belongs in a Secret.", doc)
		}
	}
}

// TestChartMountsNothingExtraByDefault: the keyring is opt-in, and a
// KMS-backed deployment must not acquire a mount it never asked for.
func TestChartMountsNothingExtraByDefault(t *testing.T) {
	manifests := renderChart(t)
	if strings.Contains(manifests, "keyring") {
		t.Error("the default render references a keyring; it is opt-in, and a cloud-KMS " +
			"deployment needs no local key material at all")
	}
}

// forEachChartDoc hands each chart documentation page to fn.
func forEachChartDoc(t *testing.T, root string, fn func(rel, body string)) {
	t.Helper()
	for _, rel := range chartDocs {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fn(rel, string(body))
	}
}

// TestChartKeyringFilenamesMatchTheProduct pins the chart's projected
// filenames against the constants the agent actually reads.
//
// This is the failure the projected mount invites. An operator keys
// their Secret however they like — `kek`, `private-key`, whatever the
// external-secrets template produced — and the chart maps it to a
// filename. If that filename drifts from keystore.KEKFileName or
// keystore.PrivateKeyFile, the mounted directory looks fully populated
// and the agent finds nothing: `doctor` reports the signing key absent,
// backups go unsigned or fail to start, and nothing in the manifest
// says the cause was a filename.
//
// So the chart does NOT take the path from values, and this test holds
// it to the source of truth rather than to a copy of it.
func TestChartKeyringFilenamesMatchTheProduct(t *testing.T) {
	manifests := renderChart(t,
		"keyring.kek.secretName=kek-secret",
		"keyring.signingKey.secretName=signing-secret",
		"keyring.signingPub.secretName=pub-secret",
	)
	if !strings.Contains(manifests, "projected:") {
		t.Fatal("per-file keyring sources did not render a projected volume; three Secrets " +
			"cannot otherwise share one mount path")
	}
	for _, want := range []string{
		keystore.KEKFileName,
		keystore.PrivateKeyFile,
		keystore.PublicKeyFile,
	} {
		if !strings.Contains(manifests, "path: "+want) {
			t.Errorf("the chart never mounts a file named %q.\n\n"+
				"That is the name the agent opens (from internal/backup/keystore). A "+
				"projected keyring under any other name gives a directory that looks "+
				"populated and an agent that finds nothing.", want)
		}
	}
}

// TestChartKeyringPerFileIsOptional: the per-file sources must not
// disturb the simple paths.
func TestChartKeyringPerFileIsOptional(t *testing.T) {
	// existingSecret alone still renders a plain secret volume.
	m := renderChart(t, "keyring.existingSecret=one-secret")
	if strings.Contains(m, "projected:") {
		t.Error("existingSecret rendered a projected volume; the simple case should stay simple")
	}
	if !strings.Contains(m, "secretName: one-secret") {
		t.Error("existingSecret did not render its secret volume")
	}
	// And nothing at all by default.
	if strings.Contains(renderChart(t), "keyring") {
		t.Error("the default render references a keyring")
	}
}

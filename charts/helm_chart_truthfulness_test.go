// helm_chart_truthfulness_test.go — pins that the K8s Helm
// charts shipped under charts/ render to valid manifests with
// the documented default values.
//
// The class of bug this catches: a chart that no longer
// templates cleanly (a syntax error, an undefined value, a
// missing required field) but still passes `helm lint`.  An
// operator following the docs runs `helm install ...` and
// hits a non-obvious template error — at the worst possible
// time, when they need a backup tool in a hurry.
//
// Approach: shell out to `helm template` against each chart
// with default values and assert:
//
//   - exit 0 (chart compiles)
//   - stdout contains every object kind the chart's README /
//     templates/ directory advertises
//
// Skips cleanly when helm is not on PATH so the test is a
// no-op on CI agents without helm installed (those agents
// should not block the build; the failure mode is "we ship a
// broken chart" which is observable elsewhere too).
package charts_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// chartsRoot finds the repo's charts/ directory relative to
// this test file.
func chartsRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}

// helmAvailable returns the helm binary path or "" if helm
// isn't installed.  Callers should t.Skip when unavailable.
func helmAvailable(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skipf("helm not available on PATH (skip is intentional; chart tests need helm): %v", err)
	}
	return p
}

// TestHelmChart_Sidecar_RendersCleanly: the operator-facing
// sidecar chart must `helm template` to a non-empty manifest
// stream that contains every advertised object kind.
func TestHelmChart_Sidecar_RendersCleanly(t *testing.T) {
	helm := helmAvailable(t)
	chart := filepath.Join(chartsRoot(t), "pg-hardstorage-sidecar")

	var out, errb bytes.Buffer
	cmd := exec.Command(helm, "template", "test-release", chart)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\nstderr=%s", err, errb.String())
	}

	body := out.String()
	if strings.TrimSpace(body) == "" {
		t.Fatal("helm template produced empty manifest stream")
	}

	// Every kind the README / runbooks reference must appear.
	// If a future PR removes one of these, this test fails
	// fast with a clear hint to update both sides.
	wantKinds := []string{
		"kind: ServiceAccount",
		"kind: ConfigMap",
		"kind: Service",
		"kind: StatefulSet",
	}
	for _, want := range wantKinds {
		if !strings.Contains(body, want) {
			t.Errorf("rendered chart missing %q (kind expected per chart's templates/)", want)
		}
	}
}

// TestHelmChart_Sidecar_StubLabels: the chart must consistently
// label every object with the documented selector keys.
// Operators write `kubectl get ... -l app.kubernetes.io/name=
// pg-hardstorage-sidecar` to discover their installation —
// inconsistent labels break that.
func TestHelmChart_Sidecar_StubLabels(t *testing.T) {
	helm := helmAvailable(t)
	chart := filepath.Join(chartsRoot(t), "pg-hardstorage-sidecar")

	out, err := exec.Command(helm, "template", "test-release", chart).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	body := string(out)

	// The release-instance label is the operator's discovery
	// hook.  Missing on any object breaks `kubectl get -l ...`
	// discovery for that object.
	mustHave := []string{
		"app.kubernetes.io/name: pg-hardstorage-sidecar",
		"app.kubernetes.io/instance: test-release",
		"app.kubernetes.io/part-of: pg-hardstorage",
	}
	for _, m := range mustHave {
		// Every kind in the chart should carry these labels —
		// count occurrences against the number of `kind:` lines
		// so a label going missing on ONE object surfaces.
		kindCount := strings.Count(body, "\nkind: ")
		labelCount := strings.Count(body, m)
		if labelCount < kindCount {
			t.Errorf("label %q present on only %d of %d objects (every object should carry it)",
				m, labelCount, kindCount)
		}
	}
}

// TestHelmChart_ServerStub_HasChartYaml: the v0.5+ server
// chart is intentionally a stub.  Pin that Chart.yaml exists
// (so `helm pull oci://...` works at the registry-listing
// layer) and that it announces its stub status so operators
// who pull it know what they're getting.
func TestHelmChart_ServerStub_HasChartYaml(t *testing.T) {
	chart := filepath.Join(chartsRoot(t), "pg-hardstorage-server", "Chart.yaml")
	body, err := readFile(chart)
	if err != nil {
		t.Fatalf("read %s: %v", chart, err)
	}
	// stub annotation must be present so the chart shows up
	// in the registry as such.
	if !strings.Contains(body, "status: stub") {
		t.Errorf("server chart should declare `status: stub` in annotations")
	}
}

// readFile is a tiny test-local helper to keep the os import
// scoped to this one use.
func readFile(path string) (string, error) {
	out, err := exec.Command("cat", path).Output()
	return string(out), err
}

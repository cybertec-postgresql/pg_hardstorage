package cli_test

// workflow_budget_test.go — a CI job must not be killed before the test
// it exists to run has had time to fail.
//
// GitHub gives a job `timeout-minutes` and gives `go test` its own
// `-timeout`. These are two independent clocks and only one of them
// produces a diagnosis:
//
//   - `go test -timeout` expiring is a FAILURE. It dumps every
//     goroutine's stack, names the test that hung, and — for the gates
//     in this repository — prints whatever partial progress message the
//     test wrote on its way out ("PROOF INCOMPLETE: verified+restored
//     13 of 32 backups").
//   - `timeout-minutes` expiring is a CANCELLATION. The step is severed
//     mid-syscall, there is no stack, no test name, and the run is
//     labelled cancelled — which in the UI is indistinguishable from a
//     human pressing the button.
//
// So the job budget must be the OUTER bound. The nightly chaos soak had
// it the other way round: one job at timeout-minutes: 120 running three
// steps whose own timeouts summed to 30m + 5h + 110m, with the chaos
// soak — the restore-proof gate, the strongest data-loss guard we have
// — running last. It was cut off three nights in a row (2026-08-04,
// -05, -06) after passing in ~1h35m on the three nights before. Nobody
// noticed, because "cancelled" reads as somebody's decision rather than
// as a gate that never ran.
//
// The fix was to split the lanes. This test is what keeps them split:
// it re-derives the arithmetic from the YAML on every run, so the next
// person who adds a step to a job gets told at `go test` time rather
// than at 04:23 some morning three weeks later.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// goTestTimeout finds the `-timeout <N><unit>` a step hands to go test.
// Both spellings are in use across the workflows.
var goTestTimeout = regexp.MustCompile(`-timeout[= ](\d+)([smh])`)

// stepBudgetMinutes returns the longest go-test timeout in a step's
// script, in minutes, and whether one was found.
//
// The longest rather than the sum: a step's `go test` invocations run in
// sequence, but a step that runs two of them and is cut off during the
// second is still a step whose budget was too small — and taking the max
// is the conservative direction. It can only understate the need, so a
// failure here is never a false alarm.
func stepBudgetMinutes(script string) (int, bool) {
	worst, found := 0, false
	for _, m := range goTestTimeout.FindAllStringSubmatch(script, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		var mins int
		switch m[2] {
		case "s":
			mins = (n + 59) / 60
		case "m":
			mins = n
		case "h":
			mins = n * 60
		}
		found = true
		if mins > worst {
			worst = mins
		}
	}
	return worst, found
}

type ciJob struct {
	workflow string
	name     string
	timeout  int
	steps    []ciStep
}

type ciStep struct {
	name    string
	minutes int
}

// loadCIJobs parses every workflow and returns the jobs that run go test
// under an explicit -timeout.
func loadCIJobs(t *testing.T) []ciJob {
	t.Helper()
	root := repoRootFromTest(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var jobs []ciJob
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var wf struct {
			Jobs map[string]struct {
				TimeoutMinutes int `yaml:"timeout-minutes"`
				Steps          []struct {
					Name string `yaml:"name"`
					Run  string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(body, &wf); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for name, j := range wf.Jobs {
			job := ciJob{workflow: e.Name(), name: name, timeout: j.TimeoutMinutes}
			for _, st := range j.Steps {
				mins, ok := stepBudgetMinutes(st.Run)
				if !ok {
					continue
				}
				label := st.Name
				if label == "" {
					label = "(unnamed step)"
				}
				job.steps = append(job.steps, ciStep{label, mins})
			}
			if len(job.steps) > 0 {
				jobs = append(jobs, job)
			}
		}
	}
	sort.Slice(jobs, func(i, k int) bool {
		if jobs[i].workflow != jobs[k].workflow {
			return jobs[i].workflow < jobs[k].workflow
		}
		return jobs[i].name < jobs[k].name
	})
	return jobs
}

// TestCIJobBudgetsOutlastTheirTests is the guard.
func TestCIJobBudgetsOutlastTheirTests(t *testing.T) {
	jobs := loadCIJobs(t)
	if len(jobs) == 0 {
		t.Fatal("no workflow job runs `go test` with an explicit -timeout — either the " +
			"workflows moved or the parser broke, and every assertion below holds vacuously")
	}

	checked := 0
	for _, j := range jobs {
		// A job with no timeout-minutes gets GitHub's 6h default, which
		// outlasts anything here.
		if j.timeout == 0 {
			continue
		}
		checked++

		need := 0
		var detail []string
		for _, st := range j.steps {
			need += st.minutes
			detail = append(detail, st.name+" -timeout "+strconv.Itoa(st.minutes)+"m")
		}
		if need <= j.timeout {
			continue
		}
		t.Errorf("%s: job %q caps at timeout-minutes: %d but its steps ask for %dm "+
			"(%s).\n\n"+
			"The job clock is the outer bound and it wins. When it expires the step is "+
			"severed: no goroutine dump, no test name, no partial-progress message — and "+
			"the run is reported CANCELLED, which reads as a human stopping it rather than "+
			"as a gate that never finished. Give the job room for its steps, or split them "+
			"into separate jobs so a slow one cannot starve the rest.",
			j.workflow, j.name, j.timeout, need, strings.Join(detail, ", "))
	}
	if checked == 0 {
		t.Fatal("every job parsed had no timeout-minutes; the guard asserted nothing")
	}
	t.Logf("checked %d job(s) with an explicit timeout-minutes across %d go-test job(s)",
		checked, len(jobs))
}

// TestCIBudgetGuardCanFail proves the arithmetic reads the units it
// claims to. A guard that silently parses `5h` as 5 minutes would have
// passed on the exact configuration that motivated it.
func TestCIBudgetGuardCanFail(t *testing.T) {
	cases := []struct {
		script string
		want   int
	}{
		{"go test -timeout 110m -v ./...", 110},
		{"go test -timeout 5h ./...", 300},
		{"go test -timeout=30m ./...", 30},
		{"go test -timeout 90s ./...", 2},
		// Two invocations in one step: the longer one is what a
		// too-small job budget cuts off.
		{"go test -timeout 10m ./a\ngo test -timeout 2h ./b", 120},
	}
	for _, tc := range cases {
		got, ok := stepBudgetMinutes(tc.script)
		if !ok {
			t.Errorf("stepBudgetMinutes(%q) found no timeout", tc.script)
			continue
		}
		if got != tc.want {
			t.Errorf("stepBudgetMinutes(%q) = %dm, want %dm — misreading the unit is how a "+
				"5h step hides inside a 2h job", tc.script, got, tc.want)
		}
	}
	if _, ok := stepBudgetMinutes("go build ./..."); ok {
		t.Error("stepBudgetMinutes found a timeout in a step that has none")
	}
}

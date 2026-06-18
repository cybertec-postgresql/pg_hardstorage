package cli_test

import (
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// commitForecastBackup plants a manifest with the given StoppedAt
// + size for forecast-CLI tests. Different from
// commitVerifiableBackup in that we control StoppedAt explicitly
// (the helper uses time.Now()-relative offsets).
func commitForecastBackup(t *testing.T, w *readWorld, deployment string, stoppedAt time.Time, bytes int64) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	body := []byte("forecast-payload-x")
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	id := deployment + ".forecast." + stoppedAt.Format("20060102T150405.000Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			// Declared logical size = bytes; chunk Len matches so
			// Validate's chunk-sum-equals-file-size invariant holds.
			// (The CAS body is a fixed small payload — forecast
			// looks at manifest-reported size, not stored bytes.)
			Path: "data/" + id, Size: bytes, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: bytes}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// forecastReportView mirrors the v1 contract's top-level shape.
type forecastReportView struct {
	Schema                string  `json:"schema"`
	URL                   string  `json:"url"`
	BaselineWindowSeconds int64   `json:"baseline_window_seconds"`
	HorizonsSeconds       []int64 `json:"horizons_seconds"`
	DeploymentFilter      string  `json:"deployment_filter"`
	Deployments           []struct {
		Name             string  `json:"name"`
		CurrentBytes     int64   `json:"current_bytes"`
		CurrentManifests int     `json:"current_manifests"`
		SamplesObserved  int     `json:"samples_observed"`
		Confidence       string  `json:"confidence"`
		BytesPerDay      float64 `json:"bytes_per_day"`
		RSquared         float64 `json:"r_squared"`
		Note             string  `json:"note"`
		Projections      []struct {
			HorizonName    string `json:"horizon_name"`
			ProjectedBytes int64  `json:"projected_bytes"`
		} `json:"projections"`
	} `json:"deployments"`
	Fleet *struct {
		TotalCurrentBytes int64 `json:"total_current_bytes"`
		TotalProjections  []struct {
			HorizonName    string `json:"horizon_name"`
			ProjectedBytes int64  `json:"projected_bytes"`
		} `json:"total_projections"`
	} `json:"fleet"`
	Cost *struct {
		PricePerGBMonth float64 `json:"price_per_gb_month"`
		Currency        string  `json:"currency"`
		PricingModel    string  `json:"pricing_model"`
		CurrentMonthly  float64 `json:"current_monthly_cost"`
	} `json:"cost"`
	Anomalies []struct {
		Deployment string `json:"deployment"`
		Reason     string `json:"reason"`
	} `json:"anomalies"`
}

// TestForecast_RequiresRepo: --repo or positional URL is required.
func TestForecast_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "forecast", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestForecast_BadFormat: --format must be json or markdown.
func TestForecast_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestForecast_BadBaselineWindow: malformed --baseline-window
// surfaces usage.bad_flag.
func TestForecast_BadBaselineWindow(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--baseline-window", "yesterday", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestForecast_BadHorizon: malformed --horizon surfaces usage.bad_flag.
func TestForecast_BadHorizon(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--horizon", "infinity", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestForecast_NegativePrice: --price-per-gb-month < 0 is refused.
func TestForecast_NegativePrice(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--price-per-gb-month", "-1.0", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestForecast_PositionalAndFlagConflict: both forms disagreeing.
func TestForecast_PositionalAndFlagConflict(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "forecast",
		w.repoURL+"-other", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.repo_conflict") {
		t.Errorf("expected usage.repo_conflict:\n%s", errb)
	}
}

// TestForecast_EmptyRepo: a fresh repo produces a clean report.
func TestForecast_EmptyRepo(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if view.Schema != "pg_hardstorage.forecast.v1" {
		t.Errorf("Schema = %q", view.Schema)
	}
	if view.URL != w.repoURL {
		t.Errorf("URL = %q", view.URL)
	}
	if len(view.Deployments) != 0 {
		t.Errorf("Deployments = %d, want 0", len(view.Deployments))
	}
	if view.Fleet == nil {
		t.Errorf("Fleet should be present")
	}
}

// TestForecast_LinearGrowth_Detected: a deployment with monotone
// growth fits with high R².
func TestForecast_LinearGrowth_Detected(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		stoppedAt := now.Add(-time.Duration(7-i) * 24 * time.Hour)
		commitForecastBackup(t, w, "db1", stoppedAt, 1<<30)
	}

	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 1 {
		t.Fatalf("Deployments = %d, want 1", len(view.Deployments))
	}
	d := view.Deployments[0]
	if d.SamplesObserved != 7 {
		t.Errorf("SamplesObserved = %d, want 7", d.SamplesObserved)
	}
	if d.Confidence != "high" {
		t.Errorf("Confidence = %q (R²=%v), want high", d.Confidence, d.RSquared)
	}
	if d.BytesPerDay <= 0 {
		t.Errorf("BytesPerDay = %v", d.BytesPerDay)
	}
	if len(d.Projections) != 3 {
		t.Errorf("Projections = %d, want 3 (default horizons)", len(d.Projections))
	}
}

// TestForecast_DefaultHorizons: 30d/90d/365d are emitted.
func TestForecast_DefaultHorizons(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	want := []int64{
		int64(30 * 24 * 60 * 60),
		int64(90 * 24 * 60 * 60),
		int64(365 * 24 * 60 * 60),
	}
	if len(view.HorizonsSeconds) != len(want) {
		t.Fatalf("HorizonsSeconds = %v, want %v", view.HorizonsSeconds, want)
	}
	for i, w := range want {
		if view.HorizonsSeconds[i] != w {
			t.Errorf("HorizonsSeconds[%d] = %d, want %d", i, view.HorizonsSeconds[i], w)
		}
	}
}

// TestForecast_CustomHorizons: --horizon repeats override the
// default set.
func TestForecast_CustomHorizons(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL,
		"--horizon", "7d", "--horizon", "60d",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if len(view.HorizonsSeconds) != 2 {
		t.Errorf("HorizonsSeconds = %v, want 2", view.HorizonsSeconds)
	}
}

// TestForecast_DeploymentFilter: only the named deployment is
// considered.
func TestForecast_DeploymentFilter(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		commitForecastBackup(t, w, "db1", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}
	for i := 0; i < 4; i++ {
		commitForecastBackup(t, w, "db2", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}

	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if view.DeploymentFilter != "db1" {
		t.Errorf("DeploymentFilter = %q", view.DeploymentFilter)
	}
	if len(view.Deployments) != 1 {
		t.Errorf("Deployments = %d, want 1 (filtered)", len(view.Deployments))
	}
}

// TestForecast_CostProjection: --price-per-gb-month enables the
// cost section.
func TestForecast_CostProjection(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		commitForecastBackup(t, w, "db1", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}

	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL,
		"--price-per-gb-month", "0.023",
		"--currency", "USD",
		"--pricing-model", "s3-standard",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if view.Cost == nil {
		t.Fatal("Cost section missing")
	}
	if view.Cost.PricePerGBMonth != 0.023 {
		t.Errorf("rate = %v", view.Cost.PricePerGBMonth)
	}
	if view.Cost.PricingModel != "s3-standard" {
		t.Errorf("model = %q", view.Cost.PricingModel)
	}
}

// TestForecast_NoFleet: --no-fleet drops the rollup.
func TestForecast_NoFleet(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--no-fleet", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if view.Fleet != nil {
		t.Errorf("Fleet = %+v, want nil", view.Fleet)
	}
}

// TestForecast_NoAnomalies: --no-anomalies suppresses detection.
func TestForecast_NoAnomalies(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--no-anomalies", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if len(view.Anomalies) != 0 {
		t.Errorf("Anomalies = %v, want empty", view.Anomalies)
	}
}

// TestForecast_MarkdownFormat: --format markdown returns the
// Markdown body when -o text. -o json still emits JSON.
func TestForecast_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	commitForecastBackup(t, w, "db1", now.Add(-1*24*time.Hour), 1<<30)

	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"# pg_hardstorage forecast report",
		"## Fleet projection",
		"## Per-deployment forecast",
		"## Methodology notes",
		"db1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}

	// JSON output ignores --format markdown.
	stdout, _, exit = runCLI(t, "forecast",
		"--repo", w.repoURL, "--format", "markdown", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var any map[string]any
	if err := stdjson.Unmarshal([]byte(stdout), &any); err != nil {
		t.Errorf("--format markdown -o json should still emit valid JSON: %v\n%s", err, stdout)
	}
}

// TestForecast_TextFormat_Compact: -o text without --format markdown
// returns a compact summary.
func TestForecast_TextFormat_Compact(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	commitForecastBackup(t, w, "db1", now.Add(-1*24*time.Hour), 1<<30)

	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"forecast",
		"Baseline:",
		"Horizons:",
		"Per-deployment:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("compact output missing %q:\n%s", want, stdout)
		}
	}
}

// TestForecast_PositionalURL: positional <url> works without --repo.
func TestForecast_PositionalURL(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if view.URL != w.repoURL {
		t.Errorf("URL = %q", view.URL)
	}
}

// TestForecast_HelpDiscoverable: help surfaces the major flags.
func TestForecast_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "forecast", "--help")
	for _, want := range []string{
		"--baseline-window",
		"--horizon",
		"--price-per-gb-month",
		"--pricing-model",
		"--currency",
		"--format",
		"--no-fleet",
		"--no-anomalies",
		"--deployment",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("forecast --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestForecast_DurationParser_Days: "30d" is a valid duration.
func TestForecast_DurationParser_Days(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "forecast",
		"--repo", w.repoURL,
		"--baseline-window", "30d",
		"--horizon", "60d",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view forecastReportView
	bodyOf(t, stdout, &view)
	if view.BaselineWindowSeconds != int64(30*24*60*60) {
		t.Errorf("BaselineWindowSeconds = %d, want %d",
			view.BaselineWindowSeconds, 30*24*60*60)
	}
}

package forecast_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/forecast"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

type forecastWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
	repoURL  string
	meta     *repo.Metadata
}

func setupWorld(t *testing.T) *forecastWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	res, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL})
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return &forecastWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
		repoURL:  repoURL,
		meta:     &res.Metadata,
	}
}

// commitBackup plants a manifest at the given StoppedAt with a
// FileEntry whose Size is `bytes` — that's what
// manifestLogicalBytes will sum.
func (w *forecastWorld) commitBackup(t *testing.T, deployment string, stoppedAt time.Time, bytes int64) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	body := []byte(strings.Repeat("x", 16)) // small but nonzero
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	id := deployment + ".full." + stoppedAt.Format("20060102T150405Z")
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
			// Manifest.Validate's chunk-sum-equals-file-size invariant
			// holds. (Forecast reads manifest-reported size, not
			// stored bytes — body is a small fixed payload.)
			Path: "data/" + id, Size: bytes, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: bytes}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestGenerate_EmptyRepo: a fresh repo produces a clean report.
func TestGenerate_EmptyRepo(t *testing.T) {
	w := setupWorld(t)
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.Schema != forecast.ReportSchema {
		t.Errorf("Schema = %q", rep.Schema)
	}
	if len(rep.Deployments) != 0 {
		t.Errorf("Deployments = %d, want 0", len(rep.Deployments))
	}
	if rep.Fleet == nil || rep.Fleet.TotalCurrentBytes != 0 {
		t.Errorf("Fleet = %+v", rep.Fleet)
	}
}

// TestGenerate_Validation: programmer-error guards.
func TestGenerate_Validation(t *testing.T) {
	w := setupWorld(t)
	if _, err := forecast.Generate(context.Background(), nil, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil sp must error")
	}
	if _, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{}); err == nil {
		t.Error("nil verifier must error")
	}
}

// TestGenerate_LinearGrowth_Detected: a deployment with monotone
// linear growth fits with high R².
func TestGenerate_LinearGrowth_Detected(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// 10 backups over 30 days, +1 GiB each. Cumulative grows
	// linearly → R² = 1.0.
	for i := 0; i < 10; i++ {
		stoppedAt := now.Add(-time.Duration(30-i*3) * 24 * time.Hour)
		w.commitBackup(t, "db1", stoppedAt, int64(1<<30)) // 1 GiB
	}
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(rep.Deployments) != 1 {
		t.Fatalf("Deployments = %d", len(rep.Deployments))
	}
	d := rep.Deployments[0]
	if d.SamplesObserved != 10 {
		t.Errorf("SamplesObserved = %d, want 10", d.SamplesObserved)
	}
	if d.Confidence != "high" {
		t.Errorf("Confidence = %q, want high (R²=%v)", d.Confidence, d.RSquared)
	}
	if d.RSquared < 0.95 {
		t.Errorf("R² = %v, want > 0.95 for monotone linear growth", d.RSquared)
	}
	if d.BytesPerDay <= 0 {
		t.Errorf("BytesPerDay = %v; want positive", d.BytesPerDay)
	}
	if len(d.Projections) == 0 {
		t.Error("no Projections")
	}
	// Projections at 30/90/365 days should be increasing.
	for i := 1; i < len(d.Projections); i++ {
		if d.Projections[i].ProjectedBytes < d.Projections[i-1].ProjectedBytes {
			t.Errorf("non-monotone projections: %+v", d.Projections)
		}
	}
}

// TestGenerate_Insufficient_Samples: fewer than MinSamples
// manifests → confidence=insufficient + flat projection.
func TestGenerate_Insufficient_Samples(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*24*time.Hour), 1<<30)
	w.commitBackup(t, "db1", now.Add(-2*24*time.Hour), 1<<30)

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Deployments[0]
	if d.Confidence != "insufficient" {
		t.Errorf("Confidence = %q, want insufficient (only 2 samples)", d.Confidence)
	}
	if d.Note == "" {
		t.Errorf("Note should explain why")
	}
	// All projections at the current size, no growth.
	for _, p := range d.Projections {
		if p.ProjectedBytes != d.CurrentBytes {
			t.Errorf("flat projection should equal current; got %d vs %d",
				p.ProjectedBytes, d.CurrentBytes)
		}
	}
}

// TestGenerate_BaselineWindow_Filters: backups outside the
// baseline don't influence the regression.
func TestGenerate_BaselineWindow_Filters(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Three in-window samples — should establish a slope.
	for i := 0; i < 3; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}
	// One ancient sample, way outside the baseline window.
	w.commitBackup(t, "db1", now.Add(-1000*24*time.Hour), 100<<30)

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier:       w.verifier,
		Now:            now,
		BaselineWindow: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Deployments[0]
	if d.SamplesObserved != 3 {
		t.Errorf("SamplesObserved = %d, want 3 (ancient backup excluded)", d.SamplesObserved)
	}
	if d.CurrentManifests != 4 {
		t.Errorf("CurrentManifests = %d, want 4 (all-time)", d.CurrentManifests)
	}
}

// TestGenerate_Fleet_Rollup: multi-deployment fleet sums
// projections per horizon.
func TestGenerate_Fleet_Rollup(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}
	for i := 0; i < 5; i++ {
		w.commitBackup(t, "db2", now.Add(-time.Duration(i+1)*24*time.Hour), 2<<30)
	}

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fleet == nil {
		t.Fatal("Fleet missing")
	}
	if rep.Fleet.TotalCurrentBytes != int64(1<<30)+int64(2<<30) {
		t.Errorf("TotalCurrentBytes = %d, want 3GiB", rep.Fleet.TotalCurrentBytes)
	}
	if len(rep.Fleet.TotalProjections) != 3 {
		t.Errorf("TotalProjections = %d, want 3 (default horizons)",
			len(rep.Fleet.TotalProjections))
	}
}

// TestGenerate_Cost_Projection: opt-in cost projection multiplies
// fleet bytes by the supplied rate.
func TestGenerate_Cost_Projection(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier:        w.verifier,
		Now:             now,
		PricePerGBMonth: 0.023, // S3 Standard Frankfurt-ish
		Currency:        "USD",
		PricingModel:    "s3-standard",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cost == nil {
		t.Fatal("Cost section missing")
	}
	if rep.Cost.PricePerGBMonth != 0.023 {
		t.Errorf("rate = %v", rep.Cost.PricePerGBMonth)
	}
	if rep.Cost.PricingModel != "s3-standard" {
		t.Errorf("model = %q", rep.Cost.PricingModel)
	}
	// 1 GiB × 0.023 = 0.023; rounded to cents = 0.02.
	if rep.Cost.CurrentMonthly < 0.01 || rep.Cost.CurrentMonthly > 0.03 {
		t.Errorf("CurrentMonthly = %v, want ~0.02", rep.Cost.CurrentMonthly)
	}
	for _, p := range rep.Cost.Projections {
		if p.ProjectedMonthlyCost < 0 {
			t.Errorf("negative cost: %v", p)
		}
	}
}

// TestGenerate_Cost_AbsentByDefault: without --price-per-gb-month,
// the cost section is nil.
func TestGenerate_Cost_AbsentByDefault(t *testing.T) {
	w := setupWorld(t)
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cost != nil {
		t.Errorf("Cost = %+v, want nil (no rate supplied)", rep.Cost)
	}
}

// TestGenerate_DeploymentFilter: --deployment scopes the rollups.
func TestGenerate_DeploymentFilter(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*24*time.Hour), 1<<30)
	w.commitBackup(t, "db2", now.Add(-1*24*time.Hour), 1<<30)

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier:         w.verifier,
		Now:              now,
		DeploymentFilter: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Deployments) != 1 {
		t.Errorf("Deployments = %d, want 1 (filtered)", len(rep.Deployments))
	}
}

// TestGenerate_AnomalyDetected: a tail with a 5× rate over the
// baseline-without-tail surfaces a sudden_uptick.
func TestGenerate_AnomalyDetected(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: 1 GiB per backup, 4 backups spread over 30 days
	// before the tail window (so AnomalyTailWindow = 7d ago,
	// baseline range = -30d to -7d).
	for i := 0; i < 4; i++ {
		stoppedAt := now.Add(-time.Duration(8+i*5) * 24 * time.Hour) // -8, -13, -18, -23
		w.commitBackup(t, "db1", stoppedAt, 1<<30)
	}
	// Tail: 5 GiB per backup, 3 backups in the last 7 days.
	for i := 0; i < 3; i++ {
		stoppedAt := now.Add(-time.Duration(i*2+1) * 24 * time.Hour) // -1, -3, -5
		w.commitBackup(t, "db1", stoppedAt, 5<<30)
	}

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Anomalies) == 0 {
		t.Errorf("expected at least one anomaly; got none\nbreakdown: %+v", rep.Deployments[0])
	} else if rep.Anomalies[0].Reason != "sudden_uptick" {
		t.Errorf("Anomaly[0].Reason = %q", rep.Anomalies[0].Reason)
	}
}

// TestGenerate_AnomalySkipped: SkipAnomalies suppresses the
// detection pass.
func TestGenerate_AnomalySkipped(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(8+i*5)*24*time.Hour), 1<<30)
	}
	for i := 0; i < 3; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(i*2+1)*24*time.Hour), 5<<30)
	}

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier:      w.verifier,
		Now:           now,
		SkipAnomalies: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Anomalies) != 0 {
		t.Errorf("Anomalies = %v, want empty", rep.Anomalies)
	}
}

// TestGenerate_FleetSkipped: SkipFleet drops the fleet section.
func TestGenerate_FleetSkipped(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*24*time.Hour), 1<<30)

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier:  w.verifier,
		Now:       now,
		SkipFleet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fleet != nil {
		t.Errorf("Fleet = %+v, want nil", rep.Fleet)
	}
}

// TestGenerate_CustomHorizons: operator-supplied horizons override
// the default set.
func TestGenerate_CustomHorizons(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*24*time.Hour), 1<<30)
	w.commitBackup(t, "db1", now.Add(-2*24*time.Hour), 1<<30)
	w.commitBackup(t, "db1", now.Add(-3*24*time.Hour), 1<<30)

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
		Now:      now,
		Horizons: []time.Duration{
			7 * 24 * time.Hour,
			60 * 24 * time.Hour,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Deployments[0]
	if len(d.Projections) != 2 {
		t.Errorf("Projections = %d, want 2", len(d.Projections))
	}
	if d.Projections[0].HorizonName != "7d" {
		t.Errorf("Projections[0].HorizonName = %q", d.Projections[0].HorizonName)
	}
}

// TestGenerate_ContextCancellation: returns ctx.Err.
func TestGenerate_ContextCancellation(t *testing.T) {
	w := setupWorld(t)
	w.commitBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 1<<30)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := forecast.Generate(ctx, w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("expected ctx error")
	}
}

// TestRenderMarkdown_HappyPath: every section's heading appears.
func TestRenderMarkdown_HappyPath(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(i+1)*24*time.Hour), 1<<30)
	}

	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier:        w.verifier,
		Now:             now,
		PricePerGBMonth: 0.023,
		Currency:        "USD",
		PricingModel:    "s3-standard",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := forecast.RenderMarkdown(&sb, rep); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage forecast report",
		"## Fleet projection",
		"## Per-deployment forecast",
		"## Cost projection",
		"## Growth anomalies",
		"## Methodology notes",
		"### `db1`",
		"USD 0.0230",
		"s3-standard",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderMarkdown_Empty: a fresh repo renders cleanly with
// "(no deployments)" / "skipped" placeholders. No crashes.
func TestRenderMarkdown_Empty(t *testing.T) {
	w := setupWorld(t)
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := forecast.RenderMarkdown(&sb, rep); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "no deployments") {
		t.Errorf("expected 'no deployments' placeholder:\n%s", out)
	}
}

// TestRenderMarkdown_Nil_Errors: rendering nil returns a clear error.
func TestRenderMarkdown_Nil_Errors(t *testing.T) {
	var sb strings.Builder
	if err := forecast.RenderMarkdown(&sb, nil); err == nil {
		t.Error("expected error for nil report")
	}
}

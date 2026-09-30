package forecast_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/forecast"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

const gib = int64(1 << 30)

// commitIncremental plants an incremental_lsn manifest chained to
// parent whose logical size is `bytes` — the size of the changed
// relation files only, which is what a PG 17 INCREMENTAL backup
// records.
func (w *forecastWorld) commitIncremental(t *testing.T, deployment, parent string, stoppedAt time.Time, bytes int64) {
	t.Helper()
	cas := casdefault.New(w.sp)
	info, err := cas.PutChunk(context.Background(), []byte(strings.Repeat("y", 16)))
	if err != nil {
		t.Fatal(err)
	}
	id := deployment + ".incremental_lsn." + stoppedAt.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeIncremental,
		ParentBackupID:   parent,
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
			Path: "data/" + id, Size: bytes, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: bytes}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestGenerate_ConstantSizeDatabase_ProjectsFlat is the regression
// for the running-total model: a 100 GiB database backed up in full
// every day for 90 days used to regress the CUMULATIVE sum of the
// backup sizes (≈100 GiB/day of "growth") and project +36.5 TiB over
// a year. The database did not grow; the projection must not either.
func TestGenerate_ConstantSizeDatabase_ProjectsFlat(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 90; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(90-i)*24*time.Hour), 100*gib)
	}
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Deployments[0]
	if d.BytesPerDay != 0 {
		t.Errorf("BytesPerDay = %v, want 0 for a constant-size database", d.BytesPerDay)
	}
	for _, p := range d.Projections {
		if p.ProjectedBytes != 100*gib {
			t.Errorf("%s projection = %d bytes (%.1f GiB), want flat 100 GiB",
				p.HorizonName, p.ProjectedBytes, float64(p.ProjectedBytes)/float64(gib))
		}
	}
	if len(rep.Anomalies) != 0 {
		t.Errorf("constant database flagged as growth anomaly: %+v", rep.Anomalies)
	}
}

// TestGenerate_GrowingDatabase_SlopeIsPerBackupGrowth: a database
// that grows 1 GiB/day projects +1 GiB/day, not the slope of the
// running total of its backup sizes.
func TestGenerate_GrowingDatabase_SlopeIsPerBackupGrowth(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		w.commitBackup(t, "db1", now.Add(-time.Duration(30-i)*24*time.Hour), 50*gib+int64(i)*gib)
	}
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Deployments[0]
	if math.Abs(d.BytesPerDay-float64(gib)) > float64(gib)/100 {
		t.Errorf("BytesPerDay = %.3f GiB/day, want ~1", d.BytesPerDay/float64(gib))
	}
	for _, p := range d.Projections {
		if p.HorizonName != "365d" {
			continue
		}
		want := d.CurrentBytes + 365*gib
		if diff := p.ProjectedBytes - want; diff > gib || diff < -gib {
			t.Errorf("365d projection = %.1f GiB, want ~%.1f GiB",
				float64(p.ProjectedBytes)/float64(gib), float64(want)/float64(gib))
		}
	}
}

// TestGenerate_IncrementalsDoNotDistortSize: a 100 GiB database with
// weekly fulls and daily 1 GiB incrementals is still a 100 GiB
// database. An incremental's logical size is the changed-files delta,
// not the database size, so it must neither drag the regression nor
// become "current size".
func TestGenerate_IncrementalsDoNotDistortSize(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	start := now.Add(-56 * 24 * time.Hour)
	for week := 0; week < 8; week++ {
		fullAt := start.Add(time.Duration(week*7) * 24 * time.Hour)
		parent := w.commitBackup(t, "db1", fullAt, 100*gib)
		for d := 1; d < 7; d++ {
			w.commitIncremental(t, "db1", parent, fullAt.Add(time.Duration(d)*24*time.Hour), gib)
		}
	}
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Deployments[0]
	if d.CurrentBytes != 100*gib {
		t.Errorf("CurrentBytes = %.1f GiB, want 100 (latest full, not latest incremental)",
			float64(d.CurrentBytes)/float64(gib))
	}
	if d.BytesPerDay != 0 {
		t.Errorf("BytesPerDay = %v, want 0", d.BytesPerDay)
	}
	if d.CurrentManifests != 56 {
		t.Errorf("CurrentManifests = %d, want 56 (incrementals still counted)", d.CurrentManifests)
	}
}

// TestGenerate_WeeklySchedule_NoFalseAnomaly is the regression for
// the anomaly fencepost: the tail window of a weekly schedule holds a
// single backup, whose span was floored to 1 day, so the "recent
// rate" was a whole backup's bytes per day against a baseline of
// summed bytes over ~11 weeks — every weekly deployment read as a
// multi-× sudden_uptick. Steady growth must not be an anomaly.
func TestGenerate_WeeklySchedule_NoFalseAnomaly(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		at := now.Add(-time.Duration(84-i*7)*24*time.Hour + time.Hour)
		w.commitBackup(t, "weekly", at, 100*gib+int64(i)*gib) // +1 GiB/week, steady
	}
	rep, err := forecast.Generate(context.Background(), w.sp, w.meta, w.repoURL, forecast.Options{
		Verifier: w.verifier, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Anomalies) != 0 {
		t.Errorf("steady weekly growth flagged: %+v", rep.Anomalies)
	}
}

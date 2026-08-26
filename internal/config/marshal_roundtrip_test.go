package config_test

// marshal_roundtrip_test.go — everything an operator writes must
// survive `pg_hardstorage init` rewriting their file.
//
// Marshal is how init emits the merged configuration back to disk. A
// field with a missing or wrong yaml tag is dropped there SILENTLY:
// the file still parses, the tool still starts, and whatever the field
// controlled simply stops applying. Retention policy, WORM mode, a
// KEKRef, a Patroni slot — each fails as "it stopped doing the thing"
// long after the rewrite.
//
// Marshal had no test at all until issue #45's release prep added one
// for the kms section. This covers the rest, and carries the same
// completeness canary the merge test uses: a new field that this test
// does not populate fails, so nobody adds one without deciding whether
// it round-trips.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// fullDeployment is a DeploymentConfig with every field non-zero, so
// the round trip below actually exercises each one.
func fullDeployment(useSlots bool) config.DeploymentConfig {
	// patroni.slot and patroni.slots are mutually exclusive by design
	// (Mechanism 2 picks one, Mechanism 3 the other), so "populate
	// every field" is not expressible in a single value. Each variant
	// carries one of them and the round trip runs over both.
	slot, slots := "s", []config.PatroniSlot(nil)
	if useSlots {
		slot, slots = "", []config.PatroniSlot{{Name: "a", Role: "leader"}}
	}
	return config.DeploymentConfig{
		PGConnection:   "postgres://u@h:5432/db",
		Repo:           "s3://bucket/prefix?region=eu-central-1",
		Tenant:         "acme",
		KEKRef:         "aws-kms://alias/prod",
		Classification: "restricted",
		Residency:      []string{"eu", "ch"},
		Schedule: config.DeploymentSchedule{
			Backup:      config.ScheduleSpec{Every: "6h"},
			Rotate:      config.ScheduleSpec{DailyAt: "04:00"},
			Drill:       config.ScheduleSpec{DailyAt: "03:00"},
			AuditAnchor: config.ScheduleSpec{Every: "30m"},
		},
		Retention: config.RetentionConfig{
			Policy: "gfs", KeepDaily: 9, KeepWeekly: 8,
			KeepMonthly: 60, KeepYearly: 20, KeepFor: "720h", KeepFulls: 7,
		},
		SLO: config.SLOConfig{RPOSeconds: 3600, RTOSeconds: 7200},
		TDE: config.TDEConfig{Enabled: true, Engine: "pg_tde", KeyRef: "ref-1"},
		Patroni: config.PatroniConfig{
			URL: "http://p:8008", User: "u", Password: "pw",
			PasswordFile: "/pw",
			Slot:         slot,
			Slots:        slots,
			Interval:     "10s",
		},
		AllowUnenforceableLease: true,
		// Scheduled-drill settings. The mapping is the field that
		// matters: a deployment with non-default tablespaces cannot be
		// drilled safely without it, so losing it in a round trip would
		// turn a safe scheduled drill into a refused one.
		Drill: config.DrillConfig{
			TablespaceMapping: []string{"/srv/live/ts_fast=/var/tmp/drill/ts_fast"},
			SkipVerify:        true,
			TempBase:          "/var/tmp/drill",
		},
	}
}

// TestMarshal_DeploymentSurvivesRoundTrip is the property: what goes in
// comes back out.
func TestMarshal_DeploymentSurvivesRoundTrip(t *testing.T) {
	for _, variant := range []struct {
		name     string
		useSlots bool
	}{
		{"patroni.slot", false},
		{"patroni.slots", true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			roundTripDeployment(t, fullDeployment(variant.useSlots))
		})
	}
}

func roundTripDeployment(t *testing.T, want config.DeploymentConfig) {
	t.Helper()

	// Canary: a field left zero is a field the round trip never checks,
	// so the test would silently stop covering it. Patroni is exempt —
	// its two slot forms are mutually exclusive and each variant above
	// carries one.
	wv := reflect.ValueOf(want)
	for i := 0; i < wv.NumField(); i++ {
		if wv.Type().Field(i).Name == "Patroni" {
			continue
		}
		if wv.Field(i).IsZero() {
			t.Fatalf("test gap: DeploymentConfig.%s is zero here, so the round trip does "+
				"not exercise it — populate it, and check it survives Marshal",
				wv.Type().Field(i).Name)
		}
	}

	cfg := config.Config{
		Schema:      config.Schema,
		Deployments: map[string]config.DeploymentConfig{"db1": want},
		KMS: config.KMSConfig{Providers: []config.KMSProvider{
			{KEKRef: "aws-kms://alias/prod", Config: map[string]any{"region": "eu-central-1"}},
		}},
	}

	out, err := config.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatalf("re-loading Marshal's own output failed: %v\n\ninit writes this file; if "+
			"the loader rejects it, init produces a config the tool cannot read:\n%s",
			err, out)
	}

	got, ok := res.Config.Deployments["db1"]
	if !ok {
		t.Fatalf("deployment db1 did not survive the round trip:\n%s", out)
	}

	if !reflect.DeepEqual(got, want) {
		gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
		tt := gv.Type()
		for i := 0; i < tt.NumField(); i++ {
			g, w := gv.Field(i).Interface(), wv.Field(i).Interface()
			if !reflect.DeepEqual(g, w) {
				t.Errorf("DeploymentConfig.%s did not survive Marshal → Load:\n  got  %+v\n"+
					"  want %+v\n\nA dropped field does not fail loudly — the file still "+
					"parses and whatever it controlled just stops applying",
					tt.Field(i).Name, g, w)
			}
		}
	}
}

// TestMarshal_TopLevelSectionsSurvive covers the sections beside
// deployments, which init rewrites just as readily.
func TestMarshal_TopLevelSectionsSurvive(t *testing.T) {
	cfg := config.Config{
		Schema:      config.Schema,
		Deployments: map[string]config.DeploymentConfig{"db1": fullDeployment(true)},
		KMS: config.KMSConfig{Providers: []config.KMSProvider{
			{KEKRef: "azure-kv://vault/key", Config: map[string]any{"use_fips_mode": true}},
			{KEKRef: "aws-kms://alias/other", Config: map[string]any{"region": "us-east-1"}},
		}},
	}

	out, err := config.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := config.Load(pathsForTempDir(t, dir))
	if err != nil {
		t.Fatalf("re-load: %v\n%s", err, out)
	}

	if got := len(res.Config.KMS.Providers); got != 2 {
		t.Fatalf("kms.providers = %d after round trip, want 2:\n%s", got, out)
	}
	for _, p := range cfg.KMS.Providers {
		rt := res.Config.KMSProviderConfig(p.KEKRef)
		if !reflect.DeepEqual(rt, p.Config) {
			t.Errorf("provider %s config = %+v after round trip, want %+v — a dropped "+
				"provider setting means the next backup opens the KMS with different "+
				"credentials than the operator wrote", p.KEKRef, rt, p.Config)
		}
	}
}

package config

import (
	"reflect"
	"testing"
)

// mergeDeployment used to copy only connection/repo/tenant/schedule
// and drop everything else from an overlay that re-declared a
// deployment (the documented conf.d override pattern): the
// operator's retention drop-in vanished silently and the scheduled
// rotate pruned with the DEFAULT policy — policy-mandated backups
// soft-deleted years early. This test overlays a fully-populated
// deployment onto a differently-populated base and asserts every
// field comes out as the overlay's value, so a future field added to
// DeploymentConfig without a merge arm fails here instead of
// silently disappearing in production.
func TestMergeDeployment_CarriesEveryField(t *testing.T) {
	encOn := true
	base := DeploymentConfig{
		PGConnection:   "postgres://base@h/db",
		Repo:           "file:///base",
		Tenant:         "base",
		KEKRef:         "local:default",
		Classification: "internal",
	}
	overlay := DeploymentConfig{
		PGConnection: "postgres://over@h/db",
		Repo:         "file:///over",
		Tenant:       "over",
		KEKRef:       "aws-kms://alias/over",
		Schedule: DeploymentSchedule{
			Backup:      ScheduleSpec{Every: "6h"},
			Rotate:      ScheduleSpec{DailyAt: "04:00"},
			Drill:       ScheduleSpec{DailyAt: "03:00"},
			AuditAnchor: ScheduleSpec{Every: "30m"},
		},
		Drill: DrillConfig{
			TablespaceMapping:    []string{"/srv/over/ts=/var/tmp/over/ts"},
			TablespaceMappingAll: "/var/tmp/over/all",
			SkipVerify:           true,
			TempBase:             "/var/tmp/over",
		},
		Retention:      RetentionConfig{Policy: "gfs", KeepDaily: Keep(9), KeepWeekly: Keep(8), KeepMonthly: Keep(60), KeepYearly: Keep(20), KeepFor: "720h", KeepFulls: Keep(7)},
		Classification: "restricted",
		Residency:      []string{"eu"},
		SLO:            SLOConfig{RPOSeconds: 3600, RTOSeconds: 7200},
		TDE:            TDEConfig{Enabled: true, Engine: "pg_tde", KeyRef: "ref-1"},
		Patroni: PatroniConfig{
			URL: "http://p:8008", User: "u", Password: "pw",
			PasswordFile: "/pw", Slot: "s", Slots: []PatroniSlot{{Name: "a", Role: "leader"}},
			Interval: "10s",
		},
		AllowUnenforceableLease: true,
		Backup:                  BackupConfig{IncludeWAL: true, Fast: true, Encrypt: &encOn, ChunkSize: "1MiB", Timeout: "36h"},
	}

	got := mergeDeployment(base, overlay)

	if !reflect.DeepEqual(got, overlay) {
		gv, ov := reflect.ValueOf(got), reflect.ValueOf(overlay)
		tt := gv.Type()
		for i := 0; i < tt.NumField(); i++ {
			if !reflect.DeepEqual(gv.Field(i).Interface(), ov.Field(i).Interface()) {
				t.Errorf("field %s dropped by mergeDeployment: got %+v, want %+v",
					tt.Field(i).Name, gv.Field(i).Interface(), ov.Field(i).Interface())
			}
		}
	}

	// Completeness canary: if DeploymentConfig gains a field this
	// test's overlay doesn't populate, force a look here. Every field
	// of the overlay must be non-zero so the DeepEqual above actually
	// exercises its merge arm.
	ov := reflect.ValueOf(overlay)
	for i := 0; i < ov.NumField(); i++ {
		if ov.Field(i).IsZero() {
			t.Errorf("test gap: overlay field %s is zero — populate it here AND add a merge arm in mergeDeployment", ov.Type().Field(i).Name)
		}
	}
}

// TestMergeDeployment_PartialRetentionPreservesBaseDimensions: a
// conf.d drop-in that overrides ONE retention dimension must keep the
// base's other dimensions. Under the old wholesale replace they were
// zeroed, and the agent then silently reverted them to
// hardcoded defaults — a base keep_daily:30 became 7, pruning dailies
// 8-30 early. Field-by-field merge (matching the Drill block) fixes it.
func TestMergeDeployment_PartialRetentionPreservesBaseDimensions(t *testing.T) {
	base := DeploymentConfig{
		Retention: RetentionConfig{
			Policy: "gfs", KeepDaily: Keep(30), KeepWeekly: Keep(8), KeepMonthly: Keep(12), KeepYearly: Keep(5),
		},
		TDE: TDEConfig{Enabled: true, Engine: "pg_tde", KeyRef: "k1"},
		SLO: SLOConfig{RPOSeconds: 300, RTOSeconds: 3600},
	}
	// Drop-in overrides ONLY keep_monthly, TDE KeyRef, and RPO.
	overlay := DeploymentConfig{
		Retention: RetentionConfig{KeepMonthly: Keep(60)},
		TDE:       TDEConfig{KeyRef: "k2"},
		SLO:       SLOConfig{RPOSeconds: 120},
	}
	got := mergeDeployment(base, overlay)

	want := RetentionConfig{Policy: "gfs", KeepDaily: Keep(30), KeepWeekly: Keep(8), KeepMonthly: Keep(60), KeepYearly: Keep(5)}
	if !got.Retention.Equal(want) {
		t.Errorf("retention: partial override lost base dimensions\n got: %+v\nwant: %+v", got.Retention, want)
	}
	// An overlay's explicit 0 is an override, not "inherit": it switches
	// the bucket off, and the base value must not shine through.
	zero := mergeDeployment(base, DeploymentConfig{Retention: RetentionConfig{KeepWeekly: Keep(0)}})
	if w := zero.Retention.KeepWeekly; w == nil || *w != 0 {
		t.Errorf("retention: overlay keep_weekly: 0 did not override the base 8: %v", w)
	}
	if base.Retention.KeepWeekly == nil || *base.Retention.KeepWeekly != 8 {
		t.Errorf("retention: merge wrote through to the base config")
	}
	if got.TDE != (TDEConfig{Enabled: true, Engine: "pg_tde", KeyRef: "k2"}) {
		t.Errorf("TDE: partial override lost base fields: %+v", got.TDE)
	}
	if got.SLO != (SLOConfig{RPOSeconds: 120, RTOSeconds: 3600}) {
		t.Errorf("SLO: partial override lost base RTO: %+v", got.SLO)
	}
}

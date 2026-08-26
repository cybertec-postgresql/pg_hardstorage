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
			TablespaceMapping: []string{"/srv/over/ts=/var/tmp/over/ts"},
			SkipVerify:        true,
			TempBase:          "/var/tmp/over",
		},
		Retention:      RetentionConfig{Policy: "gfs", KeepDaily: 9, KeepWeekly: 8, KeepMonthly: 60, KeepYearly: 20, KeepFor: "720h", KeepFulls: 7},
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

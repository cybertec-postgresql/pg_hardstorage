package validate

import (
	"reflect"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

// TestMergeLoadStatsCarriesEveryField pins mergeLoadStats against the
// shape of report.LoadStats itself.
//
// mergeLoadStats copies field by field, so a field added to LoadStats
// and not added here is silently dropped on the way to the report:
// StopWALStream computes it, mergeLoadStats ignores it, and the JSON
// shows the zero value. Nothing fails — the number is simply wrong,
// and wrong in the direction that looks like "nothing happened".
//
// That is exactly what happened to WALStreamRestarts,
// WALStreamUpAtStop and WALStreamDownFor: a full 2 h soak reported
// zero sidecar restarts on every cell, which was indistinguishable
// from a sidecar that never needed restarting. The diagnostic added
// to answer "was the streamer interrupted, or did it fall behind?"
// answered neither.
//
// Reflection rather than a hand-written list: a hand-written list has
// the same failure mode as the function it is checking.
func TestMergeLoadStatsCarriesEveryField(t *testing.T) {
	src := &report.LoadStats{}
	v := reflect.ValueOf(src).Elem()
	typ := v.Type()

	// Fill every field with a distinctive non-zero value.
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			f.SetInt(int64(i + 1))
		case reflect.Float32, reflect.Float64:
			f.SetFloat(float64(i) + 1.5)
		case reflect.String:
			f.SetString("v" + typ.Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		default:
			t.Fatalf("LoadStats.%s has kind %s, which this test does not know how to fill — extend the switch",
				typ.Field(i).Name, f.Kind())
		}
	}

	cr := &report.CellReport{}
	mergeLoadStats(cr, src)

	if cr.LoadStats == nil {
		t.Fatal("mergeLoadStats produced no LoadStats")
	}
	got := reflect.ValueOf(cr.LoadStats).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := typ.Field(i).Name
		want, have := v.Field(i).Interface(), got.Field(i).Interface()
		if !reflect.DeepEqual(want, have) {
			t.Errorf("LoadStats.%s did not survive mergeLoadStats: got %v, want %v\n"+
				"add it to mergeLoadStats — a field it does not copy reaches the report as its zero value",
				name, have, want)
		}
	}
}

// TestMergeLoadStatsDoesNotClobber covers the other half of the
// contract: the merge is called once per sidecar (WAL streamer, then
// sustained writer), and the second call must not erase what the
// first contributed. Every guard is "copy when the source is
// non-zero" for that reason.
func TestMergeLoadStatsDoesNotClobber(t *testing.T) {
	cr := &report.CellReport{}
	mergeLoadStats(cr, &report.LoadStats{
		WALStreamRan:      true,
		WALStreamRestarts: 4,
		WALStreamUpAtStop: true,
		WALRepoLagBytes:   1024,
	})
	// The sustained-writer stats arrive second and know nothing about
	// the streamer.
	mergeLoadStats(cr, &report.LoadStats{
		SustainedWriterRan: true,
		TPSAvg:             12.5,
	})

	if cr.LoadStats.WALStreamRestarts != 4 {
		t.Errorf("WALStreamRestarts clobbered by the second merge: got %d, want 4",
			cr.LoadStats.WALStreamRestarts)
	}
	if !cr.LoadStats.WALStreamUpAtStop {
		t.Error("WALStreamUpAtStop clobbered by the second merge")
	}
	if cr.LoadStats.WALRepoLagBytes != 1024 {
		t.Errorf("WALRepoLagBytes clobbered: got %d, want 1024", cr.LoadStats.WALRepoLagBytes)
	}
	if !cr.LoadStats.SustainedWriterRan || cr.LoadStats.TPSAvg != 12.5 {
		t.Error("the second merge's own fields did not land")
	}
}

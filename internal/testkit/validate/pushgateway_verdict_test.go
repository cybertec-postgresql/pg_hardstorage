package validate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// The pass gauge must reach the report's verdict. It went to 0 only on
// verify_failed and setup_failed, so a cell that failed its backups,
// its seed or its sidecars — or failed retention — showed pass=1 in
// Grafana for a run whose report said FAIL.
func TestPushgatewayEmitter_PassGaugeMatchesReportVerdict(t *testing.T) {
	for name, cell := range map[string]*validate.FakeCellRuntime{
		"backup": {NameStr: "c", BackupErr: errors.New("backup: boom")},
		"seed":   {NameStr: "c", SeedErr: errors.New("seed: boom")},
		"wal":    {NameStr: "c", WALStreamErr: errors.New("wal stream: boom")},
	} {
		t.Run(name, func(t *testing.T) {
			validate.ResetForTesting()
			srv := newPushgatewayServer(t)
			e := validate.NewPushgatewayEmitter(srv.server.URL, "job", "inst")
			rep, err := validate.Run(context.Background(), validate.RunOptions{
				Seed: 1, Duration: 50 * time.Millisecond,
				Loop:  validate.LoopOptions{BackupEvery: 1, RetentionInterval: -1},
				Cells: []validate.CellRuntime{cell}, OnEvent: e.OnEvent,
			})
			if err != nil {
				t.Fatal(err)
			}
			if rep.OverallPass {
				t.Fatal("fixture: the report must say FAIL")
			}
			if err := e.Push(context.Background()); err != nil {
				t.Fatal(err)
			}
			body := srv.calls()[0].Body
			if !strings.Contains(body, `pg_hardstorage_validate_pass{cell="c",os="",pg=""} 0`) {
				t.Errorf("report says FAIL but the pass gauge does not:\n%s", body)
			}
		})
	}
}

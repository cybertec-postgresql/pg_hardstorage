package cli

import (
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression (concurrency audit): the sysid guard ran only at startup;
// a reconnect that lands on a DIFFERENT cluster (failover to a clone,
// repointed VIP) archived foreign WAL into the same lineage. The retry
// loop now pins the first attempt's sysid and refuses a change.
func TestCheckSysIDContinuity(t *testing.T) {
	var pinned string
	// First attempt pins.
	if err := checkSysIDContinuity(&pinned, "7000000000000000001", "db1", false); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	// Same cluster on reconnect: fine.
	if err := checkSysIDContinuity(&pinned, "7000000000000000001", "db1", false); err != nil {
		t.Fatalf("same sysid: %v", err)
	}
	// DIFFERENT cluster: permanent structured refusal.
	err := checkSysIDContinuity(&pinned, "7999999999999999999", "db1", false)
	if err == nil {
		t.Fatal("sysid change accepted — foreign WAL would interleave into the lineage")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "wal.system_identifier_changed" {
		t.Errorf("error = %v, want wal.system_identifier_changed", err)
	}
	// Operator override.
	if err := checkSysIDContinuity(&pinned, "7999999999999999999", "db1", true); err != nil {
		t.Errorf("allowChange=true still refused: %v", err)
	}
}

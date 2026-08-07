package cli

// wal_standby_source_test.go — the escape hatch and the short-circuits
// on the primary-source guard.
//
// The guard itself is proved against a real demoted node by
// TestWalStream_PinnedToADemotedNode (integration && patroni), which is
// the only place the interesting behaviour exists. What is worth
// pinning cheaply here is the surrounding logic, because each branch
// is a way for the guard to quietly stop guarding:
//
//   - --allow-standby-source must actually work, or operators who
//     archive from a replica on purpose will find a worse workaround;
//   - an empty --pg-connection must not make the guard error;
//   - the probe failing must not block WAL archiving, since refusing to
//     stream is itself a way to lose WAL.

import (
	"context"
	"strings"
	"testing"
)

// TestGuardSourceIsPrimary_AllowFlagSkipsTheProbe: with the escape
// hatch set, the guard must not even dial. The DSN below points at a
// closed port, so if the probe ran at all this would be slow and would
// emit the probe-failed warning.
func TestGuardSourceIsPrimary_AllowFlagSkipsTheProbe(t *testing.T) {
	d, buf := captureDispatcher(t)
	err := guardSourceIsPrimary(context.Background(), d, walStreamOptions{
		deployment:         "db1",
		pgConn:             "postgres://127.0.0.1:1/x",
		allowStandbySource: true,
	})
	if err != nil {
		t.Fatalf("--allow-standby-source did not permit streaming: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "recovery_check_failed") {
		t.Errorf("the guard probed the server despite --allow-standby-source:\n%s\n\n"+
			"The flag says the operator has already decided; dialling anyway costs a "+
			"connection per attempt and emits a warning about a question nobody asked.", got)
	}
}

// TestGuardSourceIsPrimary_NoConnIsNotAnError: unit paths and
// --no-slot can reach this with no DSN.
func TestGuardSourceIsPrimary_NoConnIsNotAnError(t *testing.T) {
	d, buf := captureDispatcher(t)
	if err := guardSourceIsPrimary(context.Background(), d, walStreamOptions{deployment: "db1"}); err != nil {
		t.Fatalf("guard errored with no --pg-connection: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "recovery_check_failed") {
		t.Errorf("warned about a probe it could not have run:\n%s", got)
	}
}

// TestGuardSourceIsPrimary_UnreachableFailsOpenButWarns is the posture
// decision, and it is a genuine trade-off rather than an obvious call.
//
// Failing CLOSED here would mean an inconclusive probe stops WAL
// archiving — trading a possible silent gap for a certain one. Failing
// OPEN keeps the WAL flowing but risks archiving from a standby
// unnoticed, which is the bug this guard exists for. The tiebreaker is
// that the connection has just served IDENTIFY_SYSTEM, so a failure
// here is far more likely transient than meaningful.
//
// Open, therefore — but never silent.
func TestGuardSourceIsPrimary_UnreachableFailsOpenButWarns(t *testing.T) {
	d, buf := captureDispatcher(t)
	err := guardSourceIsPrimary(context.Background(), d, walStreamOptions{
		deployment: "db1",
		pgConn:     "postgres://127.0.0.1:1/x",
	})
	if err != nil {
		t.Fatalf("an unreachable probe blocked streaming: %v\n\n"+
			"Refusing to archive because we could not answer a diagnostic question trades a "+
			"possible gap for a certain one.", err)
	}
	got := buf.String()
	if !strings.Contains(got, "recovery_check_failed") {
		t.Fatalf("failed open and said nothing:\n%s\n\n"+
			"If the source really is a standby, this warning is the only signal the operator "+
			"gets before the archive starts trailing the primary.", got)
	}
	if !strings.Contains(got, "second-hand") && !strings.Contains(got, "fall permanently behind") {
		t.Errorf("the warning does not explain the consequence:\n%s", got)
	}
}

// TestWalStreamCmd_ExposesTheEscapeHatch: the guard's remediation text
// names --allow-standby-source, so the flag has to exist under exactly
// that name. A refusal that recommends a flag the binary does not have
// is worse than no advice.
func TestWalStreamCmd_ExposesTheEscapeHatch(t *testing.T) {
	c := newWalStreamCmd()
	if c.Flags().Lookup("allow-standby-source") == nil {
		t.Fatal("`wal stream` has no --allow-standby-source flag, but guardSourceIsPrimary's " +
			"suggestion tells the operator to pass it")
	}
}

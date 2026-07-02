package postverify

import "context"

// StageForRecoveryForTest exposes the unexported stageForRecovery so
// the _test package can lock the issue-#56 contract: a PITR-armed
// restore must NOT have postverify append `recovery_target = 'immediate'`
// on top of the operator's recovery_target_* block.
func StageForRecoveryForTest(dataDir, repoURL, deployment, agentBinary string, pitrTargetArmed bool) error {
	return stageForRecovery(dataDir, repoURL, deployment, agentBinary, pitrTargetArmed)
}

// PickProbeDSNForTest exposes pickProbeDSN so the issue #85
// regression test can drive the socket-then-tcp fallback without
// a real PG.  The caller passes a stub `psql` path (a small shell
// script) that returns the desired exit code per DSN.
func PickProbeDSNForTest(ctx context.Context, psql, socketDSN, tcpDSN string) (string, string, error) {
	return pickProbeDSN(ctx, psql, socketDSN, tcpDSN)
}

// StartWithStopGuardForTest mirrors Verify's start sequencing: it
// arms the stop guard BEFORE attempting the start, then runs the
// start.  Used by the bug-52 regression test to prove a FAILED
// start still triggers the stop (no leaked postmaster).  Returns
// the start error (nil on success).  The stop guard runs when
// this function returns, exactly as the deferred guard does in
// Verify.
func StartWithStopGuardForTest(ctx context.Context, pgCtl, dataDir string, startArgs []string) (err error) {
	defer registerStopGuard(pgCtl, dataDir)()
	_, err = runStart(ctx, pgCtl, startArgs)
	return err
}

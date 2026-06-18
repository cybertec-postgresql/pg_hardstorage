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

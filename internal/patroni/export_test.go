package patroni

import "context"

// SetSystemIDProbe overrides the follower's system-identifier probe so
// the cluster-identity-mismatch defence (bug #24) can be exercised
// without a live Patroni surfacing a system_identifier field. Test-only.
func (f *Follower) SetSystemIDProbe(probe func(ctx context.Context) (string, bool, error)) {
	f.mu.Lock()
	f.sysIDProbe = probe
	f.mu.Unlock()
}

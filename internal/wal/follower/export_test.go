package follower

// ExpectedSystemIDOf exposes the coordinator's configured expected
// system identifier so the wiring test can assert the value survives
// New and is available to hand to patroni.Start. The field was dead in
// production for want of exactly this plumbing.
func ExpectedSystemIDOf(c *Coordinator) string { return c.opts.ExpectedSystemID }

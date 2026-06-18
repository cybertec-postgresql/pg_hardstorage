package watch

// EventMsgForTest exposes the unexported eventMsg type to the
// _test package so we can drive Update() with synthetic events
// without spinning up the follower goroutine.
func EventMsgForTest(ev Event) interface{} { return eventMsg(ev) }

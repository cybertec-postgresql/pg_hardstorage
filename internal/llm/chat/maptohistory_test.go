package chat

import "testing"

// TestMapActionToHistory_SessionStartedIsSystemRole pins the dead-consumer
// fix: the session-start event is emitted as "llm.session_started" (see
// Session.emit at session_started), but mapActionToHistory used to match a
// stale "llm.bootstrap" action that nothing emits. The real event therefore
// fell through to the assistant default and the session-start history row
// was mislabelled as role "assistant" instead of "system".
func TestMapActionToHistory_SessionStartedIsSystemRole(t *testing.T) {
	role, op := mapActionToHistory("llm.session_started")
	if role != "system" {
		t.Errorf("session_started role = %q, want \"system\" (the dead llm.bootstrap case let it fall to the assistant default)", role)
	}
	if op != "session_started" {
		t.Errorf("session_started op = %q, want \"session_started\"", op)
	}

	// The stale "llm.bootstrap" action is no longer special-cased — nothing
	// emits it, so it now falls to the assistant default like any unknown
	// action. This guards against the rename being a copy (adding the new
	// case while leaving the dead one).
	if r, _ := mapActionToHistory("llm.bootstrap"); r != "assistant" {
		t.Errorf("llm.bootstrap should hit the default (assistant); got role %q", r)
	}

	// Sanity: the other live mappings are unchanged.
	for _, tc := range []struct{ action, role, op string }{
		{"llm.prompt", "user", "prompt"},
		{"llm.response", "assistant", "response"},
		{"llm.tool_result", "tool", "tool_result"},
	} {
		if r, o := mapActionToHistory(tc.action); r != tc.role || o != tc.op {
			t.Errorf("mapActionToHistory(%q) = (%q,%q), want (%q,%q)", tc.action, r, o, tc.role, tc.op)
		}
	}
}

// TestMapActionToHistory_GateExecuteArmsRemoved pins the removal of the dead
// llm.gate / llm.execute arms (findings #4/#5): nothing in the chat package
// ever emits those actions, so they must fall to the assistant default like
// any unknown action — op is the full action name, not the stale short label
// ("gate"/"execute") the dead arms returned.
func TestMapActionToHistory_GateExecuteArmsRemoved(t *testing.T) {
	for _, action := range []string{"llm.gate", "llm.execute"} {
		role, op := mapActionToHistory(action)
		if role != "assistant" || op != action {
			t.Errorf("mapActionToHistory(%q) = (%q,%q), want default (\"assistant\",%q) — the dead special-case arm should be gone",
				action, role, op, action)
		}
	}
}

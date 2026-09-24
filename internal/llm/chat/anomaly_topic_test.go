package chat

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// The anomaly detector refuses a high-risk verb unless the operator
// raised it.  Nothing ever fed the operator's words into it, so every
// delete / rotate / gc / purge / shred / wipe command was refused as
// severe regardless of what was asked.  Session.Ask must merge the
// operator's prompt into the detector — and ONLY the operator's prompt:
// a verb the model (or injected tool output) mentions must not put
// itself on-topic.
func TestAsk_FeedsOperatorPromptIntoAnomalyTopics(t *testing.T) {
	det := &safety.AnomalyDetector{RecentTopicTokens: map[string]struct{}{}}
	prov := &scriptedProvider{turns: []scriptedTurn{
		{text: "Sure. I could also shred the tenant key."},
		{text: "ok"},
	}}
	s := &Session{Provider: prov, Tools: tools.NewRegistry(), Anomaly: det}
	if _, err := s.Ask(context.Background(), "please rotate the KEK for db1"); err != nil {
		t.Fatal(err)
	}
	if dec := det.Score("pg_hardstorage kms rotate db1"); dec.Score != safety.ScoreNormal {
		t.Errorf("operator asked to rotate; got %v (%s)", dec.Score, dec.Reason)
	}
	if dec := det.Score("pg_hardstorage kms shred --tenant t"); dec.Score != safety.ScoreSevere {
		t.Errorf("only the model mentioned shred; got %v, want severe", dec.Score)
	}
	// Topics accumulate across turns.
	if _, err := s.Ask(context.Background(), "and run gc afterwards"); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"pg_hardstorage kms rotate db1", "pg_hardstorage repo gc s3://r/"} {
		if dec := det.Score(cmd); dec.Score != safety.ScoreNormal {
			t.Errorf("%q after two turns: %v", cmd, dec.Score)
		}
	}
}

type recordingEmitter struct{ events []AuditEvent }

func (r *recordingEmitter) Emit(ev AuditEvent) { r.events = append(r.events, ev) }

// Emit is the exported hook the CLI uses to put execute_command gate
// and anomaly verdicts on the same audit trail as the session's own
// events.
func TestSession_EmitReachesAuditEmitter(t *testing.T) {
	rec := &recordingEmitter{}
	s := &Session{AuditEmitter: rec, SessionID: "sid"}
	s.Emit("llm.execute_gate", map[string]any{"allowed": false})
	if len(rec.events) != 1 || rec.events[0].Action != "llm.execute_gate" || rec.events[0].SessionID != "sid" {
		t.Errorf("events = %+v", rec.events)
	}
}

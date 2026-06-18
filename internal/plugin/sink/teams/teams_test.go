package teams_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/teams"
)

func TestTeams_PostsAdaptiveCard(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("1"))
	}))
	defer srv.Close()

	s, err := teams.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "teams",
		Config: map[string]any{"webhook_url": srv.URL, "min_severity": "info"},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	ev := output.NewEvent(output.SeverityError, "wal", "slot_missing")
	ev.Subject = output.Subject{Deployment: "db1"}
	ev.Suggestion = &output.Suggestion{Human: "run pg_hardstorage wal repair"}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if captured["type"] != "message" {
		t.Errorf("envelope type = %v, want message", captured["type"])
	}
	atts, _ := captured["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("expected one attachment: %#v", atts)
	}
	att := atts[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("contentType = %v", att["contentType"])
	}
	card := att["content"].(map[string]any)
	if card["version"] != "1.5" {
		t.Errorf("card version = %v", card["version"])
	}
	body := card["body"].([]any)
	header := body[0].(map[string]any)
	if header["color"] != "attention" {
		t.Errorf("error severity should map to attention color: %v", header["color"])
	}
	// FactSet should contain the Subject
	factSet := body[1].(map[string]any)
	facts := factSet["facts"].([]any)
	depFound := false
	for _, f := range facts {
		fm := f.(map[string]any)
		if fm["title"] == "Deployment" && fm["value"] == "db1" {
			depFound = true
		}
	}
	if !depFound {
		t.Error("Deployment fact missing")
	}
	// Suggestion line
	rendered, _ := json.Marshal(body)
	if !strings.Contains(string(rendered), "wal repair") {
		t.Errorf("suggestion missing from body: %s", rendered)
	}
}

func TestTeams_RequiresWebhookURL(t *testing.T) {
	_, err := teams.NewFromSpec(output.SinkSpec{Name: "x", Plugin: "teams", Config: map[string]any{}})
	if err == nil {
		t.Fatal("expected error without webhook_url")
	}
}

func TestTeams_RespectsMinSeverity(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, _ := teams.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "teams",
		Config: map[string]any{"webhook_url": srv.URL, "min_severity": "error"},
	})
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Errorf("warning under error floor should drop: %v", err)
	}
	if called {
		t.Error("warning event should not have hit the server (floor=error)")
	}
}

package discord_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/discord"
)

func TestDiscord_PostsEmbed(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s, err := discord.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "discord",
		Config: map[string]any{
			"webhook_url":  srv.URL,
			"username":     "pg-bot",
			"avatar_url":   "https://example.com/icon.png",
			"min_severity": "info",
		},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	ev := output.NewEvent(output.SeverityError, "wal", "slot_missing")
	ev.Subject = output.Subject{Deployment: "db1", Tenant: "default"}
	ev.Suggestion = &output.Suggestion{Human: "run pg_hardstorage wal repair"}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if captured["username"] != "pg-bot" {
		t.Errorf("username = %v", captured["username"])
	}
	if captured["avatar_url"] != "https://example.com/icon.png" {
		t.Errorf("avatar_url = %v", captured["avatar_url"])
	}
	embeds := captured["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("expected 1 embed: %#v", embeds)
	}
	embed := embeds[0].(map[string]any)
	if int(embed["color"].(float64)) != 0xE74C3C {
		t.Errorf("error color = %v, want %d", embed["color"], 0xE74C3C)
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "wal repair") {
		t.Errorf("description missing suggestion: %v", desc)
	}
	fields := embed["fields"].([]any)
	deploymentFound := false
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["name"] == "Deployment" && fm["value"] == "db1" {
			deploymentFound = true
		}
	}
	if !deploymentFound {
		t.Error("Deployment field missing")
	}
}

func TestDiscord_RequiresWebhookURL(t *testing.T) {
	_, err := discord.NewFromSpec(output.SinkSpec{Name: "x", Plugin: "discord", Config: map[string]any{}})
	if err == nil {
		t.Fatal("expected error without webhook_url")
	}
}

func TestDiscord_RespectsMinSeverity(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s, _ := discord.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "discord",
		Config: map[string]any{"webhook_url": srv.URL, "min_severity": "error"},
	})
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Errorf("warning under error floor should drop: %v", err)
	}
	if called {
		t.Error("warning event should not have hit the server (floor=error)")
	}
}

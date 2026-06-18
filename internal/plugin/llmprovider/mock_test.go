package llmprovider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

func TestMockProvider_Echoes(t *testing.T) {
	p := &llmprovider.MockProvider{}
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var text strings.Builder
	for c, err := range p.Chat(context.Background(), []llmprovider.Message{
		{Role: "user", Content: "hello world"},
	}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(c.Text)
	}
	got := text.String()
	if !strings.Contains(got, "hello world") {
		t.Errorf("expected mock to echo; got %q", got)
	}
}

func TestMockProvider_Script(t *testing.T) {
	p := &llmprovider.MockProvider{}
	p.Script("restore", "use --preview first")
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for c, err := range p.Chat(context.Background(), []llmprovider.Message{
		{Role: "user", Content: "how do I restore?"},
	}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(c.Text)
	}
	if got := text.String(); got != "use --preview first" {
		t.Errorf("scripted reply not applied; got %q", got)
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	if _, err := llmprovider.DefaultRegistry.Get("not-a-real-provider"); err == nil {
		t.Error("Get of unknown provider should fail")
	}
}

func TestRegistry_HasMockAndOpenAI(t *testing.T) {
	for _, want := range []string{"mock", "openai"} {
		if _, err := llmprovider.DefaultRegistry.Get(want); err != nil {
			t.Errorf("expected %q to be registered; got %v", want, err)
		}
	}
}

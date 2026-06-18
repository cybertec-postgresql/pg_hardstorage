package skills_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
)

func TestLocalized_PicksExactMatch(t *testing.T) {
	s := &skills.Skill{
		Name:           "ask",
		DisplayName:    "Ask",
		PromptTemplate: "english body",
		Locales: map[string]skills.LocaleOverride{
			"de": {DisplayName: "Frag mich", PromptTemplate: "deutsche fassung"},
		},
	}
	got := s.Localized("de")
	if got.DisplayName != "Frag mich" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.PromptTemplate != "deutsche fassung" {
		t.Errorf("PromptTemplate = %q", got.PromptTemplate)
	}
}

func TestLocalized_LanguagePrefix(t *testing.T) {
	s := &skills.Skill{
		PromptTemplate: "english",
		Locales: map[string]skills.LocaleOverride{
			"fr": {PromptTemplate: "français"},
		},
	}
	got := s.Localized("fr-CA")
	if got.PromptTemplate != "français" {
		t.Errorf("expected fr fallback for fr-CA; got %q", got.PromptTemplate)
	}
}

func TestLocalized_FallbackToDefault(t *testing.T) {
	s := &skills.Skill{
		PromptTemplate: "english",
		Locales: map[string]skills.LocaleOverride{
			"de": {PromptTemplate: "deutsch"},
		},
	}
	got := s.Localized("klingon")
	if got.PromptTemplate != "english" {
		t.Errorf("expected default fallback; got %q", got.PromptTemplate)
	}
}

func TestLocalized_EmptyLocaleReturnsOriginal(t *testing.T) {
	s := &skills.Skill{
		Name:           "ask",
		PromptTemplate: "english",
		Locales: map[string]skills.LocaleOverride{
			"de": {PromptTemplate: "deutsch"},
		},
	}
	got := s.Localized("")
	if got != s {
		t.Errorf("empty locale should return same pointer")
	}
}

func TestLocalized_PartialOverride(t *testing.T) {
	// Locale entry sets only DisplayName; PromptTemplate keeps default.
	s := &skills.Skill{
		DisplayName:    "Ask",
		Description:    "english desc",
		PromptTemplate: "english body",
		Locales: map[string]skills.LocaleOverride{
			"ja": {DisplayName: "質問"},
		},
	}
	got := s.Localized("ja")
	if got.DisplayName != "質問" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.Description != "english desc" {
		t.Errorf("Description should keep default; got %q", got.Description)
	}
	if got.PromptTemplate != "english body" {
		t.Errorf("PromptTemplate should keep default; got %q", got.PromptTemplate)
	}
}

// TestBuiltins_HaveLocalizedAsk: the bundled ask skill ships
// DE / FR / JA locales as part of the v1.0 i18n contract.
func TestBuiltins_HaveLocalizedAsk(t *testing.T) {
	set, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	ask, err := set.Get("ask")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{"de", "fr", "ja"} {
		if _, ok := ask.Locales[loc]; !ok {
			t.Errorf("ask skill missing locale %q", loc)
			continue
		}
		got := ask.Localized(loc)
		if got.PromptTemplate == ask.PromptTemplate {
			t.Errorf("locale %q should override prompt_template", loc)
		}
		if !strings.Contains(strings.ToLower(got.DisplayName), nonEnglish(loc)) {
			t.Logf("DisplayName for %q = %q (not asserting strict match)", loc, got.DisplayName)
		}
	}
}

func nonEnglish(loc string) string {
	switch loc {
	case "de":
		return "frag" // "Frag mich"
	case "fr":
		return "demand" // "Demande"
	case "ja":
		return "質問"
	}
	return ""
}

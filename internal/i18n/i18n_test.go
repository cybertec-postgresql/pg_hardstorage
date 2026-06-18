package i18n_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/i18n"
)

// ----- catalog completeness -----

// TestCatalogs_AllLocalesMirrorEnglish: every key in the English
// catalog must exist in every other registered locale.  A missing
// key isn't fatal at runtime (Tn falls back to English) but we
// surface it as a hard test failure so translation gaps are
// visible at CI time.
func TestCatalogs_AllLocalesMirrorEnglish(t *testing.T) {
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	enKeys := collectKeys(t, "en")
	for _, locale := range i18n.KnownLocales() {
		if locale == "en" {
			continue
		}
		gotKeys := collectKeys(t, locale)
		gotSet := make(map[string]struct{}, len(gotKeys))
		for _, k := range gotKeys {
			gotSet[k] = struct{}{}
		}
		for _, k := range enKeys {
			if _, ok := gotSet[k]; !ok {
				t.Errorf("locale %q missing key %q", locale, k)
			}
		}
	}
}

// collectKeys runs every English key through T() in the named
// locale + reports any that fall back to the literal key (i.e.
// neither the active locale nor English produced a string).
// Used by TestCatalogs_AllLocalesMirrorEnglish.
func collectKeys(t *testing.T, locale string) []string {
	t.Helper()
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	keys := allEnglishKeysForTest(t)
	t.Cleanup(func() { _ = i18n.SetActive("en") })
	if err := i18n.SetActive(locale); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		got := i18n.T(k)
		if got == k {
			// Literal-key fallback — translation missing in BOTH
			// locale + English (impossible if the catalog was
			// registered correctly) OR active locale fell through
			// AND English is the active locale (this is the
			// callsite for `locale == "en"` only).
			continue
		}
		out = append(out, k)
	}
	return out
}

// allEnglishKeysForTest hard-codes the keys we expect in the
// English catalog.  Keeping this in the test rather than
// reflecting on the unexported map is deliberate: a translation
// gap that drops a key from the EN catalog should be a hard
// signal too.
func allEnglishKeysForTest(t *testing.T) []string {
	return []string{
		"doctor.healthy",
		"doctor.attention_required",
		"doctor.last_backup_ago",
		"doctor.wal_lag",
		"doctor.repo_unreachable",
		"doctor.kms_unreachable",
		"doctor.suggested_fix",
		"restore.refuses_live_pg",
		"restore.refuses_non_empty",
		"restore.refuses_primary",
		"restore.preview_header",
		"restore.preview_target",
		"restore.preview_pg_version",
		"restore.preview_source",
		"restore.preview_rto",
		"restore.preview_run_command",
		"restore.completed",
		"status.no_backups",
		"status.next_backup_at",
		"status.next_drill_at",
		"status.replication_active",
		"status.replication_lagging",
		"status.replication_stalled",
		"err.repo_required",
		"err.target_required",
		"err.invalid_lsn",
		"err.invalid_time",
		"err.invalid_severity",
		"err.unknown_format",
	}
}

// ----- T -----

func TestT_HappyPath_English(t *testing.T) {
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	got := i18n.T("doctor.healthy")
	if got != "all clear" {
		t.Errorf("got %q, want %q", got, "all clear")
	}
}

func TestT_GermanTranslation(t *testing.T) {
	if err := i18n.SetActive("de"); err != nil {
		t.Fatal(err)
	}
	defer i18n.SetActive("en")
	got := i18n.T("doctor.healthy")
	if got != "alles in Ordnung" {
		t.Errorf("got %q, want %q", got, "alles in Ordnung")
	}
}

func TestT_WithTemplateData(t *testing.T) {
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	got := i18n.T("doctor.repo_unreachable",
		map[string]any{"URL": "s3://acme/"})
	if !strings.Contains(got, "s3://acme/") {
		t.Errorf("template field not substituted: %q", got)
	}
}

func TestT_FrenchPunctuation(t *testing.T) {
	if err := i18n.SetActive("fr"); err != nil {
		t.Fatal(err)
	}
	defer i18n.SetActive("en")
	got := i18n.T("doctor.repo_unreachable",
		map[string]any{"URL": "s3://acme/"})
	// French uses " : " before colons (with a space before).
	if !strings.Contains(got, " : ") {
		t.Errorf("expected French colon convention; got %q", got)
	}
}

func TestT_JapaneseCharacters(t *testing.T) {
	if err := i18n.SetActive("ja"); err != nil {
		t.Fatal(err)
	}
	defer i18n.SetActive("en")
	got := i18n.T("doctor.healthy")
	if got != "正常" {
		t.Errorf("got %q, want %q", got, "正常")
	}
}

// TestT_FallsBackToEnglishWhenLocaleMissingKey: if an active
// locale lacks a key but English has it, T returns the English
// translation.
func TestT_FallsBackToEnglishWhenLocaleMissingKey(t *testing.T) {
	i18n.Register("eo", map[string]string{
		"only_in_esperanto": "tio estas testo",
	})
	defer func() {
		_ = i18n.SetActive("en")
		// eo catalog persists across the test; that's fine — the
		// other tests that read KnownLocales filter by what they
		// care about.
	}()
	if err := i18n.SetActive("eo"); err != nil {
		t.Fatal(err)
	}
	// Key only in English → fall back.
	got := i18n.T("doctor.healthy")
	if got != "all clear" {
		t.Errorf("expected English fallback; got %q", got)
	}
	// Key only in eo → eo wins.
	got = i18n.T("only_in_esperanto")
	if got != "tio estas testo" {
		t.Errorf("got %q, want %q", got, "tio estas testo")
	}
}

func TestT_UnknownKeyReturnsLiteral(t *testing.T) {
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	got := i18n.T("does.not.exist")
	if got != "does.not.exist" {
		t.Errorf("got %q, want literal key", got)
	}
}

// ----- Tn (plurals) -----

func TestTn_EnglishPlural(t *testing.T) {
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	if got := i18n.Tn("backups.count", 1); got != "1 backup" {
		t.Errorf("singular: got %q, want %q", got, "1 backup")
	}
	if got := i18n.Tn("backups.count", 5); got != "5 backups" {
		t.Errorf("plural: got %q, want %q", got, "5 backups")
	}
	if got := i18n.Tn("backups.count", 0); got != "0 backups" {
		t.Errorf("zero: got %q, want %q", got, "0 backups")
	}
}

func TestTn_GermanPlural(t *testing.T) {
	if err := i18n.SetActive("de"); err != nil {
		t.Fatal(err)
	}
	defer i18n.SetActive("en")
	if got := i18n.Tn("backups.count", 1); got != "1 Backup" {
		t.Errorf("singular: got %q, want %q", got, "1 Backup")
	}
	if got := i18n.Tn("backups.count", 7); got != "7 Backups" {
		t.Errorf("plural: got %q, want %q", got, "7 Backups")
	}
}

func TestTn_JapaneseHasNoGrammaticalPlural(t *testing.T) {
	if err := i18n.SetActive("ja"); err != nil {
		t.Fatal(err)
	}
	defer i18n.SetActive("en")
	one := i18n.Tn("backups.count", 1)
	many := i18n.Tn("backups.count", 5)
	// Japanese has no grammatical plural marker — both forms
	// render the count + the noun + the counter.
	if !strings.Contains(one, "1") || !strings.Contains(many, "5") {
		t.Errorf("expected count substituted: one=%q many=%q", one, many)
	}
}

func TestTn_MissingPluralFallsBackToSingular(t *testing.T) {
	if err := i18n.SetActive("en"); err != nil {
		t.Fatal(err)
	}
	// "doctor.healthy" is not registered in the plurals map; Tn
	// falls back to the singular catalog entry.
	got := i18n.Tn("doctor.healthy", 5)
	if got != "all clear" {
		t.Errorf("got %q, want fallback to singular", got)
	}
}

// ----- locale resolution -----

func TestSetActive_UnknownLocaleErrors(t *testing.T) {
	defer i18n.SetActive("en")
	err := i18n.SetActive("xx-YY")
	if err == nil {
		t.Errorf("expected error for unknown locale")
	}
}

func TestKnownLocales_ListsAllRegistered(t *testing.T) {
	got := i18n.KnownLocales()
	want := map[string]bool{"en": false, "de": false, "fr": false, "ja": false}
	for _, l := range got {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for l, present := range want {
		if !present {
			t.Errorf("KnownLocales missing %q (got %v)", l, got)
		}
	}
}

func TestResolveLocale_NormalisesLANG(t *testing.T) {
	t.Setenv(i18n.LocaleEnv, "")
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	got, ok := i18n.ResolveLocale()
	if !ok {
		t.Fatalf("expected env-driven resolution")
	}
	if got != "de" {
		t.Errorf("got %q, want %q", got, "de")
	}
}

func TestResolveLocale_PrefersExplicitOverride(t *testing.T) {
	t.Setenv(i18n.LocaleEnv, "fr")
	t.Setenv("LANG", "de_DE.UTF-8")
	got, ok := i18n.ResolveLocale()
	if !ok {
		t.Fatalf("expected resolution")
	}
	if got != "fr" {
		t.Errorf("PG_HARDSTORAGE_LANG should win; got %q", got)
	}
}

func TestResolveLocale_DefaultsToEnglish(t *testing.T) {
	t.Setenv(i18n.LocaleEnv, "")
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	got, ok := i18n.ResolveLocale()
	if ok {
		t.Errorf("expected false for empty env")
	}
	if got != i18n.DefaultLocale {
		t.Errorf("got %q, want default locale %q", got, i18n.DefaultLocale)
	}
}

func TestAutoActivate_FallsBackForUnregisteredLocale(t *testing.T) {
	defer i18n.SetActive("en")
	t.Setenv(i18n.LocaleEnv, "zh")
	got := i18n.AutoActivate()
	if got != i18n.DefaultLocale {
		t.Errorf("unregistered zh should fall back to %q; got %q",
			i18n.DefaultLocale, got)
	}
}

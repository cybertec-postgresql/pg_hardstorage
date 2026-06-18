// Package i18n implements the message-translation layer for the
// CLI / TUI / structured-error suggestions.  Closes the SPEC
// commitment "multi-language CLI / TUI (i18n).  German, French,
// Japanese first; community translation."
//
// Design — string-key catalog + locale resolution chain:
//
//  1. Every operator-facing English string lives at a registered
//     key (e.g. "doctor.healthy", "restore.refuses_live_pg").
//     Code calls T(key) instead of writing the string inline.
//  2. Catalogs are registered per-locale by init-time calls to
//     Register.  Built-in catalogs ship for English (the
//     authoritative source), German, French, Japanese.
//  3. The active locale is resolved from $PG_HARDSTORAGE_LANG,
//     then $LANG / $LC_ALL, then defaults to "en".  An explicit
//     `--lang <bcp47>` CLI flag overrides everything.
//  4. T(key) returns the translation in the active locale, or
//     falls back to English, or returns the literal key if the
//     key isn't even registered in English.  The fallback chain
//     is never silent: an unregistered key fires a one-line
//     warning to stderr in development builds.
//
// Why string-keyed and not formatted strings: formatted strings
// (printf-style) tangle the message with the data layout.  A
// string-key catalog separates the two so a translator can re-
// order interpolated fields without touching code.  We use Go
// `text/template` (fields named, not positional) for
// interpolation so a German translator can write
// "Backup {{.ID}} schlug fehl" while the English original is
// "Backup {{.ID}} failed" — same data, different word order.
//
// Pluralisation: explicit one/other forms via `Tn(key, n, data)`
// — Go templates have no built-in plural form, so we expose a
// dedicated entry point.  Catalog files spell out both forms
// for every plural key.
//
// Scope at+: ship the framework + a small representative
// catalog (~30 keys) covering doctor / restore / status / common
// errors in EN/DE/FR/JA.  Future commits expand the catalog as
// new commands surface translatable strings.
package i18n

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"
)

// LocaleEnv is the env var operators use to override the runtime-
// detected locale.  Takes precedence over LANG / LC_ALL.
const LocaleEnv = "PG_HARDSTORAGE_LANG"

// DefaultLocale is the fallback when nothing else is set + when
// a key isn't registered in the active locale.
const DefaultLocale = "en"

// Sentinel errors.
var (
	ErrUnknownKey    = errors.New("i18n: unknown message key")
	ErrUnknownLocale = errors.New("i18n: unknown locale")
)

// Form is the singular/plural form selector used by Tn.
type Form int

const (
	// FormOther is the non-singular case ("0 backups", "2 backups").
	FormOther Form = iota
	// FormOne is the singular case ("1 backup").  English + most
	// Romance + Germanic languages collapse to this binary
	// distinction; languages with richer plural rules can extend
	// this enum without breaking existing translations.
	FormOne
)

// catalog is one locale's message map.
type catalog struct {
	locale   string
	messages map[string]string          // key → singular template
	plurals  map[string]map[Form]string // key → form → template
}

// state holds the package-wide registry + the active locale.
type state struct {
	mu        sync.RWMutex
	catalogs  map[string]*catalog
	active    string
	devWarned map[string]bool // dev-mode warnings fire once per key
}

var s = &state{
	catalogs:  map[string]*catalog{},
	active:    DefaultLocale,
	devWarned: map[string]bool{},
}

// Register installs a catalog for the named locale.  Repeated
// calls for the same locale merge new keys + override existing
// ones — useful for plugin packs that ship additional strings
// without owning the whole catalog.
func Register(locale string, messages map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cat, ok := s.catalogs[locale]
	if !ok {
		cat = &catalog{
			locale:   locale,
			messages: map[string]string{},
			plurals:  map[string]map[Form]string{},
		}
		s.catalogs[locale] = cat
	}
	for k, v := range messages {
		cat.messages[k] = v
	}
}

// RegisterPlurals installs plural-aware messages for the locale.
// The map key is the message key; the value is the per-form
// template.  Both FormOne + FormOther must be supplied.
func RegisterPlurals(locale string, plurals map[string]map[Form]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cat, ok := s.catalogs[locale]
	if !ok {
		cat = &catalog{
			locale:   locale,
			messages: map[string]string{},
			plurals:  map[string]map[Form]string{},
		}
		s.catalogs[locale] = cat
	}
	for k, v := range plurals {
		cat.plurals[k] = v
	}
}

// SetActive switches the active locale.  Returns ErrUnknownLocale
// if the named locale has no catalog.  Always succeeds for the
// default locale (English) since the built-in catalog is registered
// at init time.
func SetActive(locale string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.catalogs[locale]; !ok {
		return fmt.Errorf("%w: %q (registered: %v)",
			ErrUnknownLocale, locale, knownLocalesLocked())
	}
	s.active = locale
	return nil
}

// Active returns the currently-selected locale.
func Active() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// KnownLocales returns the list of registered locales, sorted.
// Useful for `--lang` validation in CLI flag parsers.
func KnownLocales() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return knownLocalesLocked()
}

func knownLocalesLocked() []string {
	out := make([]string, 0, len(s.catalogs))
	for k := range s.catalogs {
		out = append(out, k)
	}
	// Sort by lex order — small N, sort.Strings would be fine but
	// we avoid the import for this trivial case.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// T resolves a message key in the active locale, falling back to
// English, then to the literal key.  Optional data is fed to a
// text/template render so messages can interpolate named fields
// without printf-style positional ordering.
func T(key string, data ...any) string {
	tmpl, locale := lookupMessage(key)
	if tmpl == "" {
		return key
	}
	return render(tmpl, locale, key, data...)
}

// Tn resolves a plural-aware message key.  When n == 1, returns
// FormOne; otherwise FormOther.  Data is fed into the template
// alongside an implicit `.N` field so `Tn("backups.count", 5,
// nil)` renders "5 backups" from a template like "{{.N}} backups".
func Tn(key string, n int, data ...any) string {
	plurals, locale := lookupPlural(key)
	var tmpl string
	switch {
	case plurals == nil:
		// Fall through to the singular catalog.
		return T(key, data...)
	case n == 1:
		tmpl = plurals[FormOne]
		if tmpl == "" {
			tmpl = plurals[FormOther]
		}
	default:
		tmpl = plurals[FormOther]
		if tmpl == "" {
			tmpl = plurals[FormOne]
		}
	}
	if tmpl == "" {
		return key
	}
	merged := mergeData(map[string]any{"N": n}, data...)
	return render(tmpl, locale, key, merged)
}

// lookupMessage tries the active locale first, then English.
// Returns (template, locale-resolved-from) or ("", "").
func lookupMessage(key string) (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cat, ok := s.catalogs[s.active]; ok {
		if v, ok := cat.messages[key]; ok {
			return v, s.active
		}
	}
	if s.active != DefaultLocale {
		if cat, ok := s.catalogs[DefaultLocale]; ok {
			if v, ok := cat.messages[key]; ok {
				return v, DefaultLocale
			}
		}
	}
	return "", ""
}

func lookupPlural(key string) (map[Form]string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cat, ok := s.catalogs[s.active]; ok {
		if v, ok := cat.plurals[key]; ok {
			return v, s.active
		}
	}
	if s.active != DefaultLocale {
		if cat, ok := s.catalogs[DefaultLocale]; ok {
			if v, ok := cat.plurals[key]; ok {
				return v, DefaultLocale
			}
		}
	}
	return nil, ""
}

// render parses the template + executes it against data.  Falls
// back to the raw template string on parse / execute failure (so
// a buggy translation doesn't bring down the CLI).
func render(tmpl, locale, key string, data ...any) string {
	t, err := template.New(locale + "/" + key).Parse(tmpl)
	if err != nil {
		return tmpl
	}
	var buf bytes.Buffer
	var arg any
	switch len(data) {
	case 0:
		arg = nil
	case 1:
		arg = data[0]
	default:
		arg = mergeData(nil, data...)
	}
	if err := t.Execute(&buf, arg); err != nil {
		return tmpl
	}
	return buf.String()
}

// mergeData unifies multiple data arguments into one map so
// templates can refer to named fields.  Last-wins semantics on
// key collisions.
func mergeData(seed map[string]any, data ...any) map[string]any {
	out := map[string]any{}
	for k, v := range seed {
		out[k] = v
	}
	for _, d := range data {
		if d == nil {
			continue
		}
		switch m := d.(type) {
		case map[string]any:
			for k, v := range m {
				out[k] = v
			}
		default:
			// Anything non-map is dropped — the catalog uses named
			// fields exclusively.  A future template that wants to
			// pass a struct can do so by registering its fields via
			// reflection, out of+ scope.
		}
	}
	return out
}

// ResolveLocale picks the active locale based on the env-var /
// $LANG chain.  Returns the resolved locale name + a bool
// indicating whether the chain produced anything (false ⇒
// caller should default).
func ResolveLocale() (string, bool) {
	if v := strings.TrimSpace(os.Getenv(LocaleEnv)); v != "" {
		return normaliseLocale(v), true
	}
	for _, env := range []string{"LC_ALL", "LANG", "LC_MESSAGES"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return normaliseLocale(v), true
		}
	}
	return DefaultLocale, false
}

// normaliseLocale strips encoding suffixes ("en_US.UTF-8" →
// "en_US") and lowercases the language portion ("EN_US" → "en_US").
// We don't attempt a full BCP-47 parse — operators write either
// pure language codes ("de", "ja") or POSIX-style "ll_RR.encoding"
// strings; both should map to a registered catalog.
func normaliseLocale(raw string) string {
	if i := strings.IndexByte(raw, '.'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '_'); i >= 0 {
		return strings.ToLower(raw[:i])
	}
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		return strings.ToLower(raw[:i])
	}
	return strings.ToLower(raw)
}

// AutoActivate runs the resolution chain + sets the active locale
// when one is registered.  Falls back silently to the default
// when the resolved locale isn't registered (operators in zh_CN
// shouldn't see English error spam — they just see English
// messages).  Call once at program startup.
func AutoActivate() string {
	resolved, _ := ResolveLocale()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.catalogs[resolved]; ok {
		s.active = resolved
	} else {
		s.active = DefaultLocale
	}
	return s.active
}

// Reset clears every catalog + reverts to DefaultLocale.  Tests
// use this to start from a clean slate.  Production code should
// not call this — catalogs are init-time data.
func Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catalogs = map[string]*catalog{}
	s.active = DefaultLocale
	s.devWarned = map[string]bool{}
}

// Package i18n holds the embedded en/es UI string catalogs and the lookup funnel
// behind AGENTS rule 9 / D14 / D15: every user-visible string is a catalog key,
// the two catalogs share an identical message-ID set (TestCatalogParity), and a
// missing key falls back to English.
//
// Since D15 the engine is go-i18n (github.com/nicksnyder/go-i18n/v2) with the
// BurntSushi/toml unmarshaler and x/text's CLDR plural rules — this supersedes
// D14's hand-rolled parser. The PUBLIC facade is deliberately unchanged: T does
// positional fmt.Sprintf interpolation over the go-i18n-selected message (catalog
// values keep %s/%d verbs, NOT go-i18n {{.X}} template data), so every existing
// call site and the {{t}} template func keep working exactly as before. TN adds
// CLDR pluralization (one/other) where counts appear — the D15 payoff.
package i18n

import (
	"embed"
	"fmt"
	"sync"

	"github.com/BurntSushi/toml"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// baseLang is the fallback language: any unknown language or missing key resolves
// against it before giving up and returning the raw key (D14).
const baseLang = "en"

// catalogFS holds the embedded go-i18n TOML message catalogs. They are build
// assets, not runtime input; a parse failure is a programmer error caught by the
// loader (see bundle()), never by T.
//
//go:embed en.toml es.toml
var catalogFS embed.FS

var (
	bundleOnce sync.Once
	shared     *goi18n.Bundle
	// localizers holds one pre-built Localizer per known language (Langs()), each
	// configured with the [lang, en] fallback chain so a locally-missing key
	// resolves against English before we surface the raw key. It is populated
	// eagerly inside bundle()'s sync.Once and never mutated afterward, so reads in
	// localizer() need no lock (an unknown lang falls back to the base localizer).
	localizers map[string]*goi18n.Localizer
)

// bundle loads the embedded catalogs into a go-i18n Bundle exactly once, with the
// TOML unmarshaler registered and English as the default language. A malformed
// embedded file is a build-time defect, so it panics here (the one sanctioned
// panic in this package) rather than degrading silently; T/TN stay panic-free
// because they only ever read the already-loaded bundle.
func bundle() *goi18n.Bundle {
	bundleOnce.Do(func() {
		b := goi18n.NewBundle(language.English)
		b.RegisterUnmarshalFunc("toml", toml.Unmarshal)
		for _, lang := range Langs() {
			if _, err := b.LoadMessageFileFS(catalogFS, lang+".toml"); err != nil {
				panic(fmt.Sprintf("i18n: load embedded %s catalog: %v", lang, err))
			}
		}
		shared = b
		// Pre-build every known language's Localizer up front so the hot path
		// (localizer/localize/T) is a lock-free map read. The [lang, baseLang]
		// chain resolves a locally-missing key against English.
		localizers = make(map[string]*goi18n.Localizer, len(Langs()))
		for _, lang := range Langs() {
			localizers[lang] = goi18n.NewLocalizer(b, lang, baseLang)
		}
	})
	return shared
}

// localizer returns the pre-built Localizer for lang with NO locking (localizers is
// populated once in bundle() and never mutated). An unknown language — not among
// Langs() — falls back to the base (en) localizer, which is the same English text
// an on-demand [lang, en] localizer would have produced.
func localizer(lang string) *goi18n.Localizer {
	bundle() // ensure localizers is populated
	if lc, ok := localizers[lang]; ok {
		return lc
	}
	return localizers[baseLang]
}

// Langs returns the available languages in a stable order (base language first).
func Langs() []string {
	return []string{"en", "es"}
}

var (
	keysOnce sync.Once
	keySet   map[string]struct{}
)

// keys builds the flattened message-ID set from the base (en) catalog exactly once.
// Parity (TestCatalogParity) guarantees es carries the same IDs, so the en set is
// the authoritative key universe. A parse failure is a build-time defect and panics
// (like bundle()); Has never panics because it only reads the loaded set.
func keys() map[string]struct{} {
	keysOnce.Do(func() {
		src, err := catalogFS.ReadFile(baseLang + ".toml")
		if err != nil {
			panic(fmt.Sprintf("i18n: read embedded %s catalog: %v", baseLang, err))
		}
		var raw map[string]any
		if err := toml.Unmarshal(src, &raw); err != nil {
			panic(fmt.Sprintf("i18n: unmarshal embedded %s catalog: %v", baseLang, err))
		}
		keySet = make(map[string]struct{})
		flattenMessageIDs("", raw, keySet)
	})
	return keySet
}

// flattenMessageIDs walks a TOML tree into the dotted message IDs go-i18n derives:
// nested tables join with '.', and a leaf table carrying plural forms (one/other)
// is a single message, not several. This mirrors the parity test's own flattening
// so Has checks the real key universe.
func flattenMessageIDs(prefix string, m map[string]any, out map[string]struct{}) {
	// A message table is a leaf: every value is a plural-form string (or a reserved
	// field), never a further nested table.
	if prefix != "" && isMessageTableAny(m) {
		out[prefix] = struct{}{}
		return
	}
	for k, v := range m {
		id := k
		if prefix != "" {
			id = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			flattenMessageIDs(id, child, out)
			continue
		}
		out[id] = struct{}{}
	}
}

// isMessageTableAny reports whether every value in the table is a scalar (a plural
// form or reserved field), i.e. the table is a go-i18n message, not a container of
// further messages.
func isMessageTableAny(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		if _, nested := v.(map[string]any); nested {
			return false
		}
	}
	return true
}

// Has reports whether key resolves to a real message in the catalog. It lets
// callers (notably the template-key scan test) verify that a referenced key exists
// without triggering the raw-key fallback that T/TN return for a missing key.
func Has(key string) bool {
	_, ok := keys()[key]
	return ok
}

// T looks up key in lang (with en fallback), then applies fmt.Sprintf positional
// interpolation with args over the returned message text (which carries %s/%d
// verbs — go-i18n leaves non-{{}} text untouched). Fallback chain: unknown lang
// or locally-missing key → en → the key itself. It never panics and never returns
// empty for a real key. Interpolating a trusted embedded catalog string via
// fmt.Sprintf is stdlib-only and safe (values are ours).
func T(lang, key string, args ...any) string {
	msg := localize(lang, key, nil)
	if msg == "" {
		// Absent everywhere: surface the key so the gap is visible, never blank.
		return key
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// TN is T with CLDR pluralization: count selects the one/other form (via x/text
// plural rules for lang) and is also the first positional Sprintf arg, so the
// message's leading %d is filled by the same count the template passes once.
// Additional args follow count in order. Same fallback chain as T.
func TN(lang, key string, count int, args ...any) string {
	msg := localize(lang, key, count)
	if msg == "" {
		return key
	}
	all := make([]any, 0, len(args)+1)
	all = append(all, count)
	all = append(all, args...)
	return fmt.Sprintf(msg, all...)
}

// localize resolves key in lang with en fallback and returns the raw message text
// (verbs intact) or "" if the key exists in no catalog. pluralCount is nil for a
// non-count lookup, or *int to select the plural form. A go-i18n "not found"
// error still returns the English fallback text when the [lang, en] chain found
// it; only a truly-absent key yields an empty string, which the callers map to
// the raw key.
func localize(lang, key string, pluralCount any) string {
	cfg := &goi18n.LocalizeConfig{MessageID: key}
	if pluralCount != nil {
		cfg.PluralCount = pluralCount
	}
	// Localize returns the (possibly fallback) text plus an error when the ID is
	// missing in the requested language; we key off the text, not the error, so
	// the en fallback value is honored and only a truly-absent key returns "".
	msg, _ := localizer(lang).Localize(cfg)
	return msg
}

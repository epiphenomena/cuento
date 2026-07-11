// Package i18n holds the embedded en/es UI string catalogs and the lookup
// funnel behind AGENTS rule 9 / D14: every user-visible string is a catalog
// key, the two catalogs share an identical key set (TestCatalogParity), and a
// missing key falls back to English. There is deliberately NO i18n/CLDR
// dependency (D15) — the catalogs are a page of data and a tiny parser, not a
// framework. Registering the {{t}} template func is a later step (phase 10);
// this package only exposes T, Langs, and the parser.
package i18n

import (
	"fmt"
	"strings"
	"sync"

	_ "embed"
)

// baseLang is the fallback language: any unknown language or missing key
// resolves against it before giving up and returning the raw key (D14).
const baseLang = "en"

// The catalogs are embedded build assets, not runtime input; a parse failure is
// a programmer error caught by the loader (see catalogs()), never by T.
//
//go:embed en.toml
var enCatalog string

//go:embed es.toml
var esCatalog string

var (
	catalogsOnce sync.Once
	loaded       map[string]map[string]string
)

// catalogs parses the embedded catalogs exactly once into lang → key → value.
// A malformed embedded file is a build-time defect, so it panics here (the one
// sanctioned panic in this package) rather than degrading silently; T stays
// panic-free because it only ever reads the already-loaded maps.
func catalogs() map[string]map[string]string {
	catalogsOnce.Do(func() {
		loaded = make(map[string]map[string]string, len(Langs()))
		for lang, src := range map[string]string{
			"en": enCatalog,
			"es": esCatalog,
		} {
			m, err := parseCatalog(src)
			if err != nil {
				panic(fmt.Sprintf("i18n: parse embedded %s catalog: %v", lang, err))
			}
			loaded[lang] = m
		}
	})
	return loaded
}

// Langs returns the available languages in a stable order (base language first).
func Langs() []string {
	return []string{"en", "es"}
}

// T looks up key in lang's catalog, applies fmt.Sprintf interpolation with args
// (catalog values carry %s/%d verbs), and follows the fallback chain: unknown
// lang or missing key → the base (en) catalog → the key itself. It never panics
// and never returns empty for a real key. Interpolating a trusted embedded
// catalog string via fmt.Sprintf is stdlib-only and safe (values are ours).
func T(lang, key string, args ...any) string {
	return tr(catalogs(), lang, key, args...)
}

// tr is the fallback + interpolation core, parameterized over the catalog set so
// tests can drive the "valid lang, key present in en but missing locally" branch
// with a bespoke asymmetric map — a state the parity-clean real catalogs forbid.
func tr(cats map[string]map[string]string, lang, key string, args ...any) string {
	value, ok := lookup(cats, lang, key)
	if !ok {
		// Unknown lang or missing key: try the base language.
		value, ok = lookup(cats, baseLang, key)
	}
	if !ok {
		// Truly absent everywhere: surface the key so the gap is visible,
		// never a blank string or a panic.
		return key
	}
	if len(args) == 0 {
		return value
	}
	return fmt.Sprintf(value, args...)
}

// lookup returns the value for key in the given lang's catalog, if both exist.
func lookup(cats map[string]map[string]string, lang, key string) (string, bool) {
	m, ok := cats[lang]
	if !ok {
		return "", false
	}
	v, ok := m[key]
	return v, ok
}

// parseCatalog parses the flat catalog subset (NOT full TOML): each significant
// line is `key = "value"`; `#` lines and blank lines are ignored; values are
// double-quoted with \n, \" and \\ escapes. Anything else (no `=`, unquoted or
// unterminated value, unknown escape) is rejected with a line-numbered error so
// a bad embedded catalog fails loudly at load rather than mis-translating.
func parseCatalog(src string) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(src, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: missing '=' in %q", lineNo, raw)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key in %q", lineNo, raw)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", lineNo, key)
		}

		value, err := unquote(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		out[key] = value
	}
	return out, nil
}

// unquote decodes a double-quoted catalog value with the supported escapes
// (\n, \", \\). It is intentionally stricter and smaller than strconv.Unquote:
// only the escapes this format documents are accepted.
func unquote(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("value %q must be double-quoted", s)
	}
	body := s[1 : len(s)-1]

	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("value %q: dangling escape", s)
		}
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			return "", fmt.Errorf("value %q: unknown escape \\%c", s, body[i])
		}
	}
	return b.String(), nil
}

package i18n

import (
	"sort"
	"testing"
)

// TestCatalogParity is the invariant that keeps translations honest forever
// (AGENTS rule 9, D14): en and es must expose the EXACT same key set. It checks
// BOTH directions and names the offending keys so a failure points straight at
// the missing translation.
func TestCatalogParity(t *testing.T) {
	cats := catalogs()
	en, ok := cats["en"]
	if !ok {
		t.Fatal("en catalog missing")
	}
	es, ok := cats["es"]
	if !ok {
		t.Fatal("es catalog missing")
	}

	if missing := keysNotIn(en, es); len(missing) > 0 {
		t.Errorf("keys in en but missing from es: %v", missing)
	}
	if missing := keysNotIn(es, en); len(missing) > 0 {
		t.Errorf("keys in es but missing from en: %v", missing)
	}
}

// keysNotIn returns the sorted keys present in a but absent from b.
func keysNotIn(a, b map[string]string) []string {
	var missing []string
	for k := range a {
		if _, ok := b[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

// TestFallbackToEnglish covers the fallback chain: unknown lang → en, and a key
// absent from the requested catalog → en → the key itself. The "valid lang,
// key present in en but missing locally" branch cannot occur with parity-clean
// real catalogs, so it is exercised through tr() with a bespoke asymmetric map.
func TestFallbackToEnglish(t *testing.T) {
	// (a) Unknown language falls back to the English value.
	if got := T("fr", "common.save"); got != "Save" {
		t.Errorf("T(fr, common.save) = %q, want English %q", got, "Save")
	}

	// (a') A genuinely-absent key returns the key itself (never panics/empty).
	if got := T("en", "no.such.key"); got != "no.such.key" {
		t.Errorf("T(en, no.such.key) = %q, want the key itself", got)
	}

	// (b) Valid lang whose catalog is missing a key that en has: fall back to en.
	asym := map[string]map[string]string{
		"en": {"only.in.en": "English"},
		"es": {}, // deliberately missing the key
	}
	if got := tr(asym, "es", "only.in.en"); got != "English" {
		t.Errorf("tr(es, only.in.en) = %q, want English fallback %q", got, "English")
	}
	// And if it is truly absent everywhere, the key is returned.
	if got := tr(asym, "es", "ghost"); got != "ghost" {
		t.Errorf("tr(es, ghost) = %q, want the key itself", got)
	}
}

// TestInterpolation checks positional %-verb interpolation via the catalog value.
func TestInterpolation(t *testing.T) {
	if got := T("en", "greeting", "world"); got != "Hello, world" {
		t.Errorf("T(en, greeting, world) = %q, want %q", got, "Hello, world")
	}
	if got := T("es", "greeting", "mundo"); got != "Hola, mundo" {
		t.Errorf("T(es, greeting, mundo) = %q, want %q", got, "Hola, mundo")
	}
}

// TestLangs pins the available languages and their stable order.
func TestLangs(t *testing.T) {
	got := Langs()
	want := []string{"en", "es"}
	if len(got) != len(want) {
		t.Fatalf("Langs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Langs() = %v, want %v", got, want)
		}
	}
}

// TestParserSubset exercises the hand-rolled parser directly: comments, blanks,
// escapes, and a malformed line producing a clear error.
func TestParserSubset(t *testing.T) {
	src := "# comment\n\napp.name = \"cuento\"\nesc = \"a\\nb\\\"c\\\\d\"\n"
	m, err := parseCatalog(src)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if m["app.name"] != "cuento" {
		t.Errorf("app.name = %q", m["app.name"])
	}
	if m["esc"] != "a\nb\"c\\d" {
		t.Errorf("esc = %q, want unescaped", m["esc"])
	}

	if _, err := parseCatalog("bad line without equals\n"); err == nil {
		t.Error("expected error on malformed line, got nil")
	}
}

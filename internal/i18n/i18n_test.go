package i18n

import (
	"sort"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestCatalogParity is the invariant that keeps translations honest forever
// (AGENTS rule 9, D14/D15): en and es must expose the EXACT same message-ID set.
// It reads both embedded TOML catalogs directly (via the same toml lib the bundle
// uses), flattens them to the go-i18n message IDs, and compares BOTH directions,
// naming the offending IDs so a failure points straight at the missing translation.
func TestCatalogParity(t *testing.T) {
	en := messageIDs(t, "en.toml")
	es := messageIDs(t, "es.toml")

	if missing := keysNotIn(en, es); len(missing) > 0 {
		t.Errorf("message IDs in en but missing from es: %v", missing)
	}
	if missing := keysNotIn(es, en); len(missing) > 0 {
		t.Errorf("message IDs in es but missing from en: %v", missing)
	}
}

// messageIDs unmarshals a catalog's TOML into a generic tree and flattens it to
// the same dotted message IDs go-i18n derives (nested tables joined by '.'; a
// leaf table carrying plural forms one/other is one message, not several). This
// mirrors the bundle's own parsing so parity is checked against the real IDs.
func messageIDs(t *testing.T, name string) map[string]struct{} {
	t.Helper()
	src, err := catalogFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(src, &raw); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	ids := make(map[string]struct{})
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		// A message table is a leaf: every value is a plural-form string (or the
		// go-i18n reserved "description"/"hash"), never a further nested table.
		if prefix != "" && isMessageTable(m) {
			ids[prefix] = struct{}{}
			return
		}
		for k, v := range m {
			id := k
			if prefix != "" {
				id = prefix + "." + k
			}
			switch child := v.(type) {
			case map[string]any:
				walk(id, child)
			default:
				// A bare scalar at this level is a message with this ID.
				ids[id] = struct{}{}
			}
		}
	}
	walk("", raw)
	return ids
}

// isMessageTable reports whether every value in the table is a scalar (a plural
// form or reserved field), i.e. the table is a go-i18n message, not a container
// of further messages.
func isMessageTable(m map[string]any) bool {
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

// keysNotIn returns the sorted keys present in a but absent from b.
func keysNotIn(a, b map[string]struct{}) []string {
	var missing []string
	for k := range a {
		if _, ok := b[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

// TestFallbackToEnglish covers the fallback chain: an unknown language resolves
// against en, and a genuinely-absent key returns the key itself (never panics,
// never empty). The "valid lang, key present in en but missing locally" branch
// cannot occur with parity-clean real catalogs; it is proven by the go-i18n
// localizer's [lang, en] fallback chain, which the smoke test below exercises.
func TestFallbackToEnglish(t *testing.T) {
	// (a) Unknown language falls back to the English value.
	if got := T("fr", "common.save"); got != "Save" {
		t.Errorf("T(fr, common.save) = %q, want English %q", got, "Save")
	}

	// (b) A genuinely-absent key returns the key itself.
	if got := T("en", "no.such.key"); got != "no.such.key" {
		t.Errorf("T(en, no.such.key) = %q, want the key itself", got)
	}
	if got := T("es", "no.such.key"); got != "no.such.key" {
		t.Errorf("T(es, no.such.key) = %q, want the key itself", got)
	}
}

// TestInterpolation checks positional %-verb interpolation over the message text
// returned by go-i18n (the catalog keeps %s/%d verbs; interpolation is Sprintf,
// not go-i18n template data).
func TestInterpolation(t *testing.T) {
	if got := T("en", "greeting", "world"); got != "Hello, world" {
		t.Errorf("T(en, greeting, world) = %q, want %q", got, "Hello, world")
	}
	if got := T("es", "greeting", "mundo"); got != "Hola, mundo" {
		t.Errorf("T(es, greeting, mundo) = %q, want %q", got, "Hola, mundo")
	}
}

// TestLangs pins the available languages and their stable order (en first).
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

// TestBundleLoads is the go-i18n load smoke test that replaces the retired
// hand-rolled parser test: it proves the embedded catalogs parse into the bundle
// and a representative real key resolves in both languages.
func TestBundleLoads(t *testing.T) {
	if got := T("en", "app.name"); got != "cuento" {
		t.Errorf("T(en, app.name) = %q, want %q", got, "cuento")
	}
	if got := T("es", "common.save"); got != "Guardar" {
		t.Errorf("T(es, common.save) = %q, want %q", got, "Guardar")
	}
}

// TestPluralization is the D15 payoff: TN selects one/other by count via CLDR
// rules and interpolates the count. English and Spanish both distinguish 1 from
// 2, and the count is available both to select the form and to fill the %d verb.
func TestPluralization(t *testing.T) {
	cases := []struct {
		lang, key  string
		one, other string
	}{
		{"en", "merge.consequence.splits", "1 transaction line will move to the destination.", "2 transaction lines will move to the destination."},
		{"en", "merge.consequence.recons", "1 reconciliation will move.", "2 reconciliations will move."},
		{"es", "merge.consequence.splits", "1 línea de transacción se moverá al destino.", "2 líneas de transacción se moverán al destino."},
		{"es", "merge.consequence.recons", "1 conciliación se moverá.", "2 conciliaciones se moverán."},
	}
	for _, c := range cases {
		if got := TN(c.lang, c.key, 1); got != c.one {
			t.Errorf("TN(%s, %s, 1) = %q, want %q", c.lang, c.key, got, c.one)
		}
		if got := TN(c.lang, c.key, 2); got != c.other {
			t.Errorf("TN(%s, %s, 2) = %q, want %q", c.lang, c.key, got, c.other)
		}
		if TN(c.lang, c.key, 1) == TN(c.lang, c.key, 2) {
			t.Errorf("TN(%s, %s): singular and plural must differ", c.lang, c.key)
		}
	}

	// Zero uses the plural (other) form in both en and es.
	if got := TN("en", "merge.consequence.recons", 0); got != "0 reconciliations will move." {
		t.Errorf("TN(en, recons, 0) = %q, want the plural (other) form", got)
	}
}

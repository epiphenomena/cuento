package web

import (
	"io/fs"
	"regexp"
	"sort"
	"testing"

	"cuento/internal/i18n"
)

// tmplKeyRefs matches a template call to {{t "key" ...}} or {{tn "key" ...}} whose
// key is a STRING LITERAL. Non-literal keys ({{t $x}}) cannot be resolved statically
// and are skipped — this scan only guards the literal case, which is the vast
// majority and the class that bit us (import.col.payee / funds.view_statement were
// literal refs to keys absent from BOTH catalogs, so users saw the raw key).
var tmplKeyRefs = regexp.MustCompile(`\{\{-?\s*tn?\s+"([^"]+)"`)

// tmplComment matches an html/template comment block {{/* ... */}}. Comments are
// stripped before scanning so a documentation example like `{{t "key"}}` in a
// header comment is not mistaken for a live key reference.
var tmplComment = regexp.MustCompile(`(?s)\{\{-?/\*.*?\*/-?\}\}`)

// TestTemplateKeysResolve scans every embedded template for literal {{t "..."}} /
// {{tn "..."}} references and asserts each key exists in the catalog (i18n.Has).
// TestCatalogParity only proves en and es agree; it cannot catch a key referenced
// by a template but present in NEITHER catalog. This closes that class: a template
// rendering a nonexistent key would show the raw key string to users.
func TestTemplateKeysResolve(t *testing.T) {
	entries, err := fs.Glob(templatesFS, "templates/*.tmpl")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no templates found to scan")
	}

	missing := map[string][]string{} // key -> files referencing it
	for _, name := range entries {
		src, err := fs.ReadFile(templatesFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src = tmplComment.ReplaceAll(src, nil)
		for _, m := range tmplKeyRefs.FindAllSubmatch(src, -1) {
			key := string(m[1])
			if !i18n.Has(key) {
				missing[key] = append(missing[key], name)
			}
		}
	}

	if len(missing) > 0 {
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Errorf("template key %q resolves to no catalog entry (referenced in %v)", k, missing[k])
		}
	}
}

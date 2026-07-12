package web

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"

	"cuento/internal/i18n"
)

// templatesFS holds the embedded HTML templates. Styling is phase 10; these are
// minimal but real html/template pages that render every user-visible string via
// the {{t}} func (rule 9) and never carry inline script (rule 12).
//
//go:embed templates/*.tmpl
var templatesFS embed.FS

// mustParseTemplates parses the embedded templates once at startup with a
// placeholder `t` func (real localization is bound per request in render). A
// parse failure is a build-time defect, so it panics here (the sanctioned
// startup panic) rather than deferring the failure to first request.
func mustParseTemplates() *template.Template {
	// The parse-time t is a no-op stub only so parsing type-checks; every render
	// re-binds t to the request's language before Execute, so this stub never
	// actually runs.
	// t is re-bound per request in render (below); asset is re-bound per request
	// too (it depends on the server's -dev flag and manifest). Both stubs exist
	// only so parsing type-checks a template calling {{t}} or {{asset}}; neither
	// stub actually runs.
	stub := template.FuncMap{
		"t":          func(key string, _ ...any) string { return key },
		"tn":         func(key string, _ int, _ ...any) string { return key },
		"asset":      func(name string) string { return name },
		"shellTitle": shellTitle,
		"strs":       strs,
		"regRowCtx":  makeRegRowCtx, // p12.1 register: pair a row with the page-level column gates
		"regColspan": regColspan,    // p12.1 register: full-width colspan for empty/sentinel cells
		"regMoreURL": regMoreURL,    // p12.1 register: the sentinel's next-page hx-get URL
	}
	t, err := template.New("").Funcs(stub).ParseFS(templatesFS, "templates/*.tmpl")
	if err != nil {
		panic("web: parse templates: " + err.Error())
	}
	return t
}

// render executes the named template for the request's resolved language and
// writes it as an HTML document with no-store caching. It clones the parsed set
// and rebinds `t` to a closure over the request lang (html/template parses once;
// the func map is what carries per-request state), so every {{t "key"}} resolves
// through i18n.T in the right language (D14). status is the HTTP status to send.
//
// no-store is set HERE (not in the header middleware) so authenticated HTML —
// which can carry per-user data — is never cached, while /static and /healthz
// keep their own caching (p10 owns asset cache headers).
func (s *server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	lang := langOf(r.Context())

	clone, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	clone = clone.Funcs(template.FuncMap{
		"t":          func(key string, args ...any) string { return i18n.T(lang, key, args...) },
		"tn":         func(key string, count int, args ...any) string { return i18n.TN(lang, key, count, args...) },
		"asset":      s.assetURL, // hashed URL in prod, unhashed in -dev (p10.1)
		"shellTitle": shellTitle, // pairs a shellPage with a localized head title
		"strs":       strs,       // literal []string for ranging over static enums
	})

	// Render into a buffer first so a template error becomes a clean 500 rather
	// than a half-written 200 body.
	var buf bytes.Buffer
	if err := clone.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// strs is the `strs` template func: it returns its arguments as a []string so a
// template can range over a static enum (e.g. the account types, functional
// classes) without the value list living in the page model. Pure and tiny; used
// by account_form.tmpl.
func strs(items ...string) []string { return items }

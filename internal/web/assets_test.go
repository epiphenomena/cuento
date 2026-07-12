package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cuento/internal/store"
	"cuento/internal/testutil"
)

// newTestApp builds a real *App over a migrated temp db, so tests that reach the
// unexported assetURL (via app.srv) get a fully-constructed server without a nil
// db tripping scs's background session cleanup.
func newTestApp(t *testing.T, cfg Config) *App {
	t.Helper()
	db := testutil.NewDB(t)
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	return NewApp(cfg, db, store.New(db))
}

// p10.1 hashes static assets by content so their URLs change when the content
// changes, which lets us serve them immutable-cached forever while HTML stays
// no-store. These tests are white-box (package web) so they can compute the
// expected hash from the same embedded bytes the manifest sees and reach the
// unexported assetURL through NewApp (the pattern routes_test.go uses).

// embeddedHash returns the first 8 hex chars of the SHA-256 of the embedded
// static file at logical name (e.g. "app.css"), independent of the manifest's
// own implementation so the expectation is derived, not echoed.
func embeddedHash(t *testing.T, name string) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		t.Fatalf("read embedded static/%s: %v", name, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:8]
}

// TestAssetURLHashed: in prod, asset "app.css" maps to /static/app.<8hex>.css
// where <8hex> is the first 8 hex chars of the file's content SHA-256.
func TestAssetURLHashed(t *testing.T) {
	app := newTestApp(t, Config{})

	want := "/static/app." + embeddedHash(t, "app.css") + ".css"
	got := app.srv.assetURL("app.css")
	if got != want {
		t.Fatalf("assetURL(app.css) = %q, want %q", got, want)
	}
}

// TestAssetImmutableCacheHeaders: a request for a hashed asset URL is served
// with immutable + a long max-age (the URL changes when content changes, so
// caching forever is safe).
func TestAssetImmutableCacheHeaders(t *testing.T) {
	h := newTestHandler(t, Config{})

	url := "/static/app." + embeddedHash(t, "app.css") + ".css"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for hashed asset %s", rec.Code, url)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want it to contain immutable", cc)
	}
	if !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control = %q, want a long max-age (31536000)", cc)
	}
	if rec.Body.Len() == 0 {
		t.Error("empty body; hashed URL should serve the embedded file content")
	}
}

// TestVendoredHtmxServed: the p10.3 vendored htmx is a real asset — hashed into
// the manifest and served (so authenticated pages can load it under script-src
// 'self', never a CDN, rule 12).
func TestVendoredHtmxServed(t *testing.T) {
	app := newTestApp(t, Config{})
	// insertHash puts the hash before the LAST extension: htmx.min.<hash>.js.
	want := "/static/htmx.min." + embeddedHash(t, "htmx.min.js") + ".js"
	if got := app.srv.assetURL("htmx.min.js"); got != want {
		t.Fatalf("assetURL(htmx.min.js) = %q, want %q", got, want)
	}

	h := newTestHandler(t, Config{})
	req := httptest.NewRequest(http.MethodGet, want, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("vendored htmx GET %s = %d (len %d), want 200 with body", want, rec.Code, rec.Body.Len())
	}
}

// TestJSUnitTestNotServed: a *.test.js file rides along in //go:embed static but is
// test code, not a web asset — it must be ABSENT from the manifest and 404 at its
// plain /static URL (never exposed on a Public route, rule 12).
func TestJSUnitTestNotServed(t *testing.T) {
	app := newTestApp(t, Config{})
	if _, ok := app.srv.assets.byName["formfocus.test.js"]; ok {
		t.Error("formfocus.test.js is in the asset manifest; test code must not be a served asset")
	}

	h := newTestHandler(t, Config{})
	for _, url := range []string{"/static/formfocus.test.js"} {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (test code is never served)", url, rec.Code)
		}
	}
}

// TestHTMLNoStore: an HTML response (the login page) is served no-store. HTML
// must never be cached; content-hashing is only for static assets. (Guards a
// pre-existing render.go invariant.)
func TestHTMLNoStore(t *testing.T) {
	h := newTestHandler(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for /login", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for HTML", cc)
	}
}

// TestDevModeUnhashed: in -dev, asset "app.css" returns the UNHASHED
// /static/app.css (for live editing) and that URL is served WITHOUT the
// immutable cache header (short/no-cache, not cached forever).
func TestDevModeUnhashed(t *testing.T) {
	app := newTestApp(t, Config{Dev: true})

	if got := app.srv.assetURL("app.css"); got != "/static/app.css" {
		t.Fatalf("dev assetURL(app.css) = %q, want /static/app.css (unhashed for live editing)", got)
	}

	h := newTestHandler(t, Config{Dev: true})
	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for unhashed dev asset", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, dev unhashed asset must NOT be immutable-cached", cc)
	}
}

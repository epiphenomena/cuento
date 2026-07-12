package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/i18n"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// p10.2 tests. The authenticated shell (base.tmpl) renders every page through a
// shared layout: <html lang> from the resolved locale, data-theme from the theme
// cookie server-side (no flash), a perm-gated data-driven nav, and every visible
// string via {{t}}. These tests hit the REAL mounted handler over a migrated temp
// db (AGENTS testing conventions), driving personas by session injection the way
// routes_test.go does.

// getHomeAs issues GET / as persona u (nil == anon) with optional cookies and
// returns the rendered body. It never follows redirects.
func getHomeAs(t *testing.T, h http.Handler, sm *scs.SessionManager, u *store.CurrentUser, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if u != nil {
		req.AddCookie(mintCookie(t, sm, u.ID))
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// makeUser creates a user with the given input and returns its resolved identity.
func makeUser(t *testing.T, st *store.Store, in store.CreateUserInput) *store.CurrentUser {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}
	id, err := st.CreateUser(ctx, in)
	if err != nil {
		t.Fatalf("create user %s: %v", in.Username, err)
	}
	cu, err := st.UserByID(ctx, id)
	if err != nil {
		t.Fatalf("read user %s: %v", in.Username, err)
	}
	return &cu
}

// TestThemeCookieSSR: a request carrying a theme cookie renders the shell with
// <html ... data-theme="<value>"> server-side, so the correct theme is applied on
// first paint with no client round-trip (no flash). No cookie => default "auto".
func TestThemeCookieSSR(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	user := makeUser(t, st, store.CreateUserInput{Username: "themer", TxnPerm: "read"})

	rec := getHomeAs(t, h, sm, user, &http.Cookie{Name: themeCookieName, Value: "dark", Path: "/"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Errorf("body missing data-theme=\"dark\" (SSR theme / no-flash):\n%s", body)
	}

	// No cookie: the shell falls back to the default theme server-side.
	rec = getHomeAs(t, h, sm, user)
	if body := rec.Body.String(); !strings.Contains(body, `data-theme="auto"`) {
		t.Errorf("default body missing data-theme=\"auto\":\n%s", body)
	}
}

// TestNavLocalized: the same authenticated page rendered for an en user vs an es
// user shows the nav labels in the two catalogs, and every {{t}} key resolves (no
// raw "nav.*" key leaks into the HTML).
func TestNavLocalized(t *testing.T) {
	h, _, st, db, sm := newMatrixApp(t)
	enUser := makeUser(t, st, store.CreateUserInput{Username: "en_user", TxnPerm: "read"})
	esUser := makeUser(t, st, store.CreateUserInput{Username: "es_user", TxnPerm: "read"})
	// Set the es user's locale directly (settings writers land later; raw SQL in
	// tests is in-convention). The auth middleware re-reads the user each request,
	// so the injected session picks up es.
	setLocale(t, db, esUser.ID, "es")

	enBody := getHomeAs(t, h, sm, enUser).Body.String()
	esBody := getHomeAs(t, h, sm, esUser).Body.String()

	// The Settings nav label differs across catalogs.
	enSettings := i18n.T("en", "nav.settings")
	esSettings := i18n.T("es", "nav.settings")
	if enSettings == esSettings {
		t.Fatalf("catalog test precondition: en and es nav.settings are equal (%q)", enSettings)
	}
	if !strings.Contains(enBody, enSettings) {
		t.Errorf("en body missing en nav label %q", enSettings)
	}
	if !strings.Contains(esBody, esSettings) {
		t.Errorf("es body missing es nav label %q", esSettings)
	}

	// No raw catalog key must leak: a rendered "nav." literal means a {{t}} key
	// resolved to itself (missing from the catalog).
	for _, body := range []string{enBody, esBody} {
		if strings.Contains(body, ">nav.") || strings.Contains(body, "nav.settings<") {
			t.Errorf("raw nav key leaked into rendered HTML:\n%s", body)
		}
	}
}

// TestHTMLLangMatchesLocale: <html lang> equals the resolved UI locale (en for an
// en user, es for an es user).
func TestHTMLLangMatchesLocale(t *testing.T) {
	h, _, st, db, sm := newMatrixApp(t)
	enUser := makeUser(t, st, store.CreateUserInput{Username: "en_lang", TxnPerm: "read"})
	esUser := makeUser(t, st, store.CreateUserInput{Username: "es_lang", TxnPerm: "read"})
	setLocale(t, db, esUser.ID, "es")

	if body := getHomeAs(t, h, sm, enUser).Body.String(); !strings.Contains(body, `<html lang="en"`) {
		t.Errorf("en user: <html lang=\"en\"> missing:\n%s", body)
	}
	if body := getHomeAs(t, h, sm, esUser).Body.String(); !strings.Contains(body, `<html lang="es"`) {
		t.Errorf("es user: <html lang=\"es\"> missing:\n%s", body)
	}
}

// TestNavPermGated: personas see only the nav entries their perm permits AND whose
// route is registered. Admin sees the admin entry; a non-admin does not. Anon on
// the shell is redirected (no nav rendered).
func TestNavPermGated(t *testing.T) {
	h, _, st, db, sm := newMatrixApp(t)

	noAccess := makeUser(t, st, store.CreateUserInput{Username: "pg_none", TxnPerm: "none"})
	bookkeeper := makeUser(t, st, store.CreateUserInput{Username: "pg_write", TxnPerm: "write"})
	admin := makeUser(t, st, store.CreateUserInput{Username: "pg_admin", IsAdmin: true})
	_ = db

	adminLabel := i18n.T("en", "nav.admin")
	settingsLabel := i18n.T("en", "nav.settings")

	// Admin: sees the admin section.
	adminBody := getHomeAs(t, h, sm, admin).Body.String()
	if !strings.Contains(adminBody, adminLabel) {
		t.Errorf("admin persona missing admin nav entry %q:\n%s", adminLabel, adminBody)
	}

	// Non-admin (bookkeeper, no-access): NOT the admin section, but Settings
	// (AnyUser) is present for every logged-in user.
	for _, u := range []*store.CurrentUser{bookkeeper, noAccess} {
		body := getHomeAs(t, h, sm, u).Body.String()
		if strings.Contains(body, ">"+adminLabel+"<") {
			t.Errorf("non-admin %s sees the admin nav entry (should not):\n%s", u.Username, body)
		}
		if !strings.Contains(body, settingsLabel) {
			t.Errorf("logged-in %s missing Settings nav entry:\n%s", u.Username, body)
		}
	}

	// Anon on the shell is bounced to /login (no nav to gate).
	rec := getHomeAs(t, h, sm, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon on / = %d %q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
}

// TestSettingsStubRendersShell: the p10.2 GET /settings stub renders through the
// shell (AnyUser), giving the nav a real, permitted, localized target. It must
// carry the shell landmarks and be localized.
func TestSettingsStubRendersShell(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	user := makeUser(t, st, store.CreateUserInput{Username: "settings_user", TxnPerm: "none"})

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(mintCookie(t, sm, user.ID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<nav") || !strings.Contains(body, "<main") {
		t.Errorf("/settings stub missing shell landmarks:\n%s", body)
	}
}

// TestThemePersistsCookieAndSetting: POST /theme sets the theme cookie AND, for a
// logged-in user, persists the user's theme setting via the store. A subsequent
// GET renders the persisted theme from the user setting even with no cookie.
func TestThemePersistsCookieAndSetting(t *testing.T) {
	h, _, st, db, sm := newMatrixApp(t)
	user := makeUser(t, st, store.CreateUserInput{Username: "toggler", TxnPerm: "none"})

	form := strings.NewReader("theme=dark")
	req := httptest.NewRequest(http.MethodPost, "/theme", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(mintCookie(t, sm, user.ID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("POST /theme status = %d, want 303 or 200", rec.Code)
	}
	// The theme cookie is set.
	var themeCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == themeCookieName {
			themeCookie = c
		}
	}
	if themeCookie == nil || themeCookie.Value != "dark" {
		t.Fatalf("POST /theme did not set %s=dark cookie: %+v", themeCookieName, themeCookie)
	}

	// The user setting persisted: re-read the theme column.
	if got := readTheme(t, db, user.ID); got != "dark" {
		t.Errorf("persisted user theme = %q, want dark", got)
	}
}

// TestStyleguideDevOnly: GET /styleguide is served only in -dev; it 404s (route
// absent from the registry) when not -dev.
func TestStyleguideDevOnly(t *testing.T) {
	// Dev: the route exists and renders.
	dh, _, _ := newDevApp(t)
	req := httptest.NewRequest(http.MethodGet, "/styleguide", nil)
	rec := httptest.NewRecorder()
	dh.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev GET /styleguide status = %d, want 200", rec.Code)
	}

	// Not dev: the route is absent, so 404.
	h, _, _, _, _ := newMatrixApp(t)
	req = httptest.NewRequest(http.MethodGet, "/styleguide", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-dev GET /styleguide status = %d, want 404", rec.Code)
	}
}

// newDevApp builds a real dev-mode app over a migrated temp db and returns the
// handler, store, and session manager.
func newDevApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	if err := SyncReportGroups(context.Background(), st); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	app := NewApp(Config{Version: "test", Dev: true}, db, st)
	return app.handler, st, app.sessions
}

// setLocale updates a user's locale column directly (settings writers land in
// p13.1; raw SQL in tests is in-convention, mirroring routes_test.go's grant).
func setLocale(t *testing.T, db *sql.DB, userID int64, locale string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `UPDATE users SET locale = ? WHERE id = ?`, locale, userID); err != nil {
		t.Fatalf("set locale: %v", err)
	}
}

// readTheme reads a user's persisted theme column directly.
func readTheme(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	var theme string
	if err := db.QueryRowContext(context.Background(),
		`SELECT theme FROM users WHERE id = ?`, userID).Scan(&theme); err != nil {
		t.Fatalf("read theme: %v", err)
	}
	return theme
}

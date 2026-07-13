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

// TestShellRendersBrandLogo: the authenticated shell header carries the "Open
// Ledger & Star" mark as an inline, themeable SVG (element classes styled from
// app.css — no inline style, rule 12) inside the brand link, and that link has an
// accessible name (aria-label = app.name). The favicon is wired in the head.
func TestShellRendersBrandLogo(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	user := makeUser(t, st, store.CreateUserInput{Username: "brander", TxnPerm: "read"})

	body := getHomeAs(t, h, sm, user).Body.String()

	if !strings.Contains(body, `aria-label="cuento"`) {
		t.Errorf("brand link missing accessible name (aria-label):\n%s", body)
	}
	if !strings.Contains(body, `class="brand-icon"`) {
		t.Errorf("shell missing the inline brand mark (svg.brand-icon):\n%s", body)
	}
	for _, cls := range []string{`class="logo-book"`, `class="logo-star"`} {
		if !strings.Contains(body, cls) {
			t.Errorf("brand mark missing themeable shape %s:\n%s", cls, body)
		}
	}
	if !strings.Contains(body, `rel="icon"`) || !strings.Contains(body, "favicon.") {
		t.Errorf("shell head missing the favicon link:\n%s", body)
	}
}

// TestNavCurrentAccent: the nav entry matching the request path is marked
// aria-current="page" (the gold active-nav accent), and only that one is.
func TestNavCurrentAccent(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	user := makeUser(t, st, store.CreateUserInput{Username: "navcur", TxnPerm: "read"})

	// On /settings the Settings entry is current; Home is not.
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(mintCookie(t, sm, user.ID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `href="/settings" aria-current="page"`) {
		t.Errorf("/settings nav entry not marked current:\n%s", body)
	}
	if strings.Contains(body, `href="/" aria-current="page"`) {
		t.Errorf("root nav entry wrongly marked current on /settings:\n%s", body)
	}
}

// TestSubNavRendersPerSection (p23.5): the two-row nav. The section bar appears
// only on sections that have sub-nav, lists that section's sub-pages (marking the
// current one), and is absent on sections without one.
func TestSubNavRendersPerSection(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	admin := makeUser(t, st, store.CreateUserInput{Username: "subnav_admin", IsAdmin: true})

	// /admin/users: the section bar shows the admin sub-pages, Users current.
	rec := asUser(t, h, sm, admin.ID, http.MethodGet, "/admin/users", nil)
	body := rec.Body.String()
	if !strings.Contains(body, `class="app-subnav"`) {
		t.Fatalf("/admin/users missing the section bar:\n%s", body)
	}
	for _, href := range []string{"/admin/subsidiaries", "/admin/currencies", "/admin/org"} {
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("/admin section bar missing sub-link %s", href)
		}
	}
	if !strings.Contains(body, `href="/admin/users" aria-current="page"`) {
		t.Errorf("/admin/users sub-nav entry not marked current:\n%s", body)
	}

	// /accounts has no sub-nav LINKS but DOES render a section bar for its controls
	// (p23.10): the subsidiary/active filters + New/Merge moved into the bar.
	rec = asUser(t, h, sm, admin.ID, http.MethodGet, "/accounts", nil)
	body = rec.Body.String()
	if !strings.Contains(body, `class="app-subnav"`) || !strings.Contains(body, `class="app-subnav-controls"`) {
		t.Errorf("/accounts should render a section bar with controls:\n%s", body)
	}
	if !strings.Contains(body, `class="subnav-filters"`) {
		t.Errorf("/accounts section bar missing the filter form")
	}

	// A page with neither sub-nav nor controls (the reports index) renders no bar.
	rec = asUser(t, h, sm, admin.ID, http.MethodGet, "/reports", nil)
	if strings.Contains(rec.Body.String(), `class="app-subnav"`) {
		t.Errorf("/reports should render no section bar")
	}

	// /schedules belongs to the Budgets section: the bar shows Budgets + Schedules,
	// with Schedules current (a distinct top-level path, same section).
	rec = asUser(t, h, sm, admin.ID, http.MethodGet, "/schedules", nil)
	body = rec.Body.String()
	if !strings.Contains(body, `href="/budgets"`) || !strings.Contains(body, `href="/schedules" aria-current="page"`) {
		t.Errorf("/schedules section bar wrong (want Budgets + current Schedules):\n%s", body)
	}
}

// Theme persistence (cookie + user setting) is exercised via POST /settings in
// settings_test.go; p23.1 removed the standalone POST /theme route.

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

// TestFooterVersion (p18.1): the shell footer surfaces the configured build
// version -- the release-time `-X main.version` value flows main.version ->
// Config.Version -> baseData.Version -> the footer partial. A distinctive
// version is fed via Config so the assertion has discriminating power (it would
// not appear if the wiring were dead); the version string itself is not
// translated, only the surrounding "Version %s" label is a catalog key.
func TestFooterVersion(t *testing.T) {
	const version = "v9.9.9-footer-test"

	db := testutil.NewDB(t)
	st := store.New(db)
	if err := SyncReportGroups(context.Background(), st); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	app := NewApp(Config{Version: version}, db, st)

	user := makeUser(t, st, store.CreateUserInput{Username: "footer_user", TxnPerm: "read"})
	rec := getHomeAs(t, app.handler, app.sessions, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	want := i18n.T("en", "footer.version", version)
	if !strings.Contains(body, want) {
		t.Errorf("footer missing version label %q:\n%s", want, body)
	}
	// The raw version string is present verbatim (not swallowed by escaping).
	if !strings.Contains(body, version) {
		t.Errorf("footer missing version string %q", version)
	}
	// Confirm it renders inside the footer landmark, not incidentally elsewhere.
	if _, after, ok := strings.Cut(body, `<footer class="app-footer">`); !ok || !strings.Contains(after, version) {
		t.Errorf("version %q not rendered within the app footer", version)
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

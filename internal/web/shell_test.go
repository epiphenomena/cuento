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

	// The "All" nav label differs across catalogs (p26.77 renamed the old "More" hub
	// to the "All" landing; the top nav's discriminating label is now nav.all).
	enAll := i18n.T("en", "nav.all")
	esAll := i18n.T("es", "nav.all")
	if enAll == esAll {
		t.Fatalf("catalog test precondition: en and es nav.all are equal (%q)", enAll)
	}
	if !strings.Contains(enBody, enAll) {
		t.Errorf("en body missing en nav label %q", enAll)
	}
	if !strings.Contains(esBody, esAll) {
		t.Errorf("es body missing es nav label %q", esAll)
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

	allLabel := i18n.T("en", "nav.all")

	// p26.77: the top nav carries the AnyUser "All" landing entry (formerly "More"); the
	// per-destination cards on the landing (/more) are perm-gated. Every logged-in user
	// sees the "All" top-nav entry.
	for _, u := range []*store.CurrentUser{admin, bookkeeper, noAccess} {
		if !strings.Contains(getHomeAs(t, h, sm, u).Body.String(), allLabel) {
			t.Errorf("logged-in %s missing the All nav entry", u.Username)
		}
	}

	// The Admin cards (-> /admin/*) show on the All landing only for an admin; Settings
	// (AnyUser) shows for everyone. p26.77 lists the admin SUB-pages as cards, so the
	// discriminating admin href is /admin/users (not the /admin hub).
	adminAll := asUser(t, h, sm, admin.ID, http.MethodGet, "/more", nil).Body.String()
	if !strings.Contains(adminAll, `href="/admin/users"`) {
		t.Errorf("admin persona missing the Admin cards on the All landing:\n%s", adminAll)
	}
	for _, u := range []*store.CurrentUser{bookkeeper, noAccess} {
		body := asUser(t, h, sm, u.ID, http.MethodGet, "/more", nil).Body.String()
		if strings.Contains(body, `href="/admin/users"`) {
			t.Errorf("non-admin %s sees the Admin cards on the All landing (should not):\n%s", u.Username, body)
		}
		if !strings.Contains(body, `href="/settings"`) {
			t.Errorf("logged-in %s missing the Settings card on the All landing:\n%s", u.Username, body)
		}
	}

	// Anon on the shell is bounced to /login (no nav to gate).
	rec := getHomeAs(t, h, sm, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon on / = %d %q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
}

// TestAllLandingCards (p26.77): the "All" landing (/more) renders a grouped grid of
// perm-gated cards for every reachable destination — sections, sub-items, AND the
// reports the user is GRANTED (not merely "has some grant"). It proves: (1) grouping
// headers render; (2) an admin sees section + report + admin cards; (3) a report card
// appears ONLY for its granted group; (4) a pure submitter sees only their cards.
func TestAllLandingCards(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	admin := makeUser(t, st, store.CreateUserInput{Username: "all_admin", IsAdmin: true})
	// A viewer granted ONLY the "funds" report group: sees fund reports, NOT financial.
	viewer := makeUser(t, st, store.CreateUserInput{Username: "all_viewer", TxnPerm: "read"})
	if err := st.GrantReportGroup(ctx, viewer.ID, "funds", nil); err != nil {
		t.Fatalf("grant funds group: %v", err)
	}
	// A pure submitter: ExpenseSubmit only, no ledger/report access.
	submitter := makeUser(t, st, store.CreateUserInput{Username: "all_sub", TxnPerm: "none"})
	if err := st.SetUserCanSubmitExpenses(ctx, submitter.ID, true); err != nil {
		t.Fatalf("set can-submit: %v", err)
	}

	adminBody := asUser(t, h, sm, admin.ID, http.MethodGet, "/more", nil).Body.String()
	// Grouping headers render (the grid is sectioned, not a flat list).
	if !strings.Contains(adminBody, `class="hub-section-title"`) {
		t.Errorf("admin All landing missing section headers:\n%s", adminBody)
	}
	// Section, admin sub-page, and report cards all present for an admin.
	for _, href := range []string{"/accounts", "/funds", "/admin/users", "/reports/trial_balance", "/settings"} {
		if !strings.Contains(adminBody, `href="`+href+`"`) {
			t.Errorf("admin All landing missing card href=%q", href)
		}
	}

	// The funds-only viewer sees a fund report card but NOT a financial-group report
	// (trial_balance is "financial"); the fund_activity report is "funds".
	viewerBody := asUser(t, h, sm, viewer.ID, http.MethodGet, "/more", nil).Body.String()
	if !strings.Contains(viewerBody, `href="/reports/fund_activity"`) {
		t.Errorf("funds-viewer missing the granted fund report card:\n%s", viewerBody)
	}
	if strings.Contains(viewerBody, `href="/reports/trial_balance"`) {
		t.Errorf("funds-viewer sees an UNGRANTED financial report card (per-report grant leak):\n%s", viewerBody)
	}

	// The pure submitter sees My expenses + Settings, but no ledger/admin/report cards.
	subBody := asUser(t, h, sm, submitter.ID, http.MethodGet, "/more", nil).Body.String()
	if !strings.Contains(subBody, `href="/expenses"`) || !strings.Contains(subBody, `href="/settings"`) {
		t.Errorf("submitter missing expected cards (expenses/settings):\n%s", subBody)
	}
	for _, href := range []string{"/accounts", "/admin/users", "/reports/trial_balance"} {
		if strings.Contains(subBody, `href="`+href+`"`) {
			t.Errorf("submitter sees an unreachable card href=%q:\n%s", href, subBody)
		}
	}
}

// TestAllCardsHaveDescription (p26.83): every resolved card on the "All" landing —
// section cards AND report cards — carries a non-empty description under its title. It
// resolves the FULL card set for an admin (who reaches every section and every report),
// then asserts each card's Desc is present and is REAL localized text, not the raw i18n
// key echoed back (i18n.T returns the key verbatim when it is absent from the catalog, so
// a missing/typo'd key would otherwise pass a bare non-empty check). This is the check
// that matches the requirement "every card has a description"; the e2e asserts only one.
func TestAllCardsHaveDescription(t *testing.T) {
	app := newTestApp(t, Config{})
	s := app.srv
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if err := SyncReportGroups(ctx, s.store); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	admin := makeUser(t, s.store, store.CreateUserInput{Username: "desc_admin", IsAdmin: true})

	sections := s.allSections(ctx, admin)
	if len(sections) == 0 {
		t.Fatal("admin resolved no sections on the All landing")
	}
	cardCount := 0
	for _, sec := range sections {
		for _, c := range sec.Cards {
			cardCount++
			if strings.TrimSpace(c.Desc) == "" {
				t.Errorf("card %q (href %q) has no description", c.Label, c.Href)
			}
			// The description must be resolved catalog text, not the raw key i18n.T echoes
			// for an absent key. Every desc key is "all.desc.*" or "reports.*.desc"; real
			// text neither starts with "all.desc." nor ends with ".desc".
			if strings.HasPrefix(c.Desc, "all.desc.") || strings.HasSuffix(c.Desc, ".desc") {
				t.Errorf("card %q (href %q) shows the raw desc key %q (catalog gap)", c.Label, c.Href, c.Desc)
			}
		}
	}
	// Guard the guard: an admin must resolve the full grid (15 section cards + 13 report
	// cards; p27.3 merged the old budgets+schedules cards into one budget-plans card).
	// If the card model ever changes, this pins that the loop actually exercised every
	// card rather than silently iterating an empty set.
	if cardCount < 28 {
		t.Errorf("admin resolved only %d cards; expected the full grid (>=28)", cardCount)
	}
}

// TestAllLandingFoldsReportGroups (p28.13, reordered p29.10): each dimension's report
// group folds INTO its functional home section — financial into a report-only Financial
// section (no nav card), funds into the /funds section, programs into the /programs
// section, reconciliation into Accounts, budget into Budget plans. Only the tax (990)
// group stays a distinct trailing report section. The Accounts section no longer carries
// the funds/programs cards (they have their own sections) and now carries the Import card.
// Section ORDER is pinned: Accounts, then Financial, then Funds, then Programs, all ABOVE
// Budget plans. Verified per-section by label + card hrefs on the resolved model (an admin
// reaches every section and report).
func TestAllLandingFoldsReportGroups(t *testing.T) {
	app := newTestApp(t, Config{})
	s := app.srv
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if err := SyncReportGroups(ctx, s.store); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}
	admin := makeUser(t, s.store, store.CreateUserInput{Username: "fold_admin", IsAdmin: true})

	sections := s.allSections(ctx, admin)

	// hrefs of the cards in the section whose Label equals the localized key, or nil when
	// no such section exists (dropped as empty).
	cardsIn := func(labelKey string) []string {
		label := i18n.T("en", labelKey)
		for _, sec := range sections {
			if sec.Label == label {
				var hrefs []string
				for _, c := range sec.Cards {
					hrefs = append(hrefs, c.Href)
				}
				return hrefs
			}
		}
		return nil
	}
	contains := func(hrefs []string, want string) bool {
		for _, h := range hrefs {
			if h == want {
				return true
			}
		}
		return false
	}
	// sectionIndex returns the position of the section with the given localized label, or
	// -1 when absent, so we can assert relative order.
	sectionIndex := func(labelKey string) int {
		label := i18n.T("en", labelKey)
		for i, sec := range sections {
			if sec.Label == label {
				return i
			}
		}
		return -1
	}
	// sectionCount counts how many sections carry the given localized label. Because
	// nav.funds and reports.group.funds both localize to "Funds" (likewise programs), a
	// folded group and a stray trailing report section would share a label — so "folded,
	// not trailing" is asserted as "exactly one section with that label" rather than by key.
	sectionCount := func(labelKey string) int {
		label := i18n.T("en", labelKey)
		n := 0
		for _, sec := range sections {
			if sec.Label == label {
				n++
			}
		}
		return n
	}

	// Accounts now holds accounts + reconciliations + import, plus the folded reconciliation
	// report cards — and NO longer the funds/programs cards (own sections now).
	accounts := cardsIn("nav.accounts")
	if !contains(accounts, "/accounts") || !contains(accounts, "/reconciliations") || !contains(accounts, "/import") {
		t.Errorf("accounts section missing accounts/reconciliations/import: %v", accounts)
	}
	if !contains(accounts, "/reports/reconciliation_statement") {
		t.Errorf("accounts section missing folded reconciliation report card: %v", accounts)
	}
	if contains(accounts, "/funds") || contains(accounts, "/programs") {
		t.Errorf("accounts section should NOT contain funds/programs cards: %v", accounts)
	}
	if cardsIn("reports.group.reconciliation") != nil {
		t.Errorf("reconciliation report group should be folded, not a trailing section")
	}

	// Financial is a report-only section (no nav card): it carries financial report cards
	// and NO management page.
	financial := cardsIn("reports.group.financial")
	if !contains(financial, "/reports/balance_sheet") {
		t.Errorf("financial section missing financial report card: %v", financial)
	}
	for _, h := range financial {
		if !strings.HasPrefix(h, "/reports/") {
			t.Errorf("financial section should be report-only, found nav card %q", h)
		}
	}

	// Funds pairs the /funds management page with its report group folded in.
	funds := cardsIn("nav.funds")
	if !contains(funds, "/funds") || !contains(funds, "/reports/capital_campaign") {
		t.Errorf("funds section missing /funds card or folded fund report: %v", funds)
	}
	// One "Funds" section only — the report group folds in, it does not also trail.
	if n := sectionCount("nav.funds"); n != 1 {
		t.Errorf("expected exactly one Funds section (folded, not trailing), got %d", n)
	}

	// Programs pairs the /programs management page with its report group folded in.
	programs := cardsIn("nav.programs")
	if !contains(programs, "/programs") || !contains(programs, "/reports/program_statement") {
		t.Errorf("programs section missing /programs card or folded program report: %v", programs)
	}
	if n := sectionCount("nav.programs"); n != 1 {
		t.Errorf("expected exactly one Programs section (folded, not trailing), got %d", n)
	}

	// Budget report cards fold into the budget-plans section (after the "make a budget"
	// card), NOT into a standalone reports.group.budget section.
	budget := cardsIn("nav.budgetplans")
	if !contains(budget, "/budget-plans") || !contains(budget, "/reports/budget_variance") {
		t.Errorf("budget section missing folded budget report cards: %v", budget)
	}
	if cardsIn("reports.group.budget") != nil {
		t.Errorf("budget report group should be folded, not a trailing section")
	}

	// Tax (the 990 package) is the ONLY report group that stays a distinct trailing section.
	if cardsIn("reports.group.tax") == nil {
		t.Errorf("tax report group should remain a trailing section (not folded)")
	}

	// Order: Accounts, then Financial (2nd), then Funds, then Programs, all ABOVE Budget plans.
	iAcct := sectionIndex("nav.accounts")
	iFin := sectionIndex("reports.group.financial")
	iFunds := sectionIndex("nav.funds")
	iProg := sectionIndex("nav.programs")
	iBudget := sectionIndex("nav.budgetplans")
	if iAcct >= iFin || iFin >= iFunds || iFunds >= iProg || iProg >= iBudget {
		t.Errorf("section order wrong: accounts=%d financial=%d funds=%d programs=%d budget=%d (want strictly increasing)",
			iAcct, iFin, iFunds, iProg, iBudget)
	}
	if iFin != iAcct+1 {
		t.Errorf("Financial section should be immediately after Accounts: accounts=%d financial=%d", iAcct, iFin)
	}
}

// TestHomeRendersAllGrid (p26.78): GET / (home) now serves the SAME "All" card grid as
// /more — the card grid is the landing, not the chart of accounts. It renders the full
// shell (landmarks) with the grouped cards, and the "All" top-nav entry is marked
// current on the bare "/".
func TestHomeRendersAllGrid(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)
	user := makeUser(t, st, store.CreateUserInput{Username: "home_user", TxnPerm: "read"})

	body := getHomeAs(t, h, sm, user).Body.String()
	// The card grid (a section header + at least the Accounts card) renders on /.
	if !strings.Contains(body, `class="hub-section-title"`) {
		t.Errorf("GET / missing the All card grid (no section header):\n%s", body)
	}
	if !strings.Contains(body, `href="/accounts"`) {
		t.Errorf("GET / missing the Accounts card:\n%s", body)
	}
	// The "All" entry (served at /more) is the current nav on the bare "/".
	navStart := strings.Index(body, `<nav class="app-nav"`)
	navEnd := strings.Index(body[navStart:], "</nav>")
	nav := body[navStart : navStart+navEnd]
	if !strings.Contains(nav, `href="/more" aria-current="page"`) {
		t.Errorf("All nav entry not marked current on /:\n%s", nav)
	}
	// Accounts is NOT current on / anymore (the landing moved off accounts).
	if strings.Contains(nav, `href="/accounts" aria-current="page"`) {
		t.Errorf("Accounts should NOT be current on / after p26.78:\n%s", nav)
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
	// p29.4: the accounts filter bar now renders through the shared reports .report-params
	// markup (label-above controls) so the two filter bars read identically.
	if !strings.Contains(body, `class="report-params accounts-filters"`) {
		t.Errorf("/accounts section bar missing the filter form")
	}

	// A page with neither sub-nav nor controls (the reports index) renders no bar.
	rec = asUser(t, h, sm, admin.ID, http.MethodGet, "/reports", nil)
	if strings.Contains(rec.Body.String(), `class="app-subnav"`) {
		t.Errorf("/reports should render no section bar")
	}

	// /budget-plans belongs to the "More" section (p27.3 retired the old /budgets +
	// /schedules): the bar shows sibling entries (e.g. Funds) with Budget plans current.
	rec = asUser(t, h, sm, admin.ID, http.MethodGet, "/budget-plans", nil)
	body = rec.Body.String()
	if !strings.Contains(body, `href="/funds"`) || !strings.Contains(body, `href="/budget-plans" aria-current="page"`) {
		t.Errorf("/budget-plans section bar wrong (want Funds sibling + current Budget plans):\n%s", body)
	}
}

// TestExpensesNavConsolidated (p24): the two expense workspaces live under ONE
// top-level "Expenses" section. The parent shows when the user can do EITHER (submit
// or review) and lands on whichever they can reach; the section bar carries the two
// children, each perm-gated; and on a nested review path only the more-specific child
// is marked current (no double aria-current from prefix nesting).
func TestExpensesNavConsolidated(t *testing.T) {
	h, _, st, _, sm := newMatrixApp(t)

	submitter := mkSubmitter(t, st, "nav_submitter")                                               // ExpenseSubmit only
	reviewer := makeUser(t, st, store.CreateUserInput{Username: "nav_reviewer", TxnPerm: "write"}) // TxnWrite only
	reader := makeUser(t, st, store.CreateUserInput{Username: "nav_reader", TxnPerm: "read"})      // neither
	admin := makeUser(t, st, store.CreateUserInput{Username: "nav_exp_admin", IsAdmin: true})      // both (D10)
	expensesLabel := i18n.T("en", "nav.expenses")

	// Pure submitter: parent shown, lands on /expenses (the submit workspace).
	body := asUser(t, h, sm, submitter, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, expensesLabel) || !strings.Contains(body, `href="/expenses"`) {
		t.Errorf("submitter missing the Expenses top-nav -> /expenses:\n%s", body)
	}

	// Pure reviewer (no submit grant): parent lands on /expenses/review, not /expenses.
	body = asUser(t, h, sm, reviewer.ID, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, `href="/expenses/review"`) {
		t.Errorf("reviewer's Expenses top-nav should land on /expenses/review:\n%s", body)
	}

	// Reader (neither perm): no Expenses top-nav at all (home is the accounts section,
	// so no expenses link should appear anywhere on the page).
	body = asUser(t, h, sm, reader.ID, http.MethodGet, "/", nil).Body.String()
	if strings.Contains(body, `href="/expenses"`) || strings.Contains(body, `href="/expenses/review"`) {
		t.Errorf("reader should see no Expenses nav:\n%s", body)
	}

	// Section bar on /expenses (submitter): My expenses child present + current; the
	// Review child is hidden (no TxnWrite).
	body = asUser(t, h, sm, submitter, http.MethodGet, "/expenses", nil).Body.String()
	if !strings.Contains(body, `class="app-subnav"`) {
		t.Errorf("/expenses missing the section bar:\n%s", body)
	}
	if !strings.Contains(body, `href="/expenses" aria-current="page"`) {
		t.Errorf("/expenses: the My-expenses child is not current:\n%s", body)
	}
	if strings.Contains(body, `href="/expenses/review"`) {
		t.Errorf("submitter must not see the Expense-review child:\n%s", body)
	}

	// Admin on a NESTED review path: both children show, but only Expense review is
	// current. The top-nav parent (href="/expenses") is legitimately current too, so
	// `href="/expenses" aria-current` must appear EXACTLY once (the parent) — a second
	// occurrence would be the section-bar My-expenses child wrongly double-marked.
	body = asUser(t, h, sm, admin.ID, http.MethodGet, "/expenses/review", nil).Body.String()
	if !strings.Contains(body, `href="/expenses/review" aria-current="page"`) {
		t.Errorf("admin /expenses/review: the Review child is not current:\n%s", body)
	}
	if !strings.Contains(body, `href="/expenses"`) {
		t.Errorf("admin should also see the My-expenses child:\n%s", body)
	}
	if n := strings.Count(body, `href="/expenses" aria-current="page"`); n != 1 {
		t.Errorf("admin /expenses/review: `href=\"/expenses\" aria-current` count = %d, want 1 (parent only; a 2 means the child was double-marked):\n%s", n, body)
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

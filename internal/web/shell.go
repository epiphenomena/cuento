package web

import (
	"context"
	"net/http"
	"strings"

	"cuento/internal/i18n"
	"cuento/internal/store"
)

// p10.2: the authenticated shell. base.tmpl is the layout every authenticated
// page extends -- semantic landmarks, <html lang> from the resolved locale,
// data-theme applied SERVER-SIDE from the theme cookie (no flash), a flash region,
// and a perm-gated, data-driven nav. This file holds the shell's Go side: the
// top-nav + section-bar tables (p23.5/p23.9), the "More" hub, and the home /
// settings / styleguide handlers. (Theme is set via /settings, p23.1.)

// themeCookieName is the cookie carrying the chosen theme. It is read server-side
// in baseData so the correct data-theme is present on first paint (no flash / no
// client round-trip). Distinct from the "lang" cookie (middleware) and the scs
// session cookie.
const themeCookieName = "cuento_theme"

// defaultTheme is the data-theme applied when no cookie (and no user setting) is
// present. "auto" follows the OS via CSS color-scheme (see app.css). It matches
// the users.theme column default (00006_credentials_perms.sql).
const defaultTheme = "auto"

// navEntry is one top-level section of the authenticated nav (Appendix F). It is
// DATA: LabelKey is an i18n key, Href a registered route, Perm the permission a
// user must satisfy to see it. Rendering a section only when it is BOTH permitted
// AND registered avoids dead links -- later phases (p11-p18) append their entry
// here when they add the route, and it lights up automatically.
type navEntry struct {
	LabelKey string
	Href     string
	Perm     Perm
}

// navSections is the ordered top-level nav (p23.9): a lean top bar of accounts
// (the landing), reports, my-expenses, expense-review, and a single AnyUser "More"
// hub. Everything else (funds, programs, reconciliations, budgets, import,
// settings, admin) hangs off More as perm-gated cards + section-bar links, so the
// top nav stays short. Entries are filtered to registered+permitted at render
// (visibleNav drops any entry whose Href has no route yet, or that the user's perm
// fails), keeping the nav honest (no dead links) while being trivially appendable.
func navSections() []navEntry {
	return []navEntry{
		// p23.8: the brand logo is "home" (-> the chart of accounts); no separate
		// Home entry. Accounts leads the nav and is the landing.
		{"nav.accounts", "/accounts", TxnRead},
		{"nav.reports", "/reports", ReportGroup("")},
		// p20.2: the submitter workspace. Gated by ExpenseSubmit (the standalone
		// capability) so a PURE submitter (txn_perm=none, no grants) sees ONLY this
		// entry (+ More), never the ledger/reports/admin -- the access boundary. An
		// admin sees it too (is_admin implies everything, D10).
		{"nav.myexpenses", "/expenses", ExpenseSubmit},
		// p20.3: the reviewer queue (TxnWrite). A submit-only user never sees it (fails
		// TxnWrite); an editing user (or admin) does.
		{"nav.expensereview", "/expenses/review", TxnWrite},
		// p23.9: everything else (funds, programs, reconciliations, budgets, import,
		// settings, admin) lives under the AnyUser "More" hub as perm-gated cards,
		// keeping the top nav lean. The hub page is a card grid; the second-level menu
		// carries the same links (subNavGroups).
		{"nav.more", "/more", AnyUser},
	}
}

// navItem is a nav entry resolved for one request: its already-localized label and
// href, ready for the template (which never sees a Perm or a raw key). Current
// marks the section matching the request path so the shell can render
// aria-current="page" (the gold active-nav accent, ux brand identity).
type navItem struct {
	Label   string
	Href    string
	Current bool
}

// visibleNav resolves navSections for the current request: it keeps an entry only
// when (a) its route is REGISTERED (a GET route for the Href exists -- no dead
// links) and (b) the current user SATISFIES its Perm (reusing decide, the p06.3
// policy, so nav visibility and route enforcement can never disagree). The
// ReportGroup case is treated as "any report grant (or admin)" for nav purposes:
// the index route lands in p15 and will gate the concrete reports.
func (s *server) visibleNav(ctx context.Context, u *store.CurrentUser, currentPath string) []navItem {
	registered := s.registeredGetPaths()
	lang := langOf(ctx)

	var out []navItem
	for _, e := range navSections() {
		if !registered[e.Href] {
			continue // route not wired yet (later phase) -> no dead link
		}
		if !s.navPermits(ctx, u, e.Perm) {
			continue
		}
		out = append(out, navItem{
			Label: i18n.T(lang, e.LabelKey),
			Href:  e.Href,
			// Accounts is also the landing (p23.8), so highlight it on the bare "/".
			Current: isCurrentNav(e.Href, currentPath) || (e.Href == "/accounts" && currentPath == "/"),
		})
	}
	return out
}

// subNavGroup is one top-level section's SECOND-ROW navigation (p23.5): the request
// belongs to the group when its path matches any Prefix, and Entries are the
// sub-pages shown in the section bar. Data-driven exactly like navSections — a
// section lights up its sub-nav by appending an entry here; filtered to
// registered+permitted at render so there are no dead links.
type subNavGroup struct {
	Prefixes []string // a request under any of these belongs to this section
	Entries  []navEntry
}

// subNavGroups is the ordered section→sub-nav map (p23.5). Only sections with real
// sub-pages appear; the rest render no second row (the frame stays out of the way
// until a section needs it, and later phases add filters/search into this slot).
// Budgets and Schedules are one section (Schedules is a distinct top-level path but
// belongs to budgeting), so both prefixes select the same group.
func subNavGroups() []subNavGroup {
	return []subNavGroup{
		{
			// Reuse the existing section-title keys the admin index already renders
			// (both catalogs), so the sub-nav adds no new label strings.
			Prefixes: []string{"/admin"},
			Entries: []navEntry{
				{"admin.users.title", "/admin/users", Admin},
				{"subsidiaries.title", "/admin/subsidiaries", Admin},
				{"admin.currencies.title", "/admin/currencies", Admin},
				{"admin.rates.title", "/admin/rates", Admin},
				{"org.title", "/admin/org", Admin},
				{"admin.ops.title", "/admin/ops", Admin},
			},
		},
		{
			// p23.9 the "More" hub area: every page under it shows the same lateral
			// sub-nav (perm-filtered by navPermits). /admin is NOT a prefix here — it
			// has its own group above (a sub-hub); the More bar links out to it.
			Prefixes: []string{"/more", "/funds", "/programs", "/reconciliations", "/budgets", "/schedules", "/import", "/settings"},
			Entries: []navEntry{
				{"nav.funds", "/funds", TxnRead},
				{"nav.programs", "/programs", TxnRead},
				{"nav.reconciliations", "/reconciliations", TxnRead},
				{"nav.budgets", "/budgets", TxnRead},
				{"budget.schedules.title", "/schedules", TxnRead},
				{"nav.import", "/import", TxnWrite},
				{"nav.settings", "/settings", AnyUser},
				{"nav.admin", "/admin", Admin},
			},
		},
	}
}

// subNav resolves the current request's section bar (p23.5): it finds the group the
// path belongs to (any Prefix matches), then keeps each entry that is BOTH registered
// (no dead links) AND permitted (reusing navPermits, so the section bar and route
// enforcement share one truth). Returns nil when the section has no sub-nav — the
// template then renders no second row.
func (s *server) subNav(ctx context.Context, u *store.CurrentUser, currentPath string) []navItem {
	var group *subNavGroup
	for i := range subNavGroups() {
		g := subNavGroups()[i]
		for _, p := range g.Prefixes {
			if isCurrentNav(p, currentPath) {
				group = &g
				break
			}
		}
		if group != nil {
			break
		}
	}
	if group == nil {
		return nil
	}

	registered := s.registeredGetPaths()
	lang := langOf(ctx)
	var out []navItem
	for _, e := range group.Entries {
		if !registered[e.Href] {
			continue
		}
		if !s.navPermits(ctx, u, e.Perm) {
			continue
		}
		out = append(out, navItem{
			Label:   i18n.T(lang, e.LabelKey),
			Href:    e.Href,
			Current: isCurrentNav(e.Href, currentPath),
		})
	}
	return out
}

// isCurrentNav reports whether nav entry href is the active section for the
// request path: an exact match, or (for a non-root section like "/accounts") the
// path being under it ("/accounts/new"). The root "/" matches only itself so it
// isn't flagged current on every page.
func isCurrentNav(href, path string) bool {
	if href == "/" {
		return path == "/"
	}
	return path == href || strings.HasPrefix(path, href+"/")
}

// navPermits reports whether user u may see a nav entry guarded by perm. It reuses
// decide (the single enforcement policy, rule 8) so nav gating and route
// enforcement share one truth: an entry is shown iff the route would ALLOW. For a
// ReportGroup nav entry (the reports index) "permitted" means the user holds ANY
// report grant, or is admin -- the concrete report routes still gate per group.
func (s *server) navPermits(ctx context.Context, u *store.CurrentUser, perm Perm) bool {
	if perm.kind == permReportGroup {
		if u == nil {
			return false
		}
		if u.IsAdmin {
			return true
		}
		grants, err := s.store.ReportGrants(ctx, u.ID)
		if err != nil {
			return false // fail closed
		}
		return len(grants) > 0
	}
	// For every other perm, an entry is visible exactly when the route would allow
	// the request. grantChecker is never consulted here (non-ReportGroup perms
	// don't query grants), so the closure is a harmless never-called stub.
	return decide(perm, u, func(string) bool { return false }) == outcomeAllow
}

// registeredGetPaths returns the set of concrete GET route patterns in the
// registry, so visibleNav can drop a nav entry whose section has no route yet
// (avoiding dead links). Only exact (non-wildcard) GET patterns are nav targets;
// {$} is the exact-root anchor for "/". Built from routes() so it auto-reflects
// routes appended by later phases.
func (s *server) registeredGetPaths() map[string]bool {
	paths := make(map[string]bool)
	for _, r := range s.routes() {
		if r.Method != http.MethodGet {
			continue
		}
		p := r.Pattern
		if p == "/{$}" {
			p = "/"
		}
		paths[p] = true
	}
	return paths
}

// baseData is the model every authenticated shell page embeds as its .Shell field.
// It carries the request-scoped chrome: resolved Lang for <html lang>, Theme for
// the SSR data-theme (no flash), the localized Nav, and the app Version for the
// footer. Page handlers wrap their own data in shellPage so the template can reach
// both.
type baseData struct {
	Lang   string
	Theme  string
	Nav    []navItem
	SubNav []navItem // p23.5 second-row section nav (nil = no section bar)
	// SubNavControls names a page-specific controls partial (p23.10) the section bar
	// renders alongside the sub-nav — filters/buttons a page moves out of its body
	// into the second-level menu (e.g. "accounts"). "" = none. The shell renders it
	// by a constant-name {{template}} guarded on this string (no dynamic dispatch).
	SubNavControls string
	Version        string
	// Wide opts <main> out of the centered 60rem column so a data-dense page (the
	// transaction editor, p23.2) can use the full horizontal width. Set via
	// newWideShellPage; default false keeps the comfortable reading column.
	Wide bool
	// DateFormat is the user's date-format code ("ISO"/"US"/"EU"), stamped on <body>
	// so the global date-field module (datefield.js, p23.4) can format/parse every
	// date input per the user's setting without each page re-emitting it.
	DateFormat string
}

// shellPage bundles the shell chrome (Shell) with a page's own model (Page) so a
// single template set can render <base> + the page body. Handlers build it via
// newShellPage.
type shellPage struct {
	Shell baseData
	Page  any
}

// titledShell wraps a shellPage with the localized head title a shell-open needs.
// The `shellTitle` template func builds it so a page can pass its own title
// string (already run through {{t}}) to the shared shell-open partial.
type titledShell struct {
	Shell baseData
	Page  any
	Title string
}

// shellTitle is the `shellTitle` template func: it pairs a shellPage with a
// localized title, yielding the model shell-open renders (.Shell, .Page, .Title).
// It keeps the head title a real catalog string per page while the frame stays
// shared (one parse set).
func shellTitle(p shellPage, title string) titledShell {
	return titledShell{Shell: p.Shell, Page: p.Page, Title: title}
}

// newShellPage assembles the shell chrome for the current request and wraps the
// page's data. Theme resolution is SSR and flash-free: the theme cookie wins (set
// the instant the user toggles), else the logged-in user's stored setting, else
// the default -- all read server-side so the right data-theme is in the very first
// byte of HTML.
func (s *server) newShellPage(r *http.Request, page any) shellPage {
	ctx := r.Context()
	u := currentUser(ctx)
	return shellPage{
		Shell: baseData{
			Lang:       langOf(ctx),
			Theme:      resolveTheme(r, u),
			Nav:        s.visibleNav(ctx, u, r.URL.Path),
			SubNav:     s.subNav(ctx, u, r.URL.Path),
			Version:    s.cfg.Version,
			DateFormat: dateFormatCode(u),
		},
		Page: page,
	}
}

// newShellPageControls is newShellPage with a page-controls partial named for the
// section bar (p23.10/p23.11) — the page's filters/New button moved out of its body
// into the second-level menu. controls must match a guarded {{template}} in the
// section bar (base.tmpl).
func (s *server) newShellPageControls(r *http.Request, page any, controls string) shellPage {
	p := s.newShellPage(r, page)
	p.Shell.SubNavControls = controls
	return p
}

// newWideShellPage is newShellPage with the full-width <main> opt-out set (Wide),
// for data-dense pages that need the horizontal space (the transaction editor,
// p23.2). Everything else is identical to newShellPage.
func (s *server) newWideShellPage(r *http.Request, page any) shellPage {
	p := s.newShellPage(r, page)
	p.Shell.Wide = true
	return p
}

// resolveTheme picks the data-theme to render SERVER-SIDE (no flash): the theme
// cookie first (authoritative -- POST /theme sets it immediately), then the
// logged-in user's persisted setting, then the default. An unrecognized cookie
// value is ignored (falls through) so a tampered cookie can't inject arbitrary
// attribute text -- the template still receives one of the known values.
func resolveTheme(r *http.Request, u *store.CurrentUser) string {
	if c, err := r.Cookie(themeCookieName); err == nil && store.ValidTheme(c.Value) {
		return c.Value
	}
	if u != nil && store.ValidTheme(u.Theme) {
		return u.Theme
	}
	return defaultTheme
}

// renderShell renders a shell page (base.tmpl + a page body block) for the
// request's language, wrapping page in the shell chrome. It mirrors render but
// always executes base.tmpl and passes the shellPage model.
func (s *server) renderShell(w http.ResponseWriter, r *http.Request, status int, page any) {
	s.render(w, r, status, "base.tmpl", s.newShellPage(r, page))
}

// home is the authenticated landing (GET /{$}). p23.8: the chart of accounts is the
// landing for anyone who can read the ledger — the old empty welcome was pointless.
// It renders accounts INLINE (not a redirect) so `/` stays a real shell page (the
// theme/nav/lang chrome, and the tests that assert on the `/` body, still hold). A
// user without ledger access (a pure expense submitter) gets the minimal welcome.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	if u != nil && s.navPermits(ctx, u, TxnRead) {
		s.accountsPage(w, r)
		return
	}
	s.renderShell(w, r, http.StatusOK, nil)
}

// hubCard is one card on a hub landing (p23.9): a localized label + description
// linking to a section, shown only when the section is registered AND the user's
// perm permits it (so a hub never links somewhere the user would be 403'd).
type hubCard struct {
	LabelKey string
	DescKey  string
	Href     string
	Perm     Perm
}

// hubCardItem is a resolved card for the template (localized, no Perm/keys leaked).
type hubCardItem struct {
	Label string
	Desc  string
	Href  string
}

// moreCards is the "More" hub's contents (p23.9): the ledger dimensions/operations
// plus personal settings and the admin sub-hub, each perm-gated. Reuses the existing
// nav.* labels; descriptions are the new more.desc.* keys.
func moreCards() []hubCard {
	return []hubCard{
		{"nav.funds", "more.desc.funds", "/funds", TxnRead},
		{"nav.programs", "more.desc.programs", "/programs", TxnRead},
		{"nav.reconciliations", "more.desc.reconciliations", "/reconciliations", TxnRead},
		{"nav.budgets", "more.desc.budgets", "/budgets", TxnRead},
		{"nav.import", "more.desc.import", "/import", TxnWrite},
		{"nav.settings", "more.desc.settings", "/settings", AnyUser},
		{"nav.admin", "more.desc.admin", "/admin", Admin},
	}
}

// visibleHubCards resolves cards for the current user: registered route + permitted
// (reusing navPermits, so a card and its route agree), localized.
func (s *server) visibleHubCards(ctx context.Context, u *store.CurrentUser, cards []hubCard) []hubCardItem {
	registered := s.registeredGetPaths()
	lang := langOf(ctx)
	var out []hubCardItem
	for _, c := range cards {
		if !registered[c.Href] || !s.navPermits(ctx, u, c.Perm) {
			continue
		}
		out = append(out, hubCardItem{
			Label: i18n.T(lang, c.LabelKey),
			Desc:  i18n.T(lang, c.DescKey),
			Href:  c.Href,
		})
	}
	return out
}

// hubPageModel is the model for a card-hub landing.
type hubPageModel struct {
	TitleKey string
	IntroKey string
	Cards    []hubCardItem
}

// moreHub handles GET /more (AnyUser, p23.9): the "More" landing — a grid of
// perm-gated cards to the sections lifted out of the top nav. A pure submitter sees
// only the cards their perms allow (often just Settings).
func (s *server) moreHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(ctx)
	model := hubPageModel{
		TitleKey: "more.title",
		IntroKey: "more.intro",
		Cards:    s.visibleHubCards(ctx, u, moreCards()),
	}
	s.render(w, r, http.StatusOK, "more.tmpl", s.newShellPage(r, model))
}

// adminIndex is the GET /admin landing (Admin): the section hub linking every
// admin area (users, subsidiaries, currencies, org). p13.2 promoted it from the
// p10.2 stub to a real index once the users/currencies pages landed. Ops (p18.3)
// will add its link here later.
func (s *server) adminIndex(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "admin.tmpl", s.newShellPage(r, nil))
}

// styleguide is the -dev-only GET /styleguide component gallery. It is registered
// ONLY when cfg.Dev (routes()), so it 404s in production. Public so a designer can
// view it without logging in; it renders through the shell for real chrome. It
// hosts the p10.3 form-error demonstrator so the reusable convention is exercised
// by a real endpoint (see styleguideSubmit / the "demo-form" partial).
func (s *server) styleguide(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "styleguide.tmpl", s.newShellPage(r, demoFormModel{}))
}

// demoFormModel is the p10.3 form-error demonstrator's template model: the current
// field values (echoed back so the swap keeps what the user typed) plus the ordered
// field errors the "demo-form" partial renders and uses for autofocus placement.
// This is the SHAPE every later form model follows — its own value fields plus an
// embedded formErrors named Errors.
type demoFormModel struct {
	Name   string
	Email  string
	OK     bool // set on a valid submit so the partial shows the success message
	Errors formErrors
}

// styleguideSubmit handles POST /styleguide (-dev only, Public): the form-error
// demonstrator. It validates two fields — Name (required) and Email (required +
// must look like an email) — collecting i18n error KEYS in field order. On any
// error it re-renders ONLY the "demo-form" partial at 422 via renderFormError (the
// reusable convention: htmx swaps it in, autofocus lands on the first invalid
// field, ids stay stable). On success it re-renders the same partial at 200 with a
// success flag. This is the pattern accounts (p11) and transactions (p12) reuse.
func (s *server) styleguideSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	m := demoFormModel{
		Name:  strings.TrimSpace(r.PostFormValue("name")),
		Email: strings.TrimSpace(r.PostFormValue("email")),
	}

	// Validate in FIELD ORDER so Errors.FirstInvalid (and thus autofocus) is
	// deterministic: name before email.
	m.Errors.add("name", requiredKey(m.Name))
	m.Errors.add("email", emailKey(m.Email))

	if m.Errors.any() {
		// Invalid: 422 + the form-region partial only (the reusable convention).
		s.renderFormError(w, r, "demo-form", m)
		return
	}

	// Valid: re-render the same single-sourced partial at 200 with the success
	// message. A real form would redirect (PRG) or swap in the created row; the
	// demonstrator keeps it to the partial so success and error share one target.
	m.OK = true
	s.render(w, r, http.StatusOK, "demo-form", m)
}

// requiredKey returns the i18n error key for a missing required value, or "" when
// present. A generic, reusable field validator (rule 9: it returns a KEY).
func requiredKey(v string) string {
	if v == "" {
		return "error.required"
	}
	return ""
}

// emailKey returns the i18n error key when v is absent or not a plausible email,
// else "". Deliberately minimal (a single '@' with text on each side) — email
// validation is a UX hint, not an RFC parser; the real check is delivery. Returns a
// KEY (rule 9).
func emailKey(v string) string {
	if v == "" {
		return "error.required"
	}
	at := strings.IndexByte(v, '@')
	if at <= 0 || at >= len(v)-1 || strings.ContainsAny(v, " \t") {
		return "error.email"
	}
	return ""
}

// Theme is persisted only through POST /settings now (settingsUpdate). p23.1
// removed the header theme-control form and its POST /theme handler; the theme
// cookie is written by settingsUpdate, and resolveTheme reads it for SSR.

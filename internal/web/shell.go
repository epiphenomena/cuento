package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cuento/internal/i18n"
	"cuento/internal/store"
)

// p10.2: the authenticated shell. base.tmpl is the layout every authenticated
// page extends -- semantic landmarks, <html lang> from the resolved locale,
// data-theme applied SERVER-SIDE from the theme cookie (no flash), a flash region,
// and a perm-gated, data-driven nav. This file holds the shell's Go side: the nav
// table, the theme control, and the /settings + /styleguide handlers.

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

// navSections is the ordered top-level nav (Appendix F: accounts, funds, programs,
// reconciliations, reports, settings, admin). Home and Settings + the admin gate
// exist now; the accounts/funds/programs/reconciliations/reports entries are
// listed with their eventual perm but are filtered out until their route is
// registered (visibleNav drops any entry whose Href has no route yet), so p11-p18
// need only register the route -- no nav edit. This keeps the nav honest (no dead
// links) while being trivially appendable.
func navSections() []navEntry {
	return []navEntry{
		{"nav.home", "/", AnyUser},
		{"nav.accounts", "/accounts", TxnRead},
		{"nav.funds", "/funds", TxnRead},
		{"nav.programs", "/programs", TxnRead},
		{"nav.reconciliations", "/reconciliations", TxnRead},
		{"nav.reports", "/reports", ReportGroup("")},
		{"nav.settings", "/settings", AnyUser},
		{"nav.admin", "/admin", Admin},
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
	Lang    string
	Theme   string
	Nav     []navItem
	Version string
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
			Lang:    langOf(ctx),
			Theme:   resolveTheme(r, u),
			Nav:     s.visibleNav(ctx, u, r.URL.Path),
			Version: s.cfg.Version,
		},
		Page: page,
	}
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

// home is the authenticated landing (GET /{$}), rendered through the shell so the
// nav, theme, and locale chrome are exercised by a real route. The landing body is
// base.tmpl's own minimal welcome; the real dashboard is a backlog non-goal.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	s.renderShell(w, r, http.StatusOK, nil)
}

// adminStub is the p10.2 GET /admin landing (Admin): a minimal, Admin-only shell
// page so the admin nav section has a real, permitted, no-dead-link target and the
// perm gate is provable now. The real admin pages (users, subsidiaries, currencies,
// ops) land in p11.3/p13.2/p14.2/p18.3.
func (s *server) adminStub(w http.ResponseWriter, r *http.Request) {
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

// setTheme handles POST /theme (AnyUser): it validates the requested theme, sets
// the theme cookie (so the very next request renders it SSR with no flash), and --
// for a logged-in user -- persists it as the user's setting via the store (audited
// under one change). It then 303-redirects back so a browser <form> post lands on
// a fresh GET (Post/Redirect/Get); the Referer is used when safe, else "/".
func (s *server) setTheme(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	theme := r.PostFormValue("theme")
	if !store.ValidTheme(theme) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// Cookie first: it is the SSR source resolveTheme reads, so the preference
	// applies on the next paint whether or not the user is logged in. Path "/" so
	// every page sees it. HttpOnly: no JS ever reads it (the theme is resolved
	// server-side), so keep it off-limits to script to shrink the surface. Secure
	// mirrors the session-cookie posture (off only in -dev, for plain-HTTP dev).
	http.SetCookie(w, &http.Cookie{
		Name:     themeCookieName,
		Value:    theme,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.Dev,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})

	// Persist as the user's durable setting when logged in, so the choice follows
	// them across devices/sessions. A store failure here is a server fault (the
	// cookie already applied for this browser); surface it as 500.
	if u := currentUser(r.Context()); u != nil {
		ctx := store.WithActor(r.Context(), store.Actor{ID: u.ID})
		if err := s.store.SetUserTheme(ctx, u.ID, theme); err != nil {
			if errors.Is(err, store.ErrInvalidTheme) {
				http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
				return
			}
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, safeRedirectTarget(r), http.StatusSeeOther)
}

// safeRedirectTarget returns a same-origin path to redirect to after POST /theme:
// the Referer when it is a local path, else "/". It never trusts an absolute or
// cross-origin Referer (open-redirect defense), returning only a leading-slash
// path that is not "//" (protocol-relative).
func safeRedirectTarget(r *http.Request) string {
	if p, ok := sameOriginPath(r.Referer()); ok {
		return p
	}
	return "/"
}

// sameOriginPath extracts a safe local redirect path from a Referer. It returns
// ok=false unless the reference resolves to a plain local path: a single leading
// "/" (not "//", which is protocol-relative), no host, no scheme. This blocks
// open-redirects -- a cross-origin or absolute Referer yields "/" at the call
// site. The returned path carries any query so the user lands where they were.
func sameOriginPath(ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	if u.Scheme != "" || u.Host != "" {
		return "", false
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "", false
	}
	p := u.Path
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p, true
}

package web

import (
	"context"
	"net/http"

	"cuento/internal/store"
)

// The route registry is cuento's entire authorization surface (AGENTS rule 8).
// EVERY route is declared once here with an explicit Perm; Mount is the ONLY
// place a route is attached to the mux; and the permission-matrix test is
// generated FROM routes() so a route added here is enforced-and-tested
// automatically. A route mounted outside this registry is a security bug -- so
// health, static, login and logout are declared here too, not bolted onto the
// mux directly.
//
// Enforcement policy (encoded once in decide(), asserted by TestDecidePolicy and
// TestPermissionMatrix). is_admin implies everything (D10):
//
//   - Public       everyone, including anonymous.
//   - AnyUser      any logged-in user; anonymous -> redirect to /login.
//   - TxnRead      txn_perm in {read,write} OR admin; else 403 (anon -> login).
//   - TxnWrite     txn_perm == write   OR admin; else 403 (anon -> login).
//   - ReportGroup  a read grant for the named group OR admin; else 403 (anon -> login).
//   - Admin        is_admin; else 403 (anon -> login).
//
// The anon->login case uses a 302 redirect to /login; handlers' own redirects use
// 303. That split lets the matrix distinguish "authorized user wrongly bounced to
// login" (302) from a legitimate handler redirect to /login (303, e.g. logout).

// permKind is the discriminator for Perm. ReportGroup carries a group name; the
// rest are simple markers.
type permKind int

const (
	permPublic permKind = iota
	permAnyUser
	permTxnRead
	permTxnWrite
	permReportGroup
	permAdmin
)

// Perm is a route's required permission. It is a small value type (not an
// interface) so the registry is a plain declarative table and the matrix can
// switch on it exhaustively. Only ReportGroup uses the group field.
type Perm struct {
	kind  permKind
	group string
}

// The simple permission classes. ReportGroup is a constructor because it carries
// a group name (Appendix B: a report route's Perm is its group).
var (
	Public   = Perm{kind: permPublic}
	AnyUser  = Perm{kind: permAnyUser}
	TxnRead  = Perm{kind: permTxnRead}
	TxnWrite = Perm{kind: permTxnWrite}
	Admin    = Perm{kind: permAdmin}
)

// ReportGroup returns the Perm requiring a read grant on the named report group
// (D10). Report routes (p15) declare their group here; a user reaches the route
// iff they hold that grant (or are admin).
func ReportGroup(group string) Perm { return Perm{kind: permReportGroup, group: group} }

// String renders a Perm for test failure messages.
func (p Perm) String() string {
	switch p.kind {
	case permPublic:
		return "Public"
	case permAnyUser:
		return "AnyUser"
	case permTxnRead:
		return "TxnRead"
	case permTxnWrite:
		return "TxnWrite"
	case permReportGroup:
		return "ReportGroup(" + p.group + ")"
	case permAdmin:
		return "Admin"
	default:
		return "Perm(?)"
	}
}

// Route is one registry entry: an HTTP method + pattern, the permission required
// to reach it, and the handler. Handler is http.Handler (not http.HandlerFunc) so
// an http.Handler like the static FileServer mounts without wrapping.
type Route struct {
	Method  string
	Pattern string
	Perm    Perm
	Handler http.Handler
}

// codeReportGroups is the code-declared report-group set synced to report_groups
// at startup (D10). Report routes and their real groups arrive in phase 15; until
// then a single placeholder makes the sync mechanism real and gives the
// permission tests a concrete ReportGroup to grant. Phase 15 replaces this with
// the real groups (each report route's Perm references a name here).
func codeReportGroups() []string { return []string{placeholderReportGroup} }

// placeholderReportGroup is the lone code-declared group until p15 defines the
// real set. Kept as a named constant so tests reference it symbolically.
const placeholderReportGroup = "_placeholder"

// SyncReportGroups upserts the code-declared report groups (codeReportGroups)
// into the report_groups reference table, idempotently (D10). It is the startup
// wiring: serve() calls it after opening the db so a fresh or existing boot has
// exactly the groups the routes reference. The web package owns the canonical set
// because report routes' Perms reference these names; the store performs the
// upsert (reference data, outside the write funnel -- like currencies, rule 2).
func SyncReportGroups(ctx context.Context, st *store.Store) error {
	return st.SyncReportGroups(ctx, codeReportGroups())
}

// routes returns the complete route registry. Later phases APPEND their routes
// here (rule 8); the matrix test picks them up with no edit. Today: the four
// public infra routes, logout (AnyUser), and a minimal authenticated landing
// (GET /{$}, AnyUser) so the enforcement path is exercised by a real i18n'd
// route. GET /{$} matches ONLY exact "/" -- a bare "/" would be a catch-all,
// swallowing every unmatched GET and defeating the registry-completeness test.
func (s *server) routes() []Route {
	routes := []Route{
		{http.MethodGet, "/healthz", Public, http.HandlerFunc(healthz(s.cfg.Version))},
		{http.MethodGet, "/static/", Public, s.staticHandler()},
		{http.MethodGet, "/login", Public, http.HandlerFunc(s.loginPage)},
		{http.MethodPost, "/login", Public, http.HandlerFunc(s.loginSubmit)},
		{http.MethodPost, "/logout", AnyUser, http.HandlerFunc(s.logout)},
		{http.MethodGet, "/{$}", AnyUser, http.HandlerFunc(s.home)},
		// p10.2 shell: the theme control (persists cookie + user setting) and the
		// Settings stub (a real, permitted, localized nav target; the full page is
		// p13.1). Both AnyUser per Appendix B.
		{http.MethodPost, "/theme", AnyUser, http.HandlerFunc(s.setTheme)},
		{http.MethodGet, "/settings", AnyUser, http.HandlerFunc(s.settingsStub)},
		// A minimal Admin landing so the perm-gated nav has a real, Admin-only
		// target NOW (the shell must prove "Admin sees the admin entry, a non-admin
		// does not" -- DoD). The real /admin pages (users, subsidiaries, ops) land
		// in p11.3/p13.2/p18.3; this stub is the section index they hang off. See
		// DECISIONS p10.2.
		{http.MethodGet, "/admin", Admin, http.HandlerFunc(s.adminStub)},
		// p11.1 chart of accounts (Appendix B/F). GET is TxnRead (the tree table +
		// balances + filters + the inline form fetches); the POST mutations are
		// TxnWrite. The nav.accounts entry (shell.go) lights up now that GET
		// /accounts is registered. The permission-matrix test picks these up
		// automatically (rule 8).
		{http.MethodGet, "/accounts", TxnRead, http.HandlerFunc(s.accountsPage)},
		// p12.1 account register (Appendix B/F). TxnRead: the register table + its
		// filters + keyset htmx paging (the sentinel's next-page fetch is the SAME
		// route with a cursor param, rendering a rows fragment). The ".../new" and
		// ".../merge" literals are more specific than ".../{id}/register", so the Go
		// 1.22+ mux routes them precisely. The permission-matrix test picks it up
		// automatically (rule 8); /accounts links each row here.
		{http.MethodGet, "/accounts/{id}/register", TxnRead, http.HandlerFunc(s.registerPage)},
		{http.MethodGet, "/accounts/new", TxnWrite, http.HandlerFunc(s.accountNewForm)},
		{http.MethodGet, "/accounts/{id}/edit", TxnWrite, http.HandlerFunc(s.accountEditForm)},
		// p11.2 merge UI (TxnWrite). GET renders the merge form (source/destination
		// leaf pickers) into #account-form; POST is the two-step flow (preview without
		// confirm, execute with confirm=1). The literal "/accounts/merge" is more
		// specific than "/accounts/{id}", so the Go 1.22+ mux routes it precisely (no
		// conflict). The permission-matrix test picks both up automatically (rule 8).
		{http.MethodGet, "/accounts/merge", TxnWrite, http.HandlerFunc(s.mergeFormPartial)},
		{http.MethodPost, "/accounts/merge", TxnWrite, http.HandlerFunc(s.merge)},
		{http.MethodPost, "/accounts", TxnWrite, http.HandlerFunc(s.accountCreate)},
		{http.MethodPost, "/accounts/{id}", TxnWrite, http.HandlerFunc(s.accountUpdate)},
		{http.MethodPost, "/accounts/{id}/deactivate", TxnWrite, http.HandlerFunc(s.accountDeactivate)},
		// p11.3 subsidiaries admin (Appendix B/F: /admin/** = Admin). The tree list
		// (GET), the inline create/edit form fetches (GET .../new, .../{id}/edit), and
		// the create/update/deactivate mutations are ALL Admin -- subsidiaries are org
		// structure, not bookkeeping. The literal ".../new" and ".../merge"-style
		// segments are more specific than ".../{id}", so the Go 1.22+ mux routes them
		// precisely. The permission-matrix test picks these up automatically (rule 8);
		// the /admin landing (admin.tmpl) links this page.
		{http.MethodGet, "/admin/subsidiaries", Admin, http.HandlerFunc(s.subsidiariesPage)},
		{http.MethodGet, "/admin/subsidiaries/new", Admin, http.HandlerFunc(s.subsidiaryNewForm)},
		{http.MethodGet, "/admin/subsidiaries/{id}/edit", Admin, http.HandlerFunc(s.subsidiaryEditForm)},
		{http.MethodPost, "/admin/subsidiaries", Admin, http.HandlerFunc(s.subsidiaryCreate)},
		{http.MethodPost, "/admin/subsidiaries/{id}", Admin, http.HandlerFunc(s.subsidiaryUpdate)},
		{http.MethodPost, "/admin/subsidiaries/{id}/deactivate", Admin, http.HandlerFunc(s.subsidiaryDeactivate)},
		// p11.4 org settings & languages (Appendix B/F: /admin/** = Admin). GET
		// renders the settings form; POST stores the org name + enabled languages
		// (a CSV of the languages account NAMES may be entered in, D14 -- driving the
		// account form's per-language name inputs). Both Admin -- org config is not
		// bookkeeping. The permission-matrix test picks these up automatically (rule
		// 8); the /admin landing (admin.tmpl) links this page. Report base currency is
		// NOT a setting here -- it follows the scoped subsidiary (D18).
		{http.MethodGet, "/admin/org", Admin, http.HandlerFunc(s.orgPage)},
		{http.MethodPost, "/admin/org", Admin, http.HandlerFunc(s.orgUpdate)},
		// p11.5 programs management (Appendix B/F). Programs are a DIMENSION and their
		// structure is BOOKKEEPING (D24), so GET /programs is TxnRead (the tree list +
		// per-program activity totals + the inline form fetches) and the create/edit/
		// move/deactivate mutations are TxnWrite -- unlike subsidiaries (org structure,
		// all Admin). The literal ".../new" segment is more specific than ".../{id}",
		// so the Go 1.22+ mux routes them precisely. The nav.programs entry (shell.go)
		// lights up now that GET /programs is registered. The permission-matrix test
		// picks these up automatically (rule 8).
		{http.MethodGet, "/programs", TxnRead, http.HandlerFunc(s.programsPage)},
		{http.MethodGet, "/programs/new", TxnWrite, http.HandlerFunc(s.programNewForm)},
		{http.MethodGet, "/programs/{id}/edit", TxnWrite, http.HandlerFunc(s.programEditForm)},
		{http.MethodPost, "/programs", TxnWrite, http.HandlerFunc(s.programCreate)},
		{http.MethodPost, "/programs/{id}", TxnWrite, http.HandlerFunc(s.programUpdate)},
		{http.MethodPost, "/programs/{id}/deactivate", TxnWrite, http.HandlerFunc(s.programDeactivate)},
		// p12.2 transaction editor (Appendix B/F). The daily-use data-entry grid. All
		// four routes are TxnWrite (the editor both READS to prefill and WRITES on
		// save; per Appendix B the POST /transactions... family is TxnWrite, and the
		// GET editor forms are write-gated because they exist only to author an entry).
		// GET /transactions/new renders a blank grid; GET /transactions/{id}/edit
		// prefills an existing txn; POST /transactions creates; POST /transactions/{id}
		// updates (round-tripping split ids, trap 1). The literal ".../new" is more
		// specific than ".../{id}/edit", so the Go 1.22+ mux routes them precisely. The
		// permission-matrix test picks these up automatically (rule 8); the register
		// (p12.1) links "new/edit txn" here.
		{http.MethodGet, "/transactions/new", TxnWrite, http.HandlerFunc(s.txnNewForm)},
		{http.MethodGet, "/transactions/{id}/edit", TxnWrite, http.HandlerFunc(s.txnEditForm)},
		{http.MethodPost, "/transactions", TxnWrite, http.HandlerFunc(s.txnCreate)},
		{http.MethodPost, "/transactions/{id}", TxnWrite, http.HandlerFunc(s.txnUpdate)},
		// p12.4 edit/void/duplicate + history. History is TxnRead (a viewer may audit
		// the change trail); void (delete = soft-delete with confirm) and duplicate
		// (open the editor prefilled as a NEW entry) are TxnWrite. The literal ".../
		// history", ".../void", ".../duplicate" segments are more specific than the
		// ".../{id}" POST, so the Go 1.22+ mux routes them precisely. GET /void is the
		// confirm-review; POST /void executes. The permission-matrix test picks these up
		// automatically (rule 8); the register row links edit/void/duplicate/history.
		{http.MethodGet, "/transactions/{id}/history", TxnRead, http.HandlerFunc(s.txnHistory)},
		{http.MethodGet, "/transactions/{id}/void", TxnWrite, http.HandlerFunc(s.voidReview)},
		{http.MethodPost, "/transactions/{id}/void", TxnWrite, http.HandlerFunc(s.void)},
		{http.MethodGet, "/transactions/{id}/duplicate", TxnWrite, http.HandlerFunc(s.txnDuplicate)},
		// p12.3 payee autocomplete + autofill (Appendix B/F). Both feed the transaction
		// ENTRY flow, so both are TxnWrite (matching the editor GET forms -- they exist
		// only to author an entry). GET /payees/suggest returns a ranked suggestion
		// fragment; GET /payees/{id}/template returns the split-rows editor partial the
		// grid swaps in on a payee pick. The literal "/payees/suggest" is more specific
		// than "/payees/{id}/template", so the Go 1.22+ mux routes them precisely. The
		// permission-matrix test picks these up automatically (rule 8).
		{http.MethodGet, "/payees/suggest", TxnWrite, http.HandlerFunc(s.payeeSuggest)},
		{http.MethodGet, "/payees/{id}/template", TxnWrite, http.HandlerFunc(s.payeeTemplate)},
	}
	// The -dev-only styleguide (Appendix F): a component gallery for visual review.
	// Registered ONLY in -dev so it 404s in production (it is not in the registry
	// there, and the matrix/reachability tests never see it). Public so a designer
	// can view it without a login.
	if s.cfg.Dev {
		routes = append(routes, Route{http.MethodGet, "/styleguide", Public, http.HandlerFunc(s.styleguide)})
		// p10.3: the form-error demonstrator's POST target. -dev only (like the GET),
		// so it never exists in production and never appears in the permission matrix;
		// it lets the reusable 422/partial/autofocus/i18n convention be tested through
		// the real registry + middleware now.
		routes = append(routes, Route{http.MethodPost, "/styleguide", Public, http.HandlerFunc(s.styleguideSubmit)})
	}
	return routes
}

// Mount is the ONLY place routes attach to a mux (rule 8). It iterates the
// registry, wrapping each handler in the permission-enforcement middleware keyed
// to that route's Perm, and registers it under "METHOD /pattern". The security
// chain (secureHeaders -> crossOrigin -> session -> auth -> lang) wraps the whole
// mux in chain(), so by the time enforce() runs, auth() has already resolved the
// current user into the request context. No route exists outside this function.
func (s *server) Mount() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.Handle(r.Method+" "+r.Pattern, s.enforce(r.Perm, r.Handler))
	}
	return s.chain(mux)
}

// outcome is what the enforcement policy decides for a (Perm, user) pair. It is
// the single expectation vocabulary shared by decide(), the enforcement
// middleware, and both matrix tests -- so the policy is expressed exactly once.
type outcome int

const (
	outcomeForbid        outcome = iota // 403: authenticated but not permitted
	outcomeRedirectLogin                // 302 -> /login: anonymous on a protected route
	outcomeAllow                        // pass through to the handler
)

// String renders an outcome for test failure messages.
func (o outcome) String() string {
	switch o {
	case outcomeForbid:
		return "Forbid(403)"
	case outcomeRedirectLogin:
		return "RedirectLogin(302 /login)"
	case outcomeAllow:
		return "Allow"
	default:
		return "outcome(?)"
	}
}

// decide is the pure enforcement decision (rule 8's policy, D10). It takes the
// route's Perm, the resolved user (nil == anonymous), and a hasGrant closure
// (queried lazily ONLY for ReportGroup so the hot path never loads grants). It is
// pure and total so TestDecidePolicy can prove every Perm x persona -- including
// ReportGroup, whose HTTP coverage waits for p15's report routes.
func decide(p Perm, u *store.CurrentUser, hasGrant func(group string) bool) outcome {
	// Public is open to everyone, anon included -- decided before any identity check.
	if p.kind == permPublic {
		return outcomeAllow
	}
	// Every non-public route requires a logged-in user; anon is redirected to login
	// (never 403, so the browser lands on the sign-in page rather than a dead end).
	if u == nil {
		return outcomeRedirectLogin
	}
	// is_admin implies everything (D10): short-circuit before any perm-specific
	// check, and before any grant query.
	if u.IsAdmin {
		return outcomeAllow
	}

	switch p.kind {
	case permAnyUser:
		return outcomeAllow // any logged-in user
	case permTxnRead:
		if u.TxnPerm == "read" || u.TxnPerm == "write" {
			return outcomeAllow
		}
	case permTxnWrite:
		if u.TxnPerm == "write" {
			return outcomeAllow
		}
	case permReportGroup:
		if hasGrant(p.group) {
			return outcomeAllow
		}
	case permAdmin:
		// Non-admin already excluded above (the u.IsAdmin short-circuit).
	}
	return outcomeForbid
}

// enforce wraps h with the permission check for perm. auth() (upstream in the
// chain) has already put the current user in the context; enforce reads it,
// resolves the grant closure lazily (only a ReportGroup route pays the query),
// runs decide, and either serves h, redirects anon to /login (302), or answers
// 403 -- never running the handler on a denial.
func (s *server) enforce(perm Perm, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r.Context())

		switch decide(perm, u, s.grantChecker(r.Context(), u, perm)) {
		case outcomeAllow:
			h.ServeHTTP(w, r)
		case outcomeRedirectLogin:
			// 302 (distinct from handlers' 303) so the matrix can tell an
			// enforcement bounce from a legitimate handler redirect to /login.
			http.Redirect(w, r, "/login", http.StatusFound)
		default: // outcomeForbid
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		}
	})
}

// grantChecker returns the hasGrant closure decide needs. For non-ReportGroup
// perms (and anonymous/admin, already decided upstream) it never queries -- it
// returns a closure that is simply never called. For a ReportGroup route with a
// concrete user it loads the user's grants ONCE and checks membership in memory.
// A grant-read error fails closed (no grant), so a transient DB fault denies
// rather than leaks access.
func (s *server) grantChecker(ctx context.Context, u *store.CurrentUser, perm Perm) func(string) bool {
	if perm.kind != permReportGroup || u == nil {
		return func(string) bool { return false }
	}
	grants, err := s.store.ReportGrants(ctx, u.ID)
	if err != nil {
		return func(string) bool { return false }
	}
	return func(group string) bool {
		for _, g := range grants {
			if g == group {
				return true
			}
		}
		return false
	}
}

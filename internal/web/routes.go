package web

import (
	"context"
	"net/http"

	"cuento/internal/reports"
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
//   - ExpenseSubmit can_submit_expenses OR admin; else 403 (anon -> login). A
//     STANDALONE capability independent of txn_perm (p20.1): a pure submitter
//     (can_submit_expenses, txn_perm=none) passes this but fails Txn*/ReportGroup.
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
	permExpenseSubmit
	permAdmin
)

// Perm is a route's required permission. It is a small value type (not an
// interface) so the registry is a plain declarative table and the matrix can
// switch on it exhaustively. Only ReportGroup uses the group field; ReportGroup on
// a PROGRAM-DIMENSIONED report additionally sets programDim, which decides whether a
// PURELY program-scoped grant reaches the route (p27.4): a program-scoped grant
// reaches a program-dimensioned report (its rows are then filtered to the granted
// subtree) but NOT a non-program report (which cannot be filtered by program) --
// the DATA-scoping vs route-reachability distinction (D10, budget-redesign DECISIONS).
type Perm struct {
	kind       permKind
	group      string
	programDim bool
}

// The simple permission classes. ReportGroup is a constructor because it carries
// a group name (Appendix B: a report route's Perm is its group).
var (
	Public   = Perm{kind: permPublic}
	AnyUser  = Perm{kind: permAnyUser}
	TxnRead  = Perm{kind: permTxnRead}
	TxnWrite = Perm{kind: permTxnWrite}
	Admin    = Perm{kind: permAdmin}
	// ExpenseSubmit is a STANDALONE capability (p20.1, Phase 20): the right to
	// submit expense reports, INDEPENDENT of txn_perm and report grants. A pure
	// submitter (can_submit_expenses=true, txn_perm=none, no grants) passes
	// ExpenseSubmit but fails TxnRead/TxnWrite/ReportGroup -- decoupling submission
	// from book-editing. p20.2's submitter routes will declare this Perm.
	ExpenseSubmit = Perm{kind: permExpenseSubmit}
)

// ReportGroup returns the Perm requiring a read grant on the named report group
// (D10). Report routes (p15) declare their group here; a user reaches the route
// iff they hold that grant (or are admin). The report route mount (routes()) uses
// ReportGroupFor so a program-dimensioned report also carries the programDim bit;
// this bare constructor (programDim=false) is retained for the reportsIndex grant
// probe and TestDecidePolicy, where the group name alone is what a check keys on.
func ReportGroup(group string) Perm { return Perm{kind: permReportGroup, group: group} }

// ReportGroupFor returns the ReportGroup Perm for a concrete report, carrying its
// program-dimensioned flag (p27.4). A program-dimensioned report is one whose rows
// carry a program dimension (budget variance, program statement, fund activity,
// activities-by-restriction, functional expenses, income statement, form 990,
// cash-flow projection) -- a purely program-scoped grant reaches it (filtered to the
// subtree) but not a non-program report. Marked EXPLICITLY on the Report (not
// inferred from ParamsSpec.Program), because the program-dimensioned set is broader
// than the reports that offer a program SELECTOR.
func ReportGroupFor(rep reports.Report) Perm {
	return Perm{kind: permReportGroup, group: rep.Group, programDim: rep.ProgramDimensioned}
}

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
	case permExpenseSubmit:
		return "ExpenseSubmit"
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
// at startup (D10). p15.1 replaced the lone "_placeholder" with the REAL set,
// owned by the reports package (reports.Groups()) since that is where reports
// declare their Group. Every mounted report route's Perm is ReportGroup(one of
// these); p13.2's admin grant UI now offers exactly this set. A group may be
// declared before any report references it (SyncReportGroups syncs all names).
func codeReportGroups() []string { return reports.Groups() }

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
		// p13.1 /settings is the personal-preferences page (GET form, POST save):
		// AnyUser per Appendix B -- a user edits THEIR OWN settings (admin edit-others
		// is p13.2). Theme lives here too (its own <select>); p23.1 removed the
		// redundant header theme-control form and its POST /theme route.
		// p23.9 the "More" hub: an AnyUser card grid to the sections lifted out of the
		// top nav (funds/programs/reconciliations/budgets/import/settings/admin), each
		// card perm-gated. The perm-matrix test picks it up automatically.
		{http.MethodGet, "/more", AnyUser, http.HandlerFunc(s.moreHub)},
		{http.MethodGet, "/settings", AnyUser, http.HandlerFunc(s.settingsPage)},
		{http.MethodPost, "/settings", AnyUser, http.HandlerFunc(s.settingsUpdate)},
		// A minimal Admin landing so the perm-gated nav has a real, Admin-only
		// target NOW (the shell must prove "Admin sees the admin entry, a non-admin
		// does not" -- DoD). The real /admin pages (users, subsidiaries, ops) land
		// in p11.3/p13.2/p18.3; this stub is the section index they hang off. See
		// DECISIONS p10.2.
		{http.MethodGet, "/admin", Admin, http.HandlerFunc(s.adminIndex)},
		// p13.2 admin: users, per-user permissions, and currencies (Appendix B/F:
		// /admin/** = Admin). The users list + inline create form, disable/reset
		// actions, the per-user perm detail (txn_perm select + report-group grant
		// checkboxes -- each a VERSIONED change naming the acting admin), and the
		// currencies list + add + enable/disable toggle. The literal ".../new" is more
		// specific than ".../{id}", so the Go 1.22+ mux routes them precisely; likewise
		// the ".../disable", ".../reset-password", ".../txn-perm", ".../grants" segments
		// vs the ".../{id}" GET. Org settings (/admin/org) is already built (p11.4); the
		// admin index links it. The permission-matrix test picks these up automatically
		// (rule 8).
		{http.MethodGet, "/admin/users", Admin, http.HandlerFunc(s.usersPage)},
		{http.MethodGet, "/admin/users/new", Admin, http.HandlerFunc(s.userNewForm)},
		{http.MethodPost, "/admin/users", Admin, http.HandlerFunc(s.userCreate)},
		{http.MethodGet, "/admin/users/{id}", Admin, http.HandlerFunc(s.userDetailPage)},
		{http.MethodPost, "/admin/users/{id}/disable", Admin, http.HandlerFunc(s.userDisable)},
		{http.MethodPost, "/admin/users/{id}/reset-password", Admin, http.HandlerFunc(s.userResetPassword)},
		{http.MethodPost, "/admin/users/{id}/txn-perm", Admin, http.HandlerFunc(s.userSetTxnPerm)},
		{http.MethodPost, "/admin/users/{id}/grants", Admin, http.HandlerFunc(s.userSetGrants)},
		// p20.2: the admin toggle for the p20.1 can_submit_expenses capability (the
		// standalone ExpenseSubmit right). p20.1 deferred this UI ("Admin manages it,
		// p13.2 UI later"); it lands here so an admin can grant a user submit access (and
		// so the e2e can seed a pure submitter via the real admin flow). Admin-gated (it
		// does NOT breach the submitter boundary), a VERSIONED change naming the acting
		// admin (SetUserCanSubmitExpenses). The literal ".../can-submit" is distinct from
		// the other ".../{id}/..." action segments; the matrix picks it up (rule 8).
		{http.MethodPost, "/admin/users/{id}/can-submit", Admin, http.HandlerFunc(s.userSetCanSubmit)},
		{http.MethodGet, "/admin/currencies", Admin, http.HandlerFunc(s.currenciesPage)},
		{http.MethodPost, "/admin/currencies", Admin, http.HandlerFunc(s.currencyAdd)},
		{http.MethodPost, "/admin/currencies/{code}/toggle", Admin, http.HandlerFunc(s.currencyToggle)},
		// p14.2 admin: manual/backfill exchange-rate CSV upload (Appendix B/F:
		// /admin/** = Admin). GET is the upload form; POST parses the multipart
		// file, validates every row, and PutRates the batch as one change (the
		// automated counterpart is `cuento ratesync`). The permission-matrix test
		// picks these up automatically (rule 8); the /admin landing links the page.
		{http.MethodGet, "/admin/rates", Admin, http.HandlerFunc(s.ratesPage)},
		{http.MethodPost, "/admin/rates", Admin, http.HandlerFunc(s.ratesImport)},
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
		{http.MethodPost, "/accounts/{id}/activate", TxnWrite, http.HandlerFunc(s.accountActivate)},
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
		// p18.3 ops page (Appendix B/F: /admin/** = Admin). GET renders build info +
		// the integrity check (ledger.Check, the SAME suite `cuento check` runs, Z1-Z19
		// grouped by severity). POST /admin/ops/backup produces a VACUUM INTO snapshot,
		// streams it as an octet-stream attachment, and AUDITS the action (an ops.backup
		// change naming the admin). The backup is a POST -- it mutates the audit trail, so
		// it must sit behind the cross-origin guard (rule 13), which the middleware applies
		// to mutating (non-GET) routes. The permission-matrix test picks these up
		// automatically (rule 8); the /admin landing (admin.tmpl) links this page.
		{http.MethodGet, "/admin/ops", Admin, http.HandlerFunc(s.opsPage)},
		{http.MethodPost, "/admin/ops/backup", Admin, http.HandlerFunc(s.opsBackup)},
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
		// p26.18 per-split description autofill (step 4a of the payee->description
		// migration). Two TxnWrite routes feeding the ENTRY grids (matching the editor
		// GET forms -- they exist only to author an entry): GET /descriptions/suggest?q=
		// &sub= renders the ranked distinct-description listbox fragment; GET
		// /descriptions/prefill?q=<exact>&sub= renders one matched split's fields as
		// data-* for per-ROW prefill (replacing the whole-grid payee template). The 4b UI
		// step wires the grids to these. The permission-matrix test picks them up
		// automatically (rule 8); both answer 2xx on a blank/no-match query.
		{http.MethodGet, "/descriptions/suggest", TxnWrite, http.HandlerFunc(s.descriptionsSuggest)},
		{http.MethodGet, "/descriptions/prefill", TxnWrite, http.HandlerFunc(s.descriptionsPrefill)},
		// p12.5 funds workspace (Appendix B/F). Fund GRANTS are BOOKKEEPING data
		// (D20, managed like programs), so GET (list + statement) is TxnRead and the
		// create/edit/close/reopen mutations are TxnWrite -- subsidiaries/users stay
		// Admin (unchanged). The list (/funds) shows per-currency balances + funder +
		// scope + a Z18 negative badge, with an active/closed toggle; /funds/{id} is
		// the fund statement (its splits across all accounts, opening/closing). The
		// literal ".../new" segment is more specific than ".../{id}", so the Go 1.22+
		// mux routes them precisely; close/reopen literals likewise. The nav.funds
		// entry (shell.go) lights up now that GET /funds is registered. The
		// permission-matrix test picks these up automatically (rule 8).
		{http.MethodGet, "/funds", TxnRead, http.HandlerFunc(s.fundsPage)},
		{http.MethodGet, "/funds/new", TxnWrite, http.HandlerFunc(s.fundNewForm)},
		{http.MethodGet, "/funds/{id}/edit", TxnWrite, http.HandlerFunc(s.fundEditForm)},
		{http.MethodGet, "/funds/{id}", TxnRead, http.HandlerFunc(s.fundStatement)},
		{http.MethodPost, "/funds", TxnWrite, http.HandlerFunc(s.fundCreate)},
		{http.MethodPost, "/funds/{id}", TxnWrite, http.HandlerFunc(s.fundUpdate)},
		{http.MethodPost, "/funds/{id}/close", TxnWrite, http.HandlerFunc(s.fundClose)},
		{http.MethodPost, "/funds/{id}/reopen", TxnWrite, http.HandlerFunc(s.fundReopen)},

		// p16.3 reconciliation workspace (D13). The LIST + WORKSPACE VIEW are TxnRead
		// (viewing a bank reconciliation is a read); the start/toggle/finalize/reopen
		// ACTIONS are TxnWrite (they clear splits / finalize the statement chain). The
		// exact "/reconciliations" literal + the "/reconciliations/{id}" wildcard don't
		// collide (Go 1.22+ mux); registering GET /reconciliations lights up the
		// nav.reconciliations entry (shell.go). The permission-matrix test picks all six
		// up automatically (rule 8).
		{http.MethodGet, "/reconciliations", TxnRead, http.HandlerFunc(s.reconList)},
		{http.MethodPost, "/reconciliations", TxnWrite, http.HandlerFunc(s.reconStart)},
		{http.MethodGet, "/reconciliations/{id}", TxnRead, http.HandlerFunc(s.reconWorkspace)},
		{http.MethodPost, "/reconciliations/{id}/splits/{sid}/toggle", TxnWrite, http.HandlerFunc(s.reconToggle)},
		{http.MethodPost, "/reconciliations/{id}/finalize", TxnWrite, http.HandlerFunc(s.reconFinalize)},
		{http.MethodPost, "/reconciliations/{id}/reopen", TxnWrite, http.HandlerFunc(s.reconReopen)},
		// p26.57 edit an OPEN recon's statement date + ending balance; p26.58 discard
		// (soft-abandon) an OPEN recon. Both TxnWrite (they mutate the statement chain /
		// release cleared splits). The permission-matrix test picks them up (rule 8).
		{http.MethodPost, "/reconciliations/{id}/edit", TxnWrite, http.HandlerFunc(s.reconEdit)},
		{http.MethodPost, "/reconciliations/{id}/discard", TxnWrite, http.HandlerFunc(s.reconDiscard)},

		// p17.2 bank-CSV import (Appendix B/F: /import** = TxnWrite -- importing feeds
		// the ledger). GET is the upload + mapping form; POST /import/preview parses the
		// multipart CSV under the mapping and shows the 20-row preview (no batch created);
		// POST /import confirms -- creates the batch (validating the account maps to the
		// subsidiary) and stages all rows with duplicates flagged. The literal
		// "/import/preview" is more specific than "/import", so the Go 1.22+ mux routes
		// them precisely. All THREE are TxnWrite (a viewer must not stage rows). The nav
		// entry lights up now that GET /import is registered. The permission-matrix test
		// picks these up automatically (rule 8); the p17.3 review queue is a later step.
		{http.MethodGet, "/import", TxnWrite, http.HandlerFunc(s.importPage)},
		{http.MethodPost, "/import/preview", TxnWrite, http.HandlerFunc(s.importPreview)},
		{http.MethodPost, "/import", TxnWrite, http.HandlerFunc(s.importConfirm)},
		// p26.63: soft-delete a saved mapping profile (deactivate; the batch FK keeps
		// referencing it). TxnWrite -- managing import mappings is part of the import
		// workflow. "/import/profiles/{id}/delete" is distinct from the batches/rows
		// literals (Go 1.22+ mux); the permission-matrix test picks it up (rule 8).
		{http.MethodPost, "/import/profiles/{id}/delete", TxnWrite, http.HandlerFunc(s.importProfileDelete)},
		// p17.3 review queue -> post. The batch queue (pending list + progress
		// indicator), "edit & post" (the phase-12 editor prefilled with the batch's
		// subsidiary LOCKED), post (create the balanced txn + link the row), and
		// discard-with-reason. ALL FOUR are TxnWrite: this is an import-INTO-LEDGER
		// workflow -- even viewing the staging queue is write-adjacent and the actions
		// mutate the ledger, so the view perm is TxnWrite too (a TxnRead user has no
		// reason to work an import queue). Documented in DECISIONS p17.3. The literal
		// "/import/preview" and "/import/batches/{id}" / "/import/rows/{id}/..." don't
		// collide (Go 1.22+ mux). The permission-matrix test picks these up
		// automatically (rule 8); the import-result page links the batch queue.
		{http.MethodGet, "/import/batches/{id}", TxnWrite, http.HandlerFunc(s.importBatchQueue)},
		{http.MethodGet, "/import/rows/{id}/edit", TxnWrite, http.HandlerFunc(s.importRowEditForm)},
		{http.MethodPost, "/import/rows/{id}/post", TxnWrite, http.HandlerFunc(s.importRowPost)},
		{http.MethodPost, "/import/rows/{id}/discard", TxnWrite, http.HandlerFunc(s.importRowDiscard)},
		// p27.2 split-derived budget PLANS (the split-derived budget model; DECISIONS
		// "Budget redesign"). Budgets are PLANNING/BOOKKEEPING data (like funds, p12.5),
		// so the VIEW routes are TxnRead -- they feed the p27.3 reports -- and every
		// create/edit/delete MUTATION is TxnWrite (manage = TxnWrite). The old schedule-
		// based /budgets + /schedules routes were RETIRED in p27.3 (the schedule model is
		// gone). The plan detail page hosts the split-entry GRID (bulk save + cadence
		// helper) and the flat-CSV import. The permission-matrix test picks these up
		// automatically (rule 8).
		{http.MethodGet, "/budget-plans", TxnRead, http.HandlerFunc(s.budgetPlansPage)},
		{http.MethodGet, "/budget-plans/new", TxnWrite, http.HandlerFunc(s.budgetPlanNewForm)},
		{http.MethodPost, "/budget-plans", TxnWrite, http.HandlerFunc(s.budgetPlanCreate)},
		{http.MethodGet, "/budget-plans/{id}", TxnRead, http.HandlerFunc(s.budgetPlanDetail)},
		{http.MethodPost, "/budget-plans/{id}", TxnWrite, http.HandlerFunc(s.budgetPlanUpdate)},
		{http.MethodPost, "/budget-plans/{id}/splits", TxnWrite, http.HandlerFunc(s.budgetSplitsSave)},
		{http.MethodPost, "/budget-plans/{id}/import", TxnWrite, http.HandlerFunc(s.budgetPlanImport)},
		// The DELETE probe is registered LAST among the plan routes so the reachability
		// matrix (which substitutes {id}->1 and POSTs) hits it AFTER the split/import
		// probes -- deleting the seeded plan first would 404 the later probes.
		{http.MethodPost, "/budget-plans/{id}/delete", TxnWrite, http.HandlerFunc(s.budgetPlanDelete)},
		// p20.2 submitter workspace (Phase 20). ALL ExpenseSubmit -- the STANDALONE
		// capability (p20.1, INDEPENDENT of txn_perm): a pure submitter passes these and
		// is 403 on the ledger/reports (Txn*/ReportGroup). Ownership is enforced INSIDE
		// each id-taking handler (a missing OR not-owned report id -> 404, uniform, no
		// enumeration) -- the perm gate alone is not enough. GET /expenses is the "my
		// reports" list (lighting up the nav.myexpenses entry, shell.go); POST /expenses
		// creates a draft; GET /expenses/{id} is the editor; POST /expenses/{id}/lines is
		// the p25.4 auto-row grid's BULK save (replace-set diff-by-line-id under ONE
		// change, replacing the old line-at-a-time CRUD); submit/resubmit follow. The
		// literal ".../submit"/".../resubmit" beat the ".../{id}" GET wildcard, so the Go
		// 1.22+ mux routes them precisely. The permission-matrix test picks these up
		// automatically (rule 8).
		{http.MethodGet, "/expenses", ExpenseSubmit, http.HandlerFunc(s.expensesPage)},
		{http.MethodPost, "/expenses", ExpenseSubmit, http.HandlerFunc(s.expenseCreate)},
		{http.MethodGet, "/expenses/{id}", ExpenseSubmit, http.HandlerFunc(s.expenseDetail)},
		{http.MethodPost, "/expenses/{id}/subsidiary", ExpenseSubmit, http.HandlerFunc(s.expenseSetSubsidiary)},
		{http.MethodPost, "/expenses/{id}/header", ExpenseSubmit, http.HandlerFunc(s.expenseSetHeader)},
		{http.MethodPost, "/expenses/{id}/lines", ExpenseSubmit, http.HandlerFunc(s.expenseLinesSave)},
		{http.MethodPost, "/expenses/{id}/submit", ExpenseSubmit, http.HandlerFunc(s.expenseSubmit)},
		{http.MethodPost, "/expenses/{id}/resubmit", ExpenseSubmit, http.HandlerFunc(s.expenseResubmit)},
		// p20.3 reviewer queue -> convert / reject (Phase 20, COMPLETES it). ALL TxnWrite
		// -- reviewing = editing the books (the mirror of the p17.3 import review->post).
		// This is a DISTINCT surface from the p20.2 submitter workspace (ExpenseSubmit): a
		// pure submitter hitting these routes is gated by TxnWrite -> 403 (the two roles
		// are separate). The literal "/expenses/review" beats the "/expenses/{id}"
		// ExpenseSubmit wildcard (Go 1.22+ mux precedence), and ".../review/{id}/post" /
		// ".../review/{id}/reject" beat the "/expenses/{id}/..." ExpenseSubmit wildcards.
		// "review & post" opens the phase-12 editor prefilled with the report's splits +
		// the subsidiary LOCKED; POST creates the balanced txn AND converts the report
		// atomically (store.PostAndConvertExpenseReport); reject-with-reason routes it back
		// to the submitter. The permission matrix picks these up automatically (rule 8),
		// and nav.expensereview (shell.go) lights up now that GET /expenses/review exists.
		//
		// ROUTING: the 2-segment queue route "/expenses/review" beats "/expenses/{id}"
		// (a literal is more specific). The ACTION routes deliberately put the verb in
		// segment 3 as a LITERAL ("/expenses/review/post/{id}", ".../reject/{id}") -- NOT
		// "/expenses/review/{id}/post" -- specifically to avoid a mux AMBIGUITY with
		// "/expenses/{id}/lines/{lid}" (both would match "/expenses/review/lines/post"
		// with neither dominating -> a register panic). With the verb literal in seg 3
		// (post/reject can never equal "lines"), no path matches both patterns.
		{http.MethodGet, "/expenses/review", TxnWrite, http.HandlerFunc(s.expenseReview)},
		{http.MethodGet, "/expenses/review/{id}", TxnWrite, http.HandlerFunc(s.expenseReviewForm)},
		{http.MethodPost, "/expenses/review/post/{id}", TxnWrite, http.HandlerFunc(s.expenseReviewPost)},
		{http.MethodPost, "/expenses/review/reject/{id}", TxnWrite, http.HandlerFunc(s.expenseReviewReject)},
		// p25.3 discard (ExpenseSubmit): hard-deletes a DRAFT report + its lines.
		// Registered LAST of the /expenses routes on purpose: the permission-matrix
		// reachability sweep (TestRouteRegistryComplete) sends a real POST to every route
		// in registry order against ONE shared seeded report (id 1); discard is the only
		// route that can DELETE that report, so it must run after every route that needs
		// it to still exist. (Mux precedence is unaffected by slice order.)
		{http.MethodPost, "/expenses/{id}/discard", ExpenseSubmit, http.HandlerFunc(s.expenseDiscard)},
	}
	// p15.12 reports index: GET /reports lists the reports the current user may
	// access, grouped by report group, each a link to a concrete /reports/{id}. The
	// page itself is AnyUser (visible to any logged-in user); it FILTERS its contents
	// by grant, reusing decide()+grantChecker (the same enforcement path the concrete
	// report routes use), so an ungranted user lands on an empty list (200), not a 403.
	// The exact literal "/reports" does not collide with the "/reports/{id}" literals
	// mounted below (Go 1.22+ mux). Registering it lights up the nav.reports entry
	// (shell.go). The permission-matrix test picks it up automatically (rule 8).
	routes = append(routes, Route{http.MethodGet, "/reports", AnyUser, http.HandlerFunc(s.reportsIndex)})
	// p15.1 reports: auto-mount ONE concrete route pair per registered report --
	// GET /reports/{id} (HTML, into the app shell) and GET /reports/{id}.csv (the
	// machine export) -- gated by ReportGroup(report.Group). Mounting CONCRETE
	// literal paths (not a /reports/{id} wildcard) is deliberate: (a) each report
	// carries its OWN group Perm, which a single wildcard route could not express;
	// (b) the permission-matrix + registry-completeness tests substitute {id}->1 in
	// wildcards, so a wildcard would resolve to a bogus report id and 404; (c) an
	// unknown /reports/{id} then simply never matches a route and the mux 404s on
	// its own (rule 8: the whole surface is these declared routes). Because these
	// are appended to the SAME registry the matrix iterates, every report route is
	// permission-tested with ZERO test edits -- the "appears in the matrix
	// automatically" requirement. p15.3–p15.11 add reports to reports.Default();
	// the routes appear here with no change to this loop.
	for _, rep := range s.reports.All() {
		perm := ReportGroupFor(rep)
		routes = append(
			routes,
			Route{http.MethodGet, "/reports/" + rep.ID, perm, http.HandlerFunc(s.reportPage)},
			Route{http.MethodGet, "/reports/" + rep.ID + ".csv", perm, http.HandlerFunc(s.reportCSV)},
			// p15.3d drill-down: a per-report /reports/{id}/drill route gated by the
			// SAME ReportGroup as the report -- so drill visibility EQUALS report
			// visibility (a viewer who can see the number can drill it) and, being a
			// concrete registry route, it is permission-matrix-checked with zero test
			// edits (the matrix hits it bare, decoding to an empty drill => a 200 empty
			// list for an authorized persona, 403 for an ungranted one). The Drill
			// FILTER arrives as query params (reports.Drill.Encode -> DecodeDrill); a
			// per-report route keeps the Perm STATIC (not a dynamic-perm endpoint).
			Route{http.MethodGet, "/reports/" + rep.ID + "/drill", perm, http.HandlerFunc(s.reportDrill)},
		)
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

// grantScope is what the grant probe reports for a (user, report-group): whether the
// user holds an UNSCOPED grant on the group and/or a program-SCOPED one (p27.4). The
// two are not mutually exclusive as a type, but the one-scope-per-group model means a
// held grant is exactly one of them; Held is the union (holds SOME grant on the group).
type grantScope struct {
	Held     bool // holds a grant on the group (scoped or unscoped)
	Unscoped bool // the held grant is org-wide (program_id NULL)
}

// decide is the pure enforcement decision (rule 8's policy, D10, extended p27.4). It
// takes the route's Perm, the resolved user (nil == anonymous), and a grant closure
// (queried lazily ONLY for ReportGroup so the hot path never loads grants). It is
// pure and total so TestDecidePolicy can prove every Perm x persona.
//
// ReportGroup policy (p27.4): an UNSCOPED grant allows any report in the group. A
// program-SCOPED grant allows a PROGRAM-DIMENSIONED report (Perm.programDim -- rows
// then filtered to the granted subtree, in resolveParams) but DENIES a non-program
// report in the same group (it cannot be filtered by program, so a purely
// program-scoped user has no basis to see it). No grant -> forbid.
func decide(p Perm, u *store.CurrentUser, grant func(group string) grantScope) outcome {
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
		gs := grant(p.group)
		if !gs.Held {
			break // no grant on the group -> forbid
		}
		if gs.Unscoped || p.programDim {
			// Unscoped grant reaches everything in the group; a program-scoped grant
			// reaches a program-dimensioned report (rows filtered downstream). A
			// program-scoped grant on a NON-program report falls through to forbid.
			return outcomeAllow
		}
	case permExpenseSubmit:
		// STANDALONE capability (p20.1): reads ONLY the can_submit_expenses flag, so
		// it is independent of txn_perm by construction (admin already allowed above).
		if u.CanSubmitExpenses {
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

// grantChecker returns the grant closure decide needs (p27.4: a grantScope per
// group, not a bare bool). For non-ReportGroup perms (and anonymous/admin, already
// decided upstream) it never queries -- it returns a closure that is simply never
// called. For a ReportGroup route with a concrete user it loads the user's grants
// ONCE and reports each group's scope (unscoped vs program-scoped) in memory. A
// grant-read error fails closed (no grant), so a transient DB fault denies rather
// than leaks access. The one-scope-per-group model means at most one grant row per
// group, so the closure returns that row's scope (or a zero grantScope = not held).
func (s *server) grantChecker(ctx context.Context, u *store.CurrentUser, perm Perm) func(string) grantScope {
	// An admin is allowed by decide's u.IsAdmin short-circuit BEFORE the grant closure
	// is ever consulted (routes.go decide), so an admin ReportGroup request needs no
	// grant query: return the empty-scope closure (never called; fails closed anyway).
	if perm.kind != permReportGroup || u == nil || u.IsAdmin {
		return func(string) grantScope { return grantScope{} }
	}
	grants, err := s.store.ReportGrants(ctx, u.ID)
	if err != nil {
		return func(string) grantScope { return grantScope{} }
	}
	return func(group string) grantScope {
		for _, g := range grants {
			if g.Group == group {
				return grantScope{Held: true, Unscoped: g.ProgramID == nil}
			}
		}
		return grantScope{}
	}
}

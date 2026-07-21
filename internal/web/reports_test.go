package web

import (
	"context"
	"database/sql"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/i18n"
	"cuento/internal/ids"
	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// p15.1 report-framework web tests. They drive the REAL mounted router (httptest)
// over a migrated temp db with the startup report-group sync run, so the auto-
// mounted /reports/{id} routes, the shared params form, and the CSV endpoint are
// exercised end to end (no handler-level store mocks -- AGENTS testing conventions).
//
// The PERMISSION-matrix requirement ("new reports appear in the matrix
// automatically") is covered with ZERO edits by routes_test.go: because report
// routes are appended to the SAME registry TestPermissionMatrix iterates, and the
// ReportsOnly persona is granted the trial-balance report's group ("financial"),
// the matrix already asserts granted->200 / ungranted->403 on GET
// /reports/trial_balance. These tests cover the framework/report behaviors: unknown
// id -> 404, the scope selector on EVERY report, and the trial-balance report
// rendering typed cells + CSV.

// reportsApp builds a real app with the report groups synced (so a ReportGroup grant
// has a valid FK) and returns the handler + store + sessions. It seeds one account
// with a posted balance so the trial-balance report reads REAL data through the toolkit.
func reportsApp(t *testing.T) (http.Handler, *store.Store, *sql.DB, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	if err := SyncReportGroups(context.Background(), st); err != nil {
		t.Fatalf("sync report groups: %v", err)
	}

	// Seed a couple of accounts + a balanced posted transaction so the smoke report
	// (SubtreeBalancesAsOf at root scope) returns non-empty typed cells.
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	mkAcct := func(name string) ids.AccountID {
		id, err := st.CreateAccount(ctx, store.CreateAccountInput{
			Type: "asset", DefaultCurrency: "USD",
			Names: map[string]string{"en": name}, Subsidiaries: []ids.SubsidiaryID{1},
		})
		if err != nil {
			t.Fatalf("seed account %s: %v", name, err)
		}
		return id
	}
	a1, a2 := mkAcct("Cash"), mkAcct("Bank")
	// A balanced 250.00/-250.00 posting: the trial-balance report shows Cash +250.00
	// and Bank -250.00, whose native USD total nets to zero (a real trial balance).
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-06-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: a1, Amount: 25000, Position: 0},
			{AccountID: a2, Amount: -25000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}

	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, db, app.sessions
}

// grantGroup gives userID a read grant on group via direct SQL (grant writers are
// p13.2; raw SQL in tests is in-convention). The group must already be synced (FK).
func grantGroup(t *testing.T, db *sql.DB, userID ids.UserID, group string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO user_report_grants (user_id, group_name) VALUES (?, ?)`,
		userID, group); err != nil {
		t.Fatalf("grant %q to user %d: %v", group, userID, err)
	}
}

// TestReportGroupsSynced is the "registry sync creates groups" listed test at the
// DB layer: after the startup SyncReportGroups (run by reportsApp), report_groups
// holds EXACTLY the code-declared set reports.Groups() -- not just the one group the
// smoke report references. So a group declared before any report uses it (funds /
// programs / tax) still lands in the table (its grant FK is valid the moment p15.3+
// or an admin grants it).
func TestReportGroupsSynced(t *testing.T) {
	_, st, _, _ := reportsApp(t)

	got, err := st.ReportGroupNames(context.Background())
	if err != nil {
		t.Fatalf("ReportGroupNames: %v", err)
	}
	want := reports.Groups()
	if len(got) != len(want) {
		t.Fatalf("synced groups = %v, want %v", got, want)
	}
	// SyncReportGroups syncs in declared order and ListReportGroups returns sort
	// order, so the sets match positionally.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("synced group[%d] = %q, want %q (full: got %v want %v)", i, got[i], want[i], got, want)
		}
	}
}

// TestReportUnknownID404: a /reports/{id} for an id that is not registered never
// matches a mounted route, so the mux 404s. (Admin persona: is_admin implies all,
// so a 404 here is the mux's, not a permission bounce.)
func TestReportUnknownID404(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown report id status = %d, want 404", rec.Code)
	}
	// The CSV variant of an unknown id likewise 404s.
	rec = asUser(t, h, sm, admin, http.MethodGet, "/reports/does-not-exist.csv", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown report CSV id status = %d, want 404", rec.Code)
	}
}

// TestScopeSelectorOnEveryReport: the params form on EVERY registered report page
// includes the subsidiary SCOPE selector (D18 -- every report is scoped). Iterating
// reports.Default().All() means a report added in p15.3+ is covered automatically.
func TestScopeSelectorOnEveryReport(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true) // is_admin reaches every report

	all := reports.Default().All()
	if len(all) == 0 {
		t.Fatal("no reports registered; expected at least the trial-balance report")
	}
	for _, rep := range all {
		rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+rep.ID, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /reports/%s status = %d, want 200", rep.ID, rec.Code)
			continue
		}
		body := rec.Body.String()
		// The scope selector: a <select name="scope"> with the scope option marker
		// class. Assert the name attr (language-independent) is present.
		if !strings.Contains(body, `name="scope"`) {
			t.Errorf("report %s page missing subsidiary scope selector (name=\"scope\")", rep.ID)
		}
		if !strings.Contains(body, `class="report-scope-select"`) {
			t.Errorf("report %s page missing the scope select element", rep.ID)
		}
		// And a real subsidiary option (the seeded root, id 1) is present.
		if !strings.Contains(body, `<option value="1"`) {
			t.Errorf("report %s scope selector has no subsidiary option", rep.ID)
		}
	}
}

// TestReportFilterPlacement (p26.86): EVERY report renders its filter form in the
// SECOND-LEVEL nav bar (SubNavControls="report" -> the form inside .app-subnav), and
// NONE inline in the page body. It pins one light report (trial_balance) AND one of the
// previously-inline dense reports (income_statement) so a regression — a form vanishing
// from the bar, leaking into the body, or the p26.76 inline fallback creeping back — is
// caught. There is no "Filters" heading now the controls live in the bar.
func TestReportFilterPlacement(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// The bar opens with <nav class="app-subnav" ...>; the body with <main id="main".
	subnavForm := func(body string) bool {
		i := strings.Index(body, `class="app-subnav"`)
		j := strings.Index(body, `id="main"`)
		return i >= 0 && i < j && strings.Contains(body[i:j], `class="report-params"`)
	}
	mainForm := func(body string) bool {
		j := strings.Index(body, `id="main"`)
		return j >= 0 && strings.Contains(body[j:], `class="report-params"`)
	}

	// Both a light report (trial_balance) and a dense one (income_statement, previously
	// inline) render their filter form in the subnav, never in the body.
	for _, id := range []string{reports.TrialBalanceReportID, "income_statement"} {
		body := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+id, nil).Body.String()
		if !subnavForm(body) {
			t.Errorf("%s: filter form not in the subnav (should be)", id)
		}
		if mainForm(body) {
			t.Errorf("%s: filter form leaked into the body (should be subnav-only)", id)
		}
		// The dropped "Filters" legend must not render as a visible heading.
		if strings.Contains(body, "<legend>") {
			t.Errorf("%s: a <legend> heading still renders (the Filters legend was dropped)", id)
		}
	}
}

// TestReportResultsFragmentSwap (p26.90): a filter change is the subnav form's hx-get
// targeting #report-results (HX-Target header), so the handler must serve the BARE
// results FRAGMENT (the CSV link + table wrapped in #report-results) — not a whole
// document injected into the swap target. It also confirms the fragment's CSV href
// reflects the request's params (the export link is recomputed from the current filter
// state and swapped in with the results), and that the shell chrome is absent.
func TestReportResultsFragmentSwap(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	req := httptest.NewRequest(http.MethodGet, "/reports/"+reports.TrialBalanceReportID+"?scope=1&asof=2026-06-30", strings.NewReader(""))
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "report-results")
	req.AddCookie(mintCookie(t, sm, admin))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("results-fragment GET status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The bare fragment: the #report-results wrapper + the table are present.
	if !strings.Contains(body, `id="report-results"`) {
		t.Errorf("results fragment missing the #report-results wrapper; body: %s", body)
	}
	if !strings.Contains(body, "report-table") {
		t.Errorf("results fragment missing the report table; body: %s", body)
	}
	// NOT a full document: no shell nav chrome (would inject a whole doc into the swap).
	if strings.Contains(body, `class="app-nav"`) {
		t.Errorf("results fragment returned full shell chrome; body: %s", body)
	}
	// The filter form is OUTSIDE the swapped region, so it must NOT be in the fragment.
	if strings.Contains(body, `class="report-params"`) {
		t.Errorf("results fragment leaked the filter form (it lives outside #report-results); body: %s", body)
	}
	// The CSV export href is recomputed from the request params and rides in the fragment,
	// so a filter change refreshes the export link (never stale).
	if !strings.Contains(body, `report-csv-link`) {
		t.Errorf("results fragment missing the CSV export link; body: %s", body)
	}
	if !strings.Contains(body, "asof=2026-06-30") {
		t.Errorf("results fragment CSV href does not reflect the request params (asof); body: %s", body)
	}
}

// TestReportMissingRateInlineError (p26.95): converting a report to a target
// currency with NO exchange rate on file returns a CLEAN report-level error in the
// results region with HTTP 200 -- NOT a 500. Under apply-on-change (p26.90) a 5xx
// would leave htmx showing nothing (silent no-op); a 200 with an inline message lets
// the filter render the reason. The reportsApp db has USD-only data and no rates, so
// converting to MXN (a seeded currency) needs a USD->MXN rate that does not exist.
func TestReportMissingRateInlineError(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// As the auto-apply fragment swap would arrive: HX-Target report-results.
	req := httptest.NewRequest(http.MethodGet,
		"/reports/"+reports.TrialBalanceReportID+"?scope=1&asof=2026-06-30&currency=MXN",
		strings.NewReader(""))
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "report-results")
	req.AddCookie(mintCookie(t, sm, admin))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// A clean 200 (not a 500) so htmx swaps the fragment.
	if rec.Code != http.StatusOK {
		t.Fatalf("rate-less conversion status = %d, want 200 (clean inline error, not 5xx); body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The inline error region carries the localized no-rate message naming the currency.
	if !strings.Contains(body, i18n.T("en", "reports.error.no_rate", "MXN")) {
		t.Errorf("results fragment missing the localized no-rate error for MXN; body: %s", body)
	}
	// The results fragment is still the bare region (no shell chrome) and shows NO
	// table / CSV link (the report did not produce a valid result).
	if !strings.Contains(body, `id="report-results"`) {
		t.Errorf("error fragment missing the #report-results wrapper; body: %s", body)
	}
	if strings.Contains(body, "report-csv-link") {
		t.Errorf("error fragment should not offer a CSV export for an errored report; body: %s", body)
	}
}

// TestReportMissingRateFullPageError (p26.95): the FULL-PAGE path (a reload / bookmark
// replay with the bad currency in the URL — no HX-Target) also renders the inline
// no-rate error, not a 500. p26.90's hx-push-url syncs ?currency=MXN into the URL, so a
// reload after the fragment error issues a plain GET; report.tmpl delegates to the
// report-results define, so .Error flows through and the whole page shows the message.
func TestReportMissingRateFullPageError(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// A plain full-page GET (no HX-Request / HX-Target), as a reload of the pushed URL.
	rec := asUser(t, h, sm, admin, http.MethodGet,
		"/reports/"+reports.TrialBalanceReportID+"?scope=1&asof=2026-06-30&currency=MXN", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("full-page rate-less conversion status = %d, want 200 (inline error, not 5xx); body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, i18n.T("en", "reports.error.no_rate", "MXN")) {
		t.Errorf("full page missing the localized no-rate error for MXN; body: %s", body)
	}
	// The whole shell rendered (this is NOT a fragment) but the results region shows the
	// error, with no table / CSV export.
	if !strings.Contains(body, `id="report-results"`) {
		t.Errorf("full page missing the #report-results region; body: %s", body)
	}
	if strings.Contains(body, "report-csv-link") {
		t.Errorf("full page should not offer a CSV export for an errored report; body: %s", body)
	}
}

// TestTrialBalanceReportRenders: the trial-balance report renders its typed cells (a
// money cell formatted with a currency prefix and a native total row) into the HTML
// page. Proves the framework is end-to-end: route -> params -> toolkit -> store ->
// Table -> renderer, with real account names from account_names.
func TestTrialBalanceReportRenders(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.TrialBalanceReportID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trial balance status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "report-table") {
		t.Errorf("trial balance page missing the report table")
	}
	// A money cell with the per-currency symbol (the seeded 250.00 Cash balance, USD prefix "$", p26.24).
	if !strings.Contains(body, "$250.00") {
		t.Errorf("trial balance missing formatted money cell $250.00; body:\n%s", body)
	}
	// A resolved account name (a stored proper noun, verbatim).
	if !strings.Contains(body, "Cash") {
		t.Errorf("trial balance missing the Cash account name; body:\n%s", body)
	}
	// The total (native) subtotal row emphasis class (a total row was emitted).
	if !strings.Contains(body, "report-subtotal") {
		t.Errorf("trial balance missing the native total row")
	}
	// The grand converted total row (RowTotal styling).
	if !strings.Contains(body, "report-total") {
		t.Errorf("trial balance missing the converted total row")
	}
}

// TestWideMatrixReportsFullWidth (p29.11): a comparative statement whose columns fan out
// (income_statement's period columns) renders in the FULL-viewport shell (app-main-full) so
// they show without a horizontal scroll; a narrow report (trial balance) keeps the ordinary
// wide shell (no app-main-full, so it stays 100rem-capped). The program statement was a wide
// matrix (one column per program) but is now the p31 VERTICAL program tree with a single
// Amount column, so it too is narrow (WideMatrix dropped) — asserted below with trial balance.
func TestWideMatrixReportsFullWidth(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	for _, id := range []string{reports.IncomeStatementReportID} {
		rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", id, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "app-main-full") {
			t.Errorf("%s: comparative statement missing app-main-full (full-viewport opt-out)", id)
		}
	}

	// Narrow reports keep the ordinary wide shell: wide, but NOT full-viewport. The program
	// statement (now a vertical program tree, single Amount column) joins the trial balance.
	for _, id := range []string{reports.TrialBalanceReportID, reports.ProgramStatementReportID} {
		rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", id, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "app-main-full") {
			t.Errorf("%s should NOT be full-viewport (a narrow report keeps the 100rem cap)", id)
		}
	}
}

// TestProgramSelectShowsHierarchy (p29.13): the program-statement report's program
// selector is a fuzzy hierarchy combobox (combo-input) whose options carry the dotted
// ancestor path on data-path, exactly like the account pickers (p28.2), so the shared
// combobox ranks/labels by the program tree. The IMPLIED ROOT segment ("General.")
// is stripped from the filter's paths, and the default (value 0) is an EMPTY option so
// the box renders blank and a cleared input means "all programs".
func TestProgramSelectShowsHierarchy(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// A grandchild under the seeded root "General" so a multi-segment path exists and we
	// can prove ONLY the leading root segment is stripped (not every segment).
	ctx := store.WithActor(context.Background(), store.Actor{ID: admin})
	edu, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: 1, Name: "Education"})
	if err != nil {
		t.Fatalf("create program: %v", err)
	}
	if _, err := st.CreateProgram(ctx, store.CreateProgramInput{ParentID: edu, Name: "Interns"}); err != nil {
		t.Fatalf("create child program: %v", err)
	}

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.ProgramStatementReportID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("program statement status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `report-program-select combo-input`) {
		t.Errorf("program selector is not a combo-input; body:\n%s", body)
	}
	// The implied root "General." prefix is dropped: the child reads "Education", the
	// grandchild "Education.Interns" -- NOT "General.Education[.Interns]".
	if !strings.Contains(body, `data-path="Education"`) {
		t.Errorf("child program path should drop the implied root ('Education'); body:\n%s", body)
	}
	if !strings.Contains(body, `data-path="Education.Interns"`) {
		t.Errorf("grandchild path should keep sub-hierarchy minus root ('Education.Interns'); body:\n%s", body)
	}
	if strings.Contains(body, `data-path="General.`) {
		t.Errorf("program filter still carries the implied 'General.' root prefix; body:\n%s", body)
	}
	// The default option is EMPTY (blank label, no data-path) and opts into empty==all via
	// data-empty-value="0"; the greyed placeholder still names the state.
	if !strings.Contains(body, `data-empty-value="0"`) {
		t.Errorf("program select missing data-empty-value opt-in; body:\n%s", body)
	}
	if !strings.Contains(body, `<option value="0" selected></option>`) {
		t.Errorf("default program option should be empty (blank label); body:\n%s", body)
	}
}

// TestPeriodDefaults (p30.8, refines p29.12): the PERIOD default distinguishes an
// ABSENT bound from a PRESENT-BUT-EMPTY one.
//   - ABSENT (no from=/to= key — first page load) -> YTD: From = Jan 1 of the current
//     year, To = today.
//   - PRESENT-BUT-EMPTY (?from=&to= — the user cleared the input, htmx auto-submitted)
//     -> BRACKET: From = day BEFORE the oldest txn, To = day AFTER the newest. reportsApp
//     seeds a single 2025-06-01 posting, so the bracket is 2025-05-31 .. 2025-06-02.
//   - EXPLICIT from/to is respected verbatim.
func TestPeriodDefaults(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// ABSENT: no from/to key at all -> YTD (Jan 1 of the current year .. today). The
	// handler's clock is real time.Now, so compute the window from the same clock.
	now := time.Now()
	ytdFrom := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	ytdTo := now.Format("2006-01-02")
	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.IncomeStatementReportID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("income statement status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ytdFrom) {
		t.Errorf("absent From should default to YTD start %q; body:\n%s", ytdFrom, body)
	}
	if !strings.Contains(body, ytdTo) {
		t.Errorf("absent To should default to today %q; body:\n%s", ytdTo, body)
	}

	// PRESENT-BUT-EMPTY: from= and to= present with empty values -> bracket all data.
	rec = asUser(t, h, sm, admin, http.MethodGet,
		"/reports/"+reports.IncomeStatementReportID+"?from=&to=", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("present-empty status = %d, want 200", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "2025-05-31") {
		t.Errorf("cleared From should bracket to the day BEFORE the oldest txn (2025-05-31); body:\n%s", body)
	}
	if !strings.Contains(body, "2025-06-02") {
		t.Errorf("cleared To should bracket to the day AFTER the newest txn (2025-06-02); body:\n%s", body)
	}

	// An EXPLICIT period is respected (not overridden by either default).
	rec = asUser(t, h, sm, admin, http.MethodGet,
		"/reports/"+reports.IncomeStatementReportID+"?from=2025-01-01&to=2025-12-31", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit-period status = %d, want 200", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "2025-01-01") || !strings.Contains(body, "2025-12-31") {
		t.Errorf("explicit from/to should be respected verbatim; body:\n%s", body)
	}
}

// TestTrialBalanceReportCSV: the CSV endpoint returns text/csv, an attachment
// filename, and a parseable body whose header + rows reflect the report (proving the
// CSV renderer is wired through the route with the same params).
func TestTrialBalanceReportCSV(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.TrialBalanceReportID+".csv", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trial balance CSV status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("CSV Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "trial_balance.csv") {
		t.Errorf("CSV Content-Disposition = %q, want attachment filename trial_balance.csv", cd)
	}
	body := rec.Body.String()
	// Machine-plain money (no grouping separators): 250.00 for the seeded balance.
	if !strings.Contains(body, "250.00") {
		t.Errorf("CSV body missing machine-plain amount 250.00; body:\n%s", body)
	}
}

// TestReportCSVMetadataPreamble: a downloaded report CSV begins with a two-line
// metadata header -- line 1 the localized report title, line 2 the date context --
// followed by a blank row and then the table header, for BOTH an as-of report
// (balance sheet) and a period report (income statement). The preamble is emitted by
// the handler (writeReportCSVPreamble), not by reports.WriteCSV, so the reports
// goldens stay unchanged. We parse the body as real CSV (FieldsPerRecord = -1 to
// allow the ragged 1-2 field preamble rows) so a title-with-a-comma quoting
// regression would surface, not just a substring match.
func TestReportCSVMetadataPreamble(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// asof: the balance sheet declares ParamsSpec.AsOf -> ["As of", <date>].
	t.Run("asof", func(t *testing.T) {
		rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.BalanceSheetReportID+".csv", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("balance sheet CSV status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		assertBlankSeparatorLine(t, body)
		// csv.Reader drops the blank separator line, so parsed rows are
		// [title, date, <table header>, ...data].
		rows := parseCSVBody(t, body)
		if len(rows) < 3 {
			t.Fatalf("CSV has %d rows, want title + date + table; body:\n%s", len(rows), body)
		}
		wantTitle := i18n.T("en", "reports.balance_sheet.title")
		if got := rows[0]; len(got) != 1 || got[0] != wantTitle {
			t.Errorf("row 0 = %q, want title line [%q]", got, wantTitle)
		}
		asofLabel := i18n.T("en", "reports.params.asof")
		if got := rows[1]; len(got) != 2 || got[0] != asofLabel || got[1] == "" {
			t.Errorf("row 1 = %q, want [%q, <date>]", got, asofLabel)
		}
		// The table (its localized header row) still follows the preamble intact.
		wantHeader := i18n.T("en", "reports.balance_sheet.col.line")
		if got := rows[2]; len(got) == 0 || got[0] != wantHeader {
			t.Errorf("row 2 = %q, want the table header starting with %q", got, wantHeader)
		}
	})

	// period: the income statement declares ParamsSpec.Period -> ["From", d, "To", d].
	t.Run("period", func(t *testing.T) {
		rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.IncomeStatementReportID+".csv", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("income statement CSV status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		assertBlankSeparatorLine(t, body)
		rows := parseCSVBody(t, body)
		if len(rows) < 3 {
			t.Fatalf("CSV has %d rows, want title + date + table; body:\n%s", len(rows), body)
		}
		wantTitle := i18n.T("en", "reports.income_statement.title")
		if got := rows[0]; len(got) != 1 || got[0] != wantTitle {
			t.Errorf("row 0 = %q, want title line [%q]", got, wantTitle)
		}
		fromLabel := i18n.T("en", "reports.params.from")
		toLabel := i18n.T("en", "reports.params.to")
		if got := rows[1]; len(got) != 4 || got[0] != fromLabel || got[1] == "" || got[2] != toLabel || got[3] == "" {
			t.Errorf("row 1 = %q, want [%q, <date>, %q, <date>]", got, fromLabel, toLabel)
		}
		wantHeader := i18n.T("en", "reports.income_statement.col.line")
		if got := rows[2]; len(got) == 0 || got[0] != wantHeader {
			t.Errorf("row 2 = %q, want the table header starting with %q", got, wantHeader)
		}
	})
}

// TestReportPrintMeta (p29.17): the report page renders a print-only self-identifying
// header (.report-print-meta) INSIDE #report-results carrying the resolved date context,
// so a printed/PDF'd report is a self-describing snapshot. It is hidden on screen via CSS
// only, so the element + its date text are in the server render. An as-of report prints an
// "As of <date>" line; a period report a "From <date> · To <date>" line. The date is the
// RESOLVED, user-formatted value (ISO for the default-format test user).
func TestReportPrintMeta(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	t.Run("asof", func(t *testing.T) {
		rec := asUser(t, h, sm, admin, http.MethodGet,
			"/reports/"+reports.BalanceSheetReportID+"?scope=1&asof=2026-06-30", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("balance sheet status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `report-print-meta`) {
			t.Fatalf("report page missing the .report-print-meta print header; body:\n%s", body)
		}
		asofLabel := i18n.T("en", "reports.params.asof")
		if !strings.Contains(body, asofLabel+" 2026-06-30") {
			t.Errorf("print-meta missing the resolved as-of date %q %q; body:\n%s", asofLabel, "2026-06-30", body)
		}
	})

	t.Run("period", func(t *testing.T) {
		rec := asUser(t, h, sm, admin, http.MethodGet,
			"/reports/"+reports.IncomeStatementReportID+"?scope=1&from=2026-01-01&to=2026-06-30", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("income statement status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `report-print-meta`) {
			t.Fatalf("report page missing the .report-print-meta print header; body:\n%s", body)
		}
		fromLabel := i18n.T("en", "reports.params.from")
		toLabel := i18n.T("en", "reports.params.to")
		if !strings.Contains(body, fromLabel+" 2026-01-01") || !strings.Contains(body, toLabel+" 2026-06-30") {
			t.Errorf("print-meta missing the resolved from/to dates (%q 2026-01-01, %q 2026-06-30); body:\n%s",
				fromLabel, toLabel, body)
		}
	})
}

// parseCSVBody parses a downloaded report CSV, tolerating the ragged 1-2 field
// metadata preamble rows (FieldsPerRecord = -1) that precede the fixed-width table.
func parseCSVBody(t *testing.T, body string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse CSV body: %v; body:\n%s", err, body)
	}
	return rows
}

// assertBlankSeparatorLine confirms the metadata preamble ends with a blank line
// (an empty CSV record) before the table -- the visual separation the owner asked
// for. csv.Reader silently skips blank lines, so we check the raw body.
func assertBlankSeparatorLine(t *testing.T, body string) {
	t.Helper()
	for _, ln := range strings.Split(body, "\n") {
		if strings.Trim(ln, "\r") == "" {
			return
		}
	}
	t.Errorf("CSV body has no blank separator line between preamble and table; body:\n%s", body)
}

// TestReportPermissionThroughGrant: a report route enforces its group grant like any
// registry route -- a user WITH the group grant gets 200, a user WITHOUT gets 403.
// This is what "appears in the matrix automatically" gives at the HTTP level for the
// concrete trial-balance report (routes_test.go's matrix asserts it across all
// personas; this pins it explicitly).
func TestReportPermissionThroughGrant(t *testing.T) {
	h, st, db, sm := reportsApp(t)

	granted := mkUser(t, st, "granted", "none", false)
	grantGroup(t, db, granted, "financial") // the trial-balance report's group
	ungranted := mkUser(t, st, "ungranted", "none", false)

	rec := asUser(t, h, sm, granted, http.MethodGet, "/reports/"+reports.TrialBalanceReportID, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("granted user status = %d, want 200", rec.Code)
	}
	rec = asUser(t, h, sm, ungranted, http.MethodGet, "/reports/"+reports.TrialBalanceReportID, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ungranted user status = %d, want 403", rec.Code)
	}
}

// p15.12 reports index (/reports, AnyUser): the page lists ONLY the report groups
// and reports the current user may access, grouped by report group, each a link to
// /reports/{id}. Filtering reuses the enforcement path (decide + grantChecker) so the
// listing can never drift from what the concrete report routes actually allow.

// reportHref is the exact index link for a report id (used with a trailing quote so a
// substring check can't false-positive: /reports/functional_expenses must not satisfy
// a search for /reports/fund_activity, etc.).
func reportHref(id string) string { return `href="/reports/` + id + `"` }

// TestReportsIndexAdminSeesAll: an admin (is_admin implies every group, D10) sees the
// index with EVERY registered report as a link -- the "no dead links" guarantee: each
// id in the registry appears as an /reports/{id} href. The four group section labels
// are present too.
func TestReportsIndexAdminSeesAll(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin index status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Every registered report is a link (no dead links; the index and the registry
	// can't disagree). Admin reaches every group, so ALL ids must appear.
	for _, rep := range reports.Default().All() {
		if !strings.Contains(body, reportHref(rep.ID)) {
			t.Errorf("admin index missing link for report %q (%s)", rep.ID, reportHref(rep.ID))
		}
	}
	// All four group section labels are present (each group has >=1 report).
	for _, g := range reports.Groups() {
		if !strings.Contains(body, i18n.T("en", "reports.group."+g)) {
			t.Errorf("admin index missing group section label for %q", g)
		}
	}
}

// TestReportsIndexGrantFiltersGroups: a user granted ONLY "financial" sees the
// financial reports and NOT the funds/programs/tax reports -- proving the index
// filters by the SAME grant resolution the routes enforce.
func TestReportsIndexGrantFiltersGroups(t *testing.T) {
	h, st, db, sm := reportsApp(t)

	u := mkUser(t, st, "fin", "none", false)
	grantGroup(t, db, u, "financial")

	rec := asUser(t, h, sm, u, http.MethodGet, "/reports", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("financial-only index status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// A financial report (trial balance) IS listed; its group label is shown.
	if !strings.Contains(body, reportHref(reports.TrialBalanceReportID)) {
		t.Errorf("financial-only index missing trial_balance link")
	}
	if !strings.Contains(body, i18n.T("en", "reports.group.financial")) {
		t.Errorf("financial-only index missing the financial group label")
	}

	// Reports in the OTHER groups (funds/programs/tax/reconciliation/budget) must NOT
	// appear: fund_activity (funds), program_statement (programs), functional_expenses +
	// form_990 (tax), reconciliation_statement (reconciliation), cashflow_projection +
	// budget_variance (budget).
	for _, id := range []string{
		"fund_activity", "program_statement", "functional_expenses", "form_990",
		reports.ReconciliationStatementReportID,
		reports.CashflowProjectionReportID, reports.BudgetVarianceReportID,
	} {
		if strings.Contains(body, reportHref(id)) {
			t.Errorf("financial-only index wrongly lists non-financial report %q", id)
		}
	}
	// And the other group section labels are absent (a section renders only when it
	// has >=1 permitted report).
	for _, g := range []string{"funds", "programs", "tax", "reconciliation", "budget"} {
		if strings.Contains(body, i18n.T("en", "reports.group."+g)) {
			t.Errorf("financial-only index wrongly shows the %q group section", g)
		}
	}
}

// TestReportsIndexReconciliationGrant: a user granted ONLY "reconciliation" sees the
// reconciliation statement report (and its group section) and NOT the financial
// reports -- the p16.4 new group flows through the SAME index/grant mechanism as the
// Phase-15 groups (auto-covered, asserted once).
func TestReportsIndexReconciliationGrant(t *testing.T) {
	h, st, db, sm := reportsApp(t)

	u := mkUser(t, st, "recon", "none", false)
	grantGroup(t, db, u, "reconciliation")

	rec := asUser(t, h, sm, u, http.MethodGet, "/reports", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconciliation-only index status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, reportHref(reports.ReconciliationStatementReportID)) {
		t.Errorf("reconciliation-only index missing reconciliation_statement link")
	}
	if !strings.Contains(body, i18n.T("en", "reports.group.reconciliation")) {
		t.Errorf("reconciliation-only index missing the reconciliation group label")
	}
	// A financial report must NOT appear (grant is reconciliation-only).
	if strings.Contains(body, reportHref(reports.TrialBalanceReportID)) {
		t.Errorf("reconciliation-only index wrongly lists trial_balance (financial)")
	}
}

// TestReportsIndexBudgetGrant: a user granted ONLY "budget" sees the two budget
// reports (and their group section) and NOT the financial reports -- the p19.4 new
// group flows through the SAME index/grant mechanism as the earlier groups.
func TestReportsIndexBudgetGrant(t *testing.T) {
	h, st, db, sm := reportsApp(t)

	u := mkUser(t, st, "budgeter", "none", false)
	grantGroup(t, db, u, "budget")

	rec := asUser(t, h, sm, u, http.MethodGet, "/reports", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("budget-only index status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, reportHref(reports.CashflowProjectionReportID)) {
		t.Errorf("budget-only index missing cashflow_projection link")
	}
	if !strings.Contains(body, reportHref(reports.BudgetVarianceReportID)) {
		t.Errorf("budget-only index missing budget_variance link")
	}
	if !strings.Contains(body, i18n.T("en", "reports.group.budget")) {
		t.Errorf("budget-only index missing the budget group label")
	}
	// A financial report must NOT appear (grant is budget-only).
	if strings.Contains(body, reportHref(reports.TrialBalanceReportID)) {
		t.Errorf("budget-only index wrongly lists trial_balance (financial)")
	}
}

// TestReportsIndexNoGrantsEmpty: a non-admin with NO grants gets a 200 index (not a
// 403) with the empty-state message and ZERO report links -- the page itself is
// AnyUser; it filters its contents, so an ungranted user lands on an empty list, not
// an error.
func TestReportsIndexNoGrantsEmpty(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	u := mkUser(t, st, "nogrants", "none", false)

	rec := asUser(t, h, sm, u, http.MethodGet, "/reports", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-grants index status = %d, want 200 (not 403)", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, i18n.T("en", "reports.index.empty")) {
		t.Errorf("no-grants index missing the empty-state message")
	}
	// No report is linked (every registered id absent).
	for _, rep := range reports.Default().All() {
		if strings.Contains(body, reportHref(rep.ID)) {
			t.Errorf("no-grants index wrongly lists report %q", rep.ID)
		}
	}
}

// TestReportsIndexAnonRedirects: the index is not public -- an anonymous request is
// bounced to /login (302), like every AnyUser route.
func TestReportsIndexAnonRedirects(t *testing.T) {
	h, _, _, _ := reportsApp(t)

	rec := asUser(t, h, nil, 0, http.MethodGet, "/reports", nil)
	if rec.Code != http.StatusFound {
		t.Errorf("anon index status = %d, want 302 redirect to login", rec.Code)
	}
}

// TestAccountLedgerReportRenders: the account-ledger report (p15.6) renders its
// account SELECTOR (the report-specific param), and with an account + period chosen it
// prints the opening/closing balances, the in-range line, its FUND column, and a LINE
// LINK to the transaction editor (/transactions/{id}/edit, the p12.4 link mechanism
// via Cell.TxnID). Proves the account-param plumbing and the line-link renderer are
// wired end to end through the real route.
func TestAccountLedgerReportRenders(t *testing.T) {
	h, st, _, sm := reportsApp(t)
	admin := mkUser(t, st, "admin", "none", true)

	// The bare report page shows the account selector (report-specific control) and,
	// with no account chosen, an empty table (200).
	rec := asUser(t, h, sm, admin, http.MethodGet, "/reports/"+reports.AccountLedgerReportID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account ledger status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="rp-account"`) {
		t.Errorf("account ledger page missing the account selector")
	}
	// p28.2: the account selector is now a fuzzy hierarchy combobox (combo-input).
	if !strings.Contains(rec.Body.String(), `report-account-select combo-input`) {
		t.Errorf("account selector is not a combo-input")
	}

	// The Cash account id (seeded by reportsApp) via the account tree.
	cash := accountIDByName(t, st, "Cash")

	// Run the ledger for Cash over a range covering the seeded 2025-06-01 +250.00 posting.
	url := "/reports/" + reports.AccountLedgerReportID +
		"?account=" + itoa(int64(cash)) + "&from=2025-06-01&to=2025-06-30"
	rec = asUser(t, h, sm, admin, http.MethodGet, url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account ledger (Cash) status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The formatted seeded amount (+250.00) appears on the line and the closing balance
	// with the per-currency symbol (USD prefix "$", p26.24).
	if !strings.Contains(body, "$250.00") {
		t.Errorf("account ledger missing the $250.00 line/balance; body:\n%s", body)
	}
	// The line links to the transaction editor (Cell.TxnID -> /transactions/{id}/edit).
	if !strings.Contains(body, "/transactions/") || !strings.Contains(body, "/edit") {
		t.Errorf("account ledger line missing the txn-editor link; body:\n%s", body)
	}
	// The opening (subtotal) and closing (total) framing rows render.
	if !strings.Contains(body, "report-subtotal") || !strings.Contains(body, "report-total") {
		t.Errorf("account ledger missing opening/closing framing rows")
	}
	// The unrestricted seeded split shows the "Unrestricted" fund label.
	if !strings.Contains(body, "Unrestricted") {
		t.Errorf("account ledger missing the Unrestricted fund label")
	}
}

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p20.2 submitter-workspace handler tests. Driven through the REAL mounted router
// (httptest) against a real migrated temp db (AGENTS testing conventions) -- no
// store mocks. They prove: a pure submitter (can_submit_expenses, txn_perm=none)
// creates a report, adds UNBALANCED R/E lines, SUBMITS successfully (no balance
// requirement), and after a store-set rejection sees the reviewer's REASON and
// RESUBMITS; the ACCESS BOUNDARY (a submitter 403s on the ledger/reports/admin);
// OWNERSHIP (submitter A cannot open submitter B's report -> 404); the PERM gate
// (a non-submitter 403s on the expense routes, a submitter 200s); and validation
// (zero-line submit -> 422; an out-of-sub / non-existent account line -> field error).

// mkSubmitter creates a pure submitter (txn_perm=none + can_submit_expenses=true) and
// returns its id.
func mkSubmitter(t *testing.T, st *store.Store, username string) ids.UserID {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateUser(ctx, store.CreateUserInput{
		Username: username, DisplayName: username, TxnPerm: "none",
	})
	if err != nil {
		t.Fatalf("create submitter %s: %v", username, err)
	}
	if err := st.SetUserCanSubmitExpenses(ctx, id, true); err != nil {
		t.Fatalf("grant can_submit_expenses to %s: %v", username, err)
	}
	return id
}

// seedREAccount creates a leaf revenue/expense account in the root subsidiary and
// returns its id (for expense-report line data).
func seedREAccount(t *testing.T, st *store.Store, typ, name string) ids.AccountID {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: typ, DefaultCurrency: "USD",
		Names: map[string]string{"en": name}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("seed %s account %s: %v", typ, name, err)
	}
	return id
}

// TestExpenseSubsidiaryAndDiscard (p25.3): the subsidiary picker shows on a draft with
// no lines and changes the report's sub in-page; it disappears once a line exists; and
// a draft is discardable (hard-deleted) from the report page.
func TestExpenseSubsidiaryAndDiscard(t *testing.T) {
	h, st, sm := accountsApp(t)
	sub := mkSubmitter(t, st, "sub_disc")
	sysCtx := store.WithActor(context.Background(), store.Actor{ID: 1})
	sub2, err := st.CreateSubsidiary(sysCtx, store.CreateSubsidiaryInput{ParentID: 1, Name: "Branch", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("CreateSubsidiary: %v", err)
	}
	// A revenue account mapped to sub2 (propagates to the root, D18), so a line can be
	// added while the report is in sub2.
	rev, err := st.CreateAccount(sysCtx, store.CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Rev Branch"}, Subsidiaries: []ids.SubsidiaryID{sub2},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// New report (empty POST -> draft in the submitter's default subsidiary).
	rec := asUser(t, h, sm, sub, http.MethodPost, "/expenses", url.Values{})
	repID := reportIDFromRedirect(t, rec)

	// The draft detail shows the in-page subsidiary picker (editable + no lines).
	pickerAction := `action="/expenses/` + itoa(repID) + `/subsidiary"`
	body := asUser(t, h, sm, sub, http.MethodGet, "/expenses/"+itoa(repID), nil).Body.String()
	if !strings.Contains(body, pickerAction) {
		t.Errorf("draft detail missing the subsidiary picker:\n%s", body)
	}

	// Change the subsidiary in-page.
	asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+itoa(repID)+"/subsidiary", url.Values{"subsidiary_id": {itoa(int64(sub2))}})
	if rep, _ := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(repID)); rep.SubsidiaryID != sub2 {
		t.Fatalf("subsidiary after change = %d, want %d", rep.SubsidiaryID, sub2)
	}

	// Add a line -> the sub is now locked, so the picker disappears.
	addLine(t, h, st, sm, sub, repID, rev, "50.00")
	body = asUser(t, h, sm, sub, http.MethodGet, "/expenses/"+itoa(repID), nil).Body.String()
	if strings.Contains(body, pickerAction) {
		t.Errorf("subsidiary picker should be gone once a line exists:\n%s", body)
	}

	// Discard the draft (with its line) -> redirect to /expenses; the report is gone.
	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+itoa(repID)+"/discard", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/expenses" {
		t.Fatalf("discard = %d -> %q, want 303 -> /expenses", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(repID)); err == nil {
		t.Errorf("report still exists after discard (want deleted)")
	}
}

// TestExpenseUnbalancedSubmitOK: a submitter creates a report, adds an UNBALANCED set
// of R/E lines (a revenue and an expense whose magnitudes do NOT net to zero), and
// SUBMITS -- the submit succeeds (200/redirect) and the report shows "submitted". This
// is the core no-balance assertion.
func TestExpenseUnbalancedSubmitOK(t *testing.T) {
	h, st, sm := accountsApp(t)
	sub := mkSubmitter(t, st, "sub_ok")
	rev := seedREAccount(t, st, "revenue", "Grants Rev")
	exp := seedREAccount(t, st, "expense", "Travel Exp")

	// Create the report.
	rec := asUser(t, h, sm, sub, http.MethodPost, "/expenses", url.Values{"subsidiary_id": {"1"}})
	repID := reportIDFromRedirect(t, rec)

	// Add two UNBALANCED lines: revenue 100.00 + expense 30.00 (net != 0).
	addLine(t, h, st, sm, sub, repID, rev, "100.00")
	addLine(t, h, st, sm, sub, repID, exp, "30.00")

	// Confirm the store sees an unbalanced set (sanity: the sum is non-zero).
	lines, err := st.ExpenseReportLines(context.Background(), ids.ExpenseReportID(repID))
	if err != nil {
		t.Fatalf("lines: %v", err)
	}
	var sum int64
	for _, l := range lines {
		sum += l.Amount
	}
	if sum == 0 {
		t.Fatalf("test setup: lines net to zero (%d); want an UNBALANCED report", sum)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}

	// Submit -- must succeed despite the imbalance.
	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+itoa(repID)+"/submit", url.Values{})
	if rec.Code == http.StatusUnprocessableEntity || rec.Code >= 500 {
		t.Fatalf("unbalanced submit failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rep, err := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(repID))
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if rep.Status != "submitted" {
		t.Fatalf("status after submit = %q, want submitted", rep.Status)
	}

	// The detail page shows the "submitted" status and is READ-ONLY (no add-line form).
	rec = asUser(t, h, sm, sub, http.MethodGet, "/expenses/"+itoa(repID), nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Submitted") {
		t.Errorf("detail does not show Submitted status; body: %s", body)
	}
	if strings.Contains(body, `id="expense-submit"`) {
		t.Errorf("submitted report still shows a submit button (should be read-only)")
	}
}

// TestExpenseRejectShowsReasonAndResubmit: after a store-set rejection (simulating the
// p20.3 reviewer), the detail page shows the reviewer's REASON, and the submitter can
// edit + RESUBMIT, returning the report to "submitted".
func TestExpenseRejectShowsReasonAndResubmit(t *testing.T) {
	h, st, sm := accountsApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	sub := mkSubmitter(t, st, "sub_reject")
	exp := seedREAccount(t, st, "expense", "Meals Exp")

	rec := asUser(t, h, sm, sub, http.MethodPost, "/expenses", url.Values{"subsidiary_id": {"1"}})
	repID := reportIDFromRedirect(t, rec)
	addLine(t, h, st, sm, sub, repID, exp, "50.00")
	if rec := asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+itoa(repID)+"/submit", url.Values{}); rec.Code >= 400 {
		t.Fatalf("submit: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The reviewer rejects with a reason (via the store, as p20.3 will).
	const reason = "Please attach the receipt."
	if err := st.RejectExpenseReport(ctx, ids.ExpenseReportID(repID), reason); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// The submitter's detail page shows the reason + a resubmit affordance.
	rec = asUser(t, h, sm, sub, http.MethodGet, "/expenses/"+itoa(repID), nil)
	body := rec.Body.String()
	if !strings.Contains(body, reason) {
		t.Errorf("reviewer reason %q not shown on detail; body: %s", reason, body)
	}
	if !strings.Contains(body, `id="expense-resubmit"`) {
		t.Errorf("rejected report has no resubmit affordance; body: %s", body)
	}

	// The submitter resubmits (after a notional edit); the report returns to submitted.
	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+itoa(repID)+"/resubmit", url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("resubmit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rep, err := st.GetExpenseReport(ctx, ids.ExpenseReportID(repID))
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if rep.Status != "submitted" {
		t.Errorf("status after resubmit = %q, want submitted", rep.Status)
	}
}

// TestExpenseOwnershipBoundary: submitter A cannot open (or mutate) submitter B's
// report -- a 404 (uniform with a missing id, no enumeration).
func TestExpenseOwnershipBoundary(t *testing.T) {
	h, st, sm := accountsApp(t)
	subA := mkSubmitter(t, st, "sub_a")
	subB := mkSubmitter(t, st, "sub_b")

	// B creates a report.
	rec := asUser(t, h, sm, subB, http.MethodPost, "/expenses", url.Values{"subsidiary_id": {"1"}})
	repB := reportIDFromRedirect(t, rec)

	// A tries to open B's report -> 404.
	rec = asUser(t, h, sm, subA, http.MethodGet, "/expenses/"+itoa(repB), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("A opening B's report: status=%d, want 404", rec.Code)
	}
	// A tries to save lines to B's report (the bulk grid save) -> 404.
	rec = asUser(t, h, sm, subA, http.MethodPost, "/expenses/"+itoa(repB)+"/lines", url.Values{"rows": {"0"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("A saving lines to B's report: status=%d, want 404", rec.Code)
	}
	// A tries to submit B's report -> 404.
	rec = asUser(t, h, sm, subA, http.MethodPost, "/expenses/"+itoa(repB)+"/submit", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("A submitting B's report: status=%d, want 404", rec.Code)
	}
	// A wholly-nonexistent id is also a 404 (uniform).
	rec = asUser(t, h, sm, subA, http.MethodGet, "/expenses/99999", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("nonexistent report: status=%d, want 404", rec.Code)
	}
}

// TestExpenseAccessBoundary: a pure submitter is 403 on the ledger, reports index, and
// admin -- the routes a submitter must NOT reach (the access boundary, asserted
// explicitly beyond the auto permission matrix).
func TestExpenseAccessBoundary(t *testing.T) {
	h, st, sm := accountsApp(t)
	sub := mkSubmitter(t, st, "sub_boundary")

	// The ledger, budget plans, import, and admin are hard 403 for a pure submitter
	// (perm gate). A concrete REPORT route is 403 too (an ungranted user). The /reports
	// INDEX is AnyUser by design (it filters its contents by grant), so it is checked
	// separately below -- it must show NO report links for a submitter.
	for _, path := range []string{"/accounts", "/funds", "/programs", "/budget-plans", "/import", "/admin", "/admin/users", "/reports/trial_balance"} {
		rec := asUser(t, h, sm, sub, http.MethodGet, path, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("submitter GET %s: status=%d, want 403 (access boundary)", path, rec.Code)
		}
	}
	// The reports index is reachable (AnyUser) but shows NOTHING to an ungranted
	// submitter -- no concrete report link leaks.
	rec := asUser(t, h, sm, sub, http.MethodGet, "/reports", nil)
	if strings.Contains(rec.Body.String(), "/reports/trial_balance") {
		t.Errorf("submitter's reports index leaks a report link; body: %s", rec.Body.String())
	}
	// But the submitter's OWN workspace is reachable (200).
	rec = asUser(t, h, sm, sub, http.MethodGet, "/expenses", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("submitter GET /expenses: status=%d, want 200", rec.Code)
	}
}

// TestExpensePermGate: a non-submitter (no flag, txn_perm=write) is 403 on the expense
// routes; a submitter is 200. Proves the ExpenseSubmit gate is independent of txn_perm.
func TestExpensePermGate(t *testing.T) {
	h, st, sm := accountsApp(t)
	bookkeeper := mkUser(t, st, "book_noexp", "write", false) // txn_perm=write, NO submit flag
	sub := mkSubmitter(t, st, "sub_gate")

	rec := asUser(t, h, sm, bookkeeper, http.MethodGet, "/expenses", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bookkeeper (no submit flag) GET /expenses: status=%d, want 403", rec.Code)
	}
	rec = asUser(t, h, sm, sub, http.MethodGet, "/expenses", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("submitter GET /expenses: status=%d, want 200", rec.Code)
	}
}

// TestExpenseZeroLineSubmit422: submitting a report with zero lines is a clean 422
// (i18n), not a 500.
func TestExpenseZeroLineSubmit422(t *testing.T) {
	h, st, sm := accountsApp(t)
	sub := mkSubmitter(t, st, "sub_empty")
	rec := asUser(t, h, sm, sub, http.MethodPost, "/expenses", url.Values{"subsidiary_id": {"1"}})
	repID := reportIDFromRedirect(t, rec)

	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+itoa(repID)+"/submit", url.Values{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("zero-line submit: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "error.expense") {
		t.Errorf("raw i18n key leaked (not localized): %s", rec.Body.String())
	}
	// The report stays a draft.
	rep, _ := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(repID))
	if rep.Status != "draft" {
		t.Errorf("status after failed submit = %q, want draft", rep.Status)
	}
}

// TestExpenseLineOutOfSubIsFieldError: a line on a non-existent account (0) and a line
// on an out-of-subsidiary / non-R-E account each produce a 422 field error, not a 500
// or a silent accept.
func TestExpenseLineBadAccountFieldError(t *testing.T) {
	h, st, sm := accountsApp(t)
	sub := mkSubmitter(t, st, "sub_badacct")
	// An ASSET account (not R/E) is never offered -> out-of-set for a report line.
	asset := seedREAccount(t, st, "asset", "Cash Asset")

	rec := asUser(t, h, sm, sub, http.MethodPost, "/expenses", url.Values{"subsidiary_id": {"1"}})
	repID := itoa(reportIDFromRedirect(t, rec))

	// A non-existent account id in a grid row -> per-row error (422).
	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+repID+"/lines", url.Values{
		"rows": {"1"}, "account_0": {"88888"}, "amount_0": {"10.00"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("nonexistent-account row: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// A non-R/E (asset) account in a grid row -> per-row error (422).
	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+repID+"/lines", url.Values{
		"rows": {"1"}, "account_0": {itoa(int64(asset))}, "amount_0": {"10.00"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("non-R/E account row: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "field-error") {
		t.Errorf("expected a field-error in the re-rendered grid; body: %s", rec.Body.String())
	}

	// The 422 re-render ECHOES the user's typed input (it does not reload the persisted
	// set): a valid revenue row's distinctive amount survives alongside the bad row.
	rev := seedREAccount(t, st, "revenue", "Grants Rev Echo")
	rec = asUser(t, h, sm, sub, http.MethodPost, "/expenses/"+repID+"/lines", url.Values{
		"rows":      {"2"},
		"account_0": {itoa(int64(rev))}, "amount_0": {"77.77"},
		"account_1": {itoa(int64(asset))}, "amount_1": {"5.00"}, // asset -> row error
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mixed valid/invalid grid: status=%d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "77.77") {
		t.Errorf("422 re-render dropped the user's typed amount (77.77); body: %s", rec.Body.String())
	}
}

// TestAdminCanSubmitToggle: an admin toggles a user's can_submit_expenses via the
// user-detail form; the user then reaches /expenses (200) where before they 403'd.
// This is the p20.1-deferred admin UI (and the e2e's submitter-seeding path).
func TestAdminCanSubmitToggle(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "adm", "none", true)
	target := mkUser(t, st, "worker", "none", false) // no submit flag yet

	// Before: the target 403s on /expenses.
	if rec := asUser(t, h, sm, target, http.MethodGet, "/expenses", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("target before grant: status=%d, want 403", rec.Code)
	}
	// Admin grants can_submit_expenses.
	rec := asUser(t, h, sm, admin, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/can-submit", url.Values{
		"can_submit_expenses": {"1"},
	})
	if rec.Code >= 400 {
		t.Fatalf("admin grant: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// After: the target reaches /expenses (200).
	if rec := asUser(t, h, sm, target, http.MethodGet, "/expenses", nil); rec.Code != http.StatusOK {
		t.Errorf("target after grant: status=%d, want 200", rec.Code)
	}
	// The store reflects the versioned change.
	cu, err := st.UserByID(context.Background(), target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !cu.CanSubmitExpenses {
		t.Errorf("CanSubmitExpenses = false after admin grant")
	}
	// A non-admin cannot use the toggle (403).
	if rec := asUser(t, h, sm, target, http.MethodPost, "/admin/users/"+itoa(int64(target))+"/can-submit", url.Values{"can_submit_expenses": {"1"}}); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin using toggle: status=%d, want 403", rec.Code)
	}
}

// --- small helpers --------------------------------------------------------

// reportIDFromRedirect extracts the new report id from a POST /expenses redirect
// (HX-Redirect header for htmx, else the 303 Location). Fails the test if absent.
func reportIDFromRedirect(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	loc := rec.Header().Get("HX-Redirect")
	if loc == "" {
		loc = rec.Header().Get("Location")
	}
	if loc == "" {
		t.Fatalf("POST /expenses gave no redirect Location (status=%d)", rec.Code)
	}
	parts := strings.Split(strings.TrimPrefix(loc, "/expenses/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("bad report redirect %q: %v", loc, err)
	}
	return id
}

// addLine APPENDS a line (positive magnitude) to a report via the p25.4 bulk grid save.
// Because /expenses/{id}/lines is a REPLACE-SET (the whole line set posts at once), the
// helper first reads the report's current lines and re-posts them (round-tripping each
// line_id + its positive display magnitude) alongside the new row, so calling addLine
// repeatedly accumulates lines like the old one-at-a-time form did. Fails on a non-redirect.
func addLine(t *testing.T, h http.Handler, st *store.Store, sm *scs.SessionManager, submitter ids.UserID, reportID int64, account ids.AccountID, amount string) {
	t.Helper()
	form := gridFormFromLines(t, st, reportID)
	idx := existingRowCount(t, st, reportID)
	si := itoa64(idx)
	form.Set("account_"+si, itoa(int64(account)))
	form.Set("amount_"+si, amount)
	form.Set("rows", itoa64(idx+1))
	rec := asUser(t, h, sm, submitter, http.MethodPost, "/expenses/"+itoa(reportID)+"/lines", form)
	if rec.Code >= 400 {
		t.Fatalf("add line (acct %d, amt %s): status=%d body=%s", account, amount, rec.Code, rec.Body.String())
	}
}

// gridFormFromLines builds the bulk-grid POST body echoing a report's CURRENT lines
// (each line_id round-tripped + its positive display magnitude), so a subsequent append
// preserves them. Row order == store order; the returned form has `rows` set to the
// existing count (callers bump it when appending).
func gridFormFromLines(t *testing.T, st *store.Store, reportID int64) url.Values {
	t.Helper()
	lines, err := st.ExpenseReportLines(context.Background(), ids.ExpenseReportID(reportID))
	if err != nil {
		t.Fatalf("gridFormFromLines: %v", err)
	}
	form := url.Values{}
	for i, l := range lines {
		si := itoa64(int64(i))
		form.Set("line_id_"+si, itoa64(int64(l.ID)))
		form.Set("account_"+si, itoa64(int64(l.AccountID)))
		mag := l.Amount
		if mag < 0 {
			mag = -mag
		}
		// The seed/tests use 2-exponent USD amounts; format the positive magnitude plainly.
		form.Set("amount_"+si, minorToPlain(mag))
		if l.FundID.Valid {
			form.Set("fund_"+si, itoa64(l.FundID.Int64))
		}
		if l.ProgramID.Valid {
			form.Set("program_"+si, itoa64(l.ProgramID.Int64))
		}
		form.Set("memo_"+si, l.Memo)
	}
	form.Set("rows", itoa64(int64(len(lines))))
	return form
}

func existingRowCount(t *testing.T, st *store.Store, reportID int64) int64 {
	t.Helper()
	lines, err := st.ExpenseReportLines(context.Background(), ids.ExpenseReportID(reportID))
	if err != nil {
		t.Fatalf("existingRowCount: %v", err)
	}
	return int64(len(lines))
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// minorToPlain formats a non-negative 2-exponent minor amount as a plain decimal string
// (e.g. 10000 -> "100.00") for the test grid POST bodies.
func minorToPlain(minor int64) string {
	return strconv.FormatInt(minor/100, 10) + "." +
		leftPad2(strconv.FormatInt(minor%100, 10))
}

func leftPad2(s string) string {
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

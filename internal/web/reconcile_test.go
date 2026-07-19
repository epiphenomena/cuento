package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/ids"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// p16.3 reconciliation workspace handler tests. Driven through the REAL mounted
// router (httptest) against a real migrated db (AGENTS testing conventions) -- no
// handler-level store mocks. Each test builds its own reconcilable account + splits
// inline (bespoke data, testing conventions) so it does not depend on the fixture.

// reconWebEnv is a small chart for the workspace tests: an app, store, session
// manager, a write-capable + a read-only user, a reconcilable Checking account, and
// two posted USD splits on it (a +250 deposit and a -400 expense).
type reconWebEnv struct {
	h  http.Handler
	st *store.Store
	sm *scs.SessionManager

	writer int64
	reader int64

	checking int64
	spDep    int64 // +250 checking split
	spExp    int64 // -400 checking split
	txnDep   int64 // the deposit transaction (owns spDep)
	txnExp   int64 // the expense transaction (owns spExp)
}

func newReconWebEnv(t *testing.T) reconWebEnv {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)

	writer := mkUser(t, st, "rwriter", "write", false)
	reader := mkUser(t, st, "rreader", "read", false)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	checking, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Checking US"},
		Subsidiaries: []int64{1}, Reconcilable: true,
	})
	if err != nil {
		t.Fatalf("create checking: %v", err)
	}
	revenue, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD", Names: map[string]string{"en": "Grants"},
		Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	mgmt := "management"
	expense, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Supplies"},
		Subsidiaries: []int64{1}, FunctionalClass: &mgmt,
	})
	if err != nil {
		t.Fatalf("create expense: %v", err)
	}

	cls := "management"
	txnDep, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2026-02-05", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: checking, Amount: 25_000, Position: 0},
			{AccountID: revenue, Amount: -25_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post deposit: %v", err)
	}
	txnExp, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2026-02-08", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: checking, Amount: -40_000, Position: 0},
			{AccountID: expense, Amount: 40_000, Position: 1, FunctionalClass: &cls},
		},
	})
	if err != nil {
		t.Fatalf("post expense: %v", err)
	}

	return reconWebEnv{
		h: app.handler, st: st, sm: app.sessions,
		writer: writer, reader: reader, checking: checking,
		spDep:  splitOnAccount(t, db, txnDep, checking),
		spExp:  splitOnAccount(t, db, txnExp, checking),
		txnDep: txnDep, txnExp: txnExp,
	}
}

// splitOnAccount returns the id of the split on `account` within `txn` (test helper,
// direct SQL against the throwaway db).
func splitOnAccount(t *testing.T, db *sql.DB, txn, account int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM splits WHERE transaction_id = ? AND account_id = ?`, txn, account).Scan(&id); err != nil {
		t.Fatalf("splitOnAccount(txn %d, acct %d): %v", txn, account, err)
	}
	return id
}

// startRecon starts a recon on Checking (statement balance 0 by default) and returns
// its id, using the store directly (the list-start path is exercised separately).
func (e reconWebEnv) startRecon(t *testing.T, statement int64) ids.ReconciliationID {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: e.writer})
	id, err := e.st.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", statement)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	return id
}

// --- LIST + WORKSPACE render (TxnRead) -----------------------------------

func TestReconListShowsReconcilableAccount(t *testing.T) {
	e := newReconWebEnv(t)
	rec := asUser(t, e.h, e.sm, e.reader, http.MethodGet, "/reconciliations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reconciliations = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Checking US") {
		t.Errorf("list missing reconcilable account Checking US; body:\n%s", rec.Body.String())
	}
}

func TestReconWorkspaceRendersSplits(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0)
	rec := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET workspace = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Both uncleared splits appear with a per-split toggle control carrying a stable id.
	if !strings.Contains(body, reconToggleID(e.spDep)) {
		t.Errorf("workspace missing toggle for deposit split; body:\n%s", body)
	}
	if !strings.Contains(body, reconToggleID(e.spExp)) {
		t.Errorf("workspace missing toggle for expense split")
	}
	// The sticky summary + difference chip is present.
	if !strings.Contains(body, `id="recon-summary"`) {
		t.Errorf("workspace missing sticky summary region")
	}
	// p26.50: the "Add transaction" button links to the editor with a `from` origin back
	// to THIS workspace (so Cancel/Save return here).
	wantAdd := `href="/transactions/new?from=/reconciliations/` + strconv.FormatInt(int64(id), 10) + `"`
	if !strings.Contains(body, wantAdd) {
		t.Errorf("workspace missing Add-transaction link %q; body:\n%s", wantAdd, body)
	}
	// p26.50: each split row carries an Edit link to its transaction, carrying the same
	// `from` origin. Assert the deposit split's transaction edit link is present.
	wantEdit := `href="/transactions/` + strconv.FormatInt(e.txnDep, 10) + `/edit?from=/reconciliations/` + strconv.FormatInt(int64(id), 10) + `"`
	if !strings.Contains(body, wantEdit) {
		t.Errorf("workspace missing per-row Edit link %q; body:\n%s", wantEdit, body)
	}
}

// TestReconOriginDest: Save's recon-origin narrowing (p26.50). A bare
// /reconciliations/{id} workspace path is returned verbatim (Save lands there); any
// other same-site origin yields "" so Save keeps its default register destination.
func TestReconOriginDest(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/reconciliations/7", "/reconciliations/7"},
		{"/reconciliations/42", "/reconciliations/42"},
		{"/reconciliations", ""},            // the list, not a workspace
		{"/reconciliations/", ""},           // empty id
		{"/reconciliations/7/finalize", ""}, // a sub-route, not the bare workspace
		{"/reconciliations/abc", ""},        // non-numeric id
		{"/accounts/3/register", ""},        // an unrelated (register) origin
		{"", ""},                            // no origin
	}
	for _, c := range cases {
		if got := reconOriginDest(c.in); got != c.want {
			t.Errorf("reconOriginDest(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- TOGGLE = targeted PARTIAL swap, no full reload ----------------------

// TestToggleReturnsPartialAndUpdatesDifference: a toggle POST flips the split's
// cleared state, returns a PARTIAL (the flipped row + an OOB summary swap), NOT a
// full document, and the difference reflects the new cleared total.
func TestToggleReturnsPartialAndUpdatesDifference(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0) // statement 0; opening 0 => difference starts at 0

	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconTogglePath(int64(id), e.spDep), url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle POST = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// PARTIAL, not a full page: no <html> / no nav landmark.
	if strings.Contains(body, "<html") || strings.Contains(body, "app-nav") {
		t.Errorf("toggle response looks like a full document (should be a partial):\n%s", body)
	}
	// The flipped row is returned (the toggle now reads "clear off" / checked state).
	if !strings.Contains(body, reconToggleID(e.spDep)) {
		t.Errorf("toggle response missing the flipped row")
	}
	// The OOB summary swap is present (recon-summary with hx-swap-oob).
	if !strings.Contains(body, `id="recon-summary"`) || !strings.Contains(body, "hx-swap-oob") {
		t.Errorf("toggle response missing the OOB summary swap; body:\n%s", body)
	}

	// The split is now cleared in the store (persisted).
	ws, err := e.st.ReconciliationWorkspaceSplits(context.Background(), id)
	if err != nil {
		t.Fatalf("workspace splits: %v", err)
	}
	var cleared bool
	for _, w := range ws {
		if w.SplitID == e.spDep {
			cleared = w.Cleared
		}
	}
	if !cleared {
		t.Errorf("deposit split not cleared after toggle")
	}

	// Difference now reflects the +250 cleared: statement 0 - (0 + 250) = -250.
	sum, err := e.st.ReconciliationSummaryFor(context.Background(), id)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Difference != -25_000 {
		t.Errorf("difference after clearing +250 = %d, want -25000", sum.Difference)
	}
	// A second toggle unclears it (round-trips).
	rec2 := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconTogglePath(int64(id), e.spDep), url.Values{})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second toggle = %d, want 200", rec2.Code)
	}
	sum2, _ := e.st.ReconciliationSummaryFor(context.Background(), id)
	if sum2.Cleared != 0 {
		t.Errorf("cleared after unclear = %d, want 0", sum2.Cleared)
	}
}

// --- FINALIZE gating -----------------------------------------------------

// TestFinalizeDisabledUntilZero: the workspace renders Finalize DISABLED at a
// nonzero difference and ENABLED at zero.
func TestFinalizeDisabledUntilZero(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0) // statement 0, nothing cleared => difference 0 => enabled

	body := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil).Body.String()
	if finalizeDisabled(body) {
		t.Errorf("Finalize should be ENABLED at zero difference; body:\n%s", body)
	}

	// Clear the +250 deposit: difference becomes 0 - (0 + 250) = -250 (nonzero).
	ctxW := store.WithActor(context.Background(), store.Actor{ID: e.writer})
	if err := e.st.SetSplitReconciled(ctxW, id, e.spDep, true); err != nil {
		t.Fatalf("clear split: %v", err)
	}
	body2 := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil).Body.String()
	if !finalizeDisabled(body2) {
		t.Errorf("Finalize should be DISABLED at nonzero difference; body:\n%s", body2)
	}
}

// TestFinalizeAtNonzeroRejectedCleanly: a POST finalize at a nonzero difference is a
// clean 422 (guard), not a 500 -- the store's ErrReconciliationDifference mapped.
func TestFinalizeAtNonzeroRejectedCleanly(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 100_000) // statement 100000, opening 0, cleared 0 => diff 100000
	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconFinalizePath(int64(id)), url.Values{})
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("finalize at nonzero difference returned 500 (should be a clean guard); body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("finalize at nonzero difference = %d, want 422", rec.Code)
	}
	// Still open.
	got, _ := e.st.GetReconciliation(context.Background(), id)
	if got.Status != "open" {
		t.Errorf("recon status = %q, want open (finalize rejected)", got.Status)
	}
}

// TestFinalizeAtZeroSucceeds: at a zero difference the finalize goes through and the
// recon is finalized (the finalized recon shows).
func TestFinalizeAtZeroSucceeds(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0) // statement 0, nothing cleared => diff 0
	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconFinalizePath(int64(id)), url.Values{})
	if rec.Code >= 400 {
		t.Fatalf("finalize at zero difference = %d, want redirect/200; body: %s", rec.Code, rec.Body.String())
	}
	got, _ := e.st.GetReconciliation(context.Background(), id)
	if got.Status != "finalized" {
		t.Errorf("recon status = %q, want finalized", got.Status)
	}
}

// --- p16.5 void-block surfaces cleanly (409, not 500) --------------------

// TestVoidReconciledTxnRejectedCleanly: POSTing a confirmed void of a transaction
// whose split is cleared in a FINALIZED recon is a clean 409 with the localized
// banner (store.ErrSplitReconciled mapped), NOT a 500; the txn stays live. After the
// recon is reopened the same void succeeds (redirect). Load-bearing: without the
// store guard + handler arm this void would 500 (or silently drop the split).
func TestVoidReconciledTxnRejectedCleanly(t *testing.T) {
	e := newReconWebEnv(t)
	recon := e.finalizeRecon(t) // clears spDep + spExp, finalizes

	voidPath := "/transactions/" + strconv.FormatInt(e.txnExp, 10) + "/void"
	form := url.Values{}
	form.Set("confirm", "1")

	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, voidPath, form)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("void of reconciled txn returned 500 (should be a clean guard); body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("void of reconciled txn = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	// Localized banner (not the raw i18n key).
	if !strings.Contains(rec.Body.String(), "finalized reconciliation") {
		t.Errorf("void error response missing the localized recon-lock banner; body:\n%s", rec.Body.String())
	}
	// The txn stays live (GetTransaction still returns it).
	if _, err := e.st.GetTransaction(context.Background(), e.txnExp); err != nil {
		t.Errorf("txn should still be live after blocked void: %v", err)
	}

	// After reopening the recon the void succeeds (redirect, not 4xx/5xx).
	if err := e.st.Reopen(store.WithActor(context.Background(), store.Actor{ID: e.writer}), recon); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec2 := asUser(t, e.h, e.sm, e.writer, http.MethodPost, voidPath, form)
	if rec2.Code >= 400 {
		t.Fatalf("void after reopen = %d, want redirect; body: %s", rec2.Code, rec2.Body.String())
	}
	if _, err := e.st.GetTransaction(context.Background(), e.txnExp); err == nil {
		t.Errorf("txn should be voided (not found) after reopen+void")
	}
}

// --- PERMS: TxnRead views, cannot act ------------------------------------

func TestReconPermsReadCannotAct(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0)

	// Reader may VIEW list + workspace.
	if rec := asUser(t, e.h, e.sm, e.reader, http.MethodGet, "/reconciliations", nil); rec.Code != http.StatusOK {
		t.Errorf("reader GET list = %d, want 200", rec.Code)
	}
	if rec := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil); rec.Code != http.StatusOK {
		t.Errorf("reader GET workspace = %d, want 200", rec.Code)
	}

	// Reader may NOT toggle / finalize / reopen (403).
	for _, tc := range []struct {
		name, method, path string
	}{
		{"toggle", http.MethodPost, reconTogglePath(int64(id), e.spDep)},
		{"finalize", http.MethodPost, reconFinalizePath(int64(id))},
		{"reopen", http.MethodPost, reconReopenPath(int64(id))},
		{"start", http.MethodPost, "/reconciliations"},
	} {
		rec := asUser(t, e.h, e.sm, e.reader, tc.method, tc.path, url.Values{})
		if rec.Code != http.StatusForbidden {
			t.Errorf("reader %s = %d, want 403", tc.name, rec.Code)
		}
	}

	// Writer CAN toggle (200).
	if rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconTogglePath(int64(id), e.spDep), url.Values{}); rec.Code != http.StatusOK {
		t.Errorf("writer toggle = %d, want 200", rec.Code)
	}
}

// --- START form error convention (bad balance -> 422 partial) -------------

// TestReconStartFormErrorPartial: a start POST with an unparseable balance returns a
// 422 + the recon-start-form PARTIAL (no full doc), a localized field error (not a
// raw key), and autofocus on the offending field -- the p10.3 form-error convention.
func TestReconStartFormErrorPartial(t *testing.T) {
	e := newReconWebEnv(t)
	form := url.Values{}
	form.Set("account_id", strconv.FormatInt(e.checking, 10))
	form.Set("currency", "USD")
	form.Set("statement_date", "2026-02-28")
	form.Set("balance", "not-a-number")

	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, "/reconciliations", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad-balance start = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// A PARTIAL, not a full document.
	if strings.Contains(body, "<html") || strings.Contains(body, "app-nav") {
		t.Errorf("start error response is a full document (should be the form partial):\n%s", body)
	}
	// The localized balance error resolves (not the raw i18n key) and autofocus lands.
	if !strings.Contains(body, "valid statement balance") {
		t.Errorf("start error missing the localized balance error; body:\n%s", body)
	}
	if !strings.Contains(body, "autofocus") {
		t.Errorf("start error missing autofocus on the invalid field")
	}
	// No reconciliation was created (the POST was rejected before StartReconciliation).
	recs, _ := e.st.ReconciliationsForAccount(context.Background(), e.checking)
	if len(recs) != 0 {
		t.Errorf("a reconciliation was created on a rejected start: %+v", recs)
	}
}

// --- HISTORY (p16.4): finalized recons per account on the list page -------

// finalizeRecon clears both Checking splits and finalizes a recon whose statement
// balance is their net sum (-15,000 = +25,000 - 40,000, opening 0), returning its id.
// Used by the p16.4 history tests to build a FINALIZED recon through the store.
func (e reconWebEnv) finalizeRecon(t *testing.T) ids.ReconciliationID {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: e.writer})
	id := e.startRecon(t, -15_000)
	for _, sp := range []int64{e.spDep, e.spExp} {
		if err := e.st.SetSplitReconciled(ctx, id, sp, true); err != nil {
			t.Fatalf("clear split %d: %v", sp, err)
		}
	}
	if err := e.st.Finalize(ctx, id); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return id
}

// TestReconHistoryListsFinalizedRecon: the /reconciliations list page renders the
// p16.4 HISTORY section listing the account's FINALIZED reconciliation with its
// statement date + balance and a link to its statement report (TxnRead).
func TestReconHistoryListsFinalizedRecon(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.finalizeRecon(t)

	rec := asUser(t, e.h, e.sm, e.reader, http.MethodGet, "/reconciliations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reconciliations = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The history section heading + the finalized recon's row (its statement date and a
	// link to its statement report) appear.
	if !strings.Contains(body, `class="recon-history-table"`) {
		t.Errorf("list missing the history section; body:\n%s", body)
	}
	if !strings.Contains(body, "recon-history-"+strconv.FormatInt(int64(id), 10)) {
		t.Errorf("history missing the finalized recon row (id %d)", id)
	}
	if !strings.Contains(body, "2026-02-28") {
		t.Errorf("history missing the finalized recon's statement date")
	}
	// Each history row links to the statement report with the recon id param.
	wantHref := reconStatementReportHref(int64(id))
	if !strings.Contains(body, wantHref) {
		t.Errorf("history row missing statement-report link %q; body:\n%s", wantHref, body)
	}
	// The history row shows the finalized recon's statement balance (-15,000 minor =
	// USD -$150.00), formatted with the per-currency symbol (p26.24).
	if !strings.Contains(body, "-$150.00") {
		t.Errorf("history missing the finalized recon's statement balance (-$150.00); body:\n%s", body)
	}
}

// TestReconHistoryEmptyForAccountWithNone: an account with NO finalized recon shows no
// history rows (the history section renders but is empty for it) -- an OPEN recon is
// not history.
func TestReconHistoryEmptyForAccountWithNone(t *testing.T) {
	e := newReconWebEnv(t)
	openID := e.startRecon(t, 0) // open, not finalized -> not history

	rec := asUser(t, e.h, e.sm, e.reader, http.MethodGet, "/reconciliations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reconciliations = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "recon-history-"+strconv.FormatInt(int64(openID), 10)) {
		t.Errorf("open recon %d wrongly listed as finalized history", openID)
	}
	if strings.Contains(body, `class="recon-history-row"`) {
		t.Errorf("history has rows for an account with no finalized recon; body:\n%s", body)
	}
}

// --- helpers for the assertions ------------------------------------------

// finalizeDisabled reports whether the workspace's Finalize button is rendered with
// the disabled attribute.
func finalizeDisabled(body string) bool {
	i := strings.Index(body, `id="recon-finalize"`)
	if i < 0 {
		return false
	}
	// Look at the button tag around the id.
	start := strings.LastIndex(body[:i], "<button")
	if start < 0 {
		return false
	}
	end := strings.Index(body[i:], ">")
	if end < 0 {
		return false
	}
	tag := body[start : i+end]
	return strings.Contains(tag, "disabled")
}

// --- p26.57 edit statement (date + ending balance) ------------------------

// TestReconEditWorkspaceShowsForm: an OPEN workspace renders the statement-edit form
// prefilled with the current date + balance; a finalized workspace does not.
func TestReconEditWorkspaceShowsForm(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 25_000)
	body := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil).Body.String()
	if !strings.Contains(body, `id="recon-edit-form"`) {
		t.Errorf("open workspace missing the statement-edit form; body:\n%s", body)
	}
	// Prefilled ending balance (25000 minor => 250.00 in the default number format).
	if !strings.Contains(body, `value="250.00"`) {
		t.Errorf("edit form missing prefilled balance 250.00; body:\n%s", body)
	}
	// The discard action is present on an open recon.
	if !strings.Contains(body, reconDiscardPath(int64(id))) {
		t.Errorf("open workspace missing discard action %q", reconDiscardPath(int64(id)))
	}
}

// TestReconEditUpdatesSummaryAndGate: editing the statement to the cleared total makes
// the difference zero and ENABLES Finalize; a bad balance re-renders at 422.
func TestReconEditUpdatesSummaryAndGate(t *testing.T) {
	e := newReconWebEnv(t)
	// Start at statement 0, clear the +250 deposit => difference -250, Finalize disabled.
	id := e.startRecon(t, 0)
	if rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconTogglePath(int64(id), e.spDep), url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("toggle deposit = %d", rec.Code)
	}
	before := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil).Body.String()
	if !finalizeDisabled(before) {
		t.Fatalf("Finalize should be DISABLED before the edit (diff -250)")
	}

	// Edit the ending balance to 250.00 (== opening 0 + cleared 250) and a new date.
	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconEditPath(int64(id)), url.Values{
		"statement_date": {"2026-03-15"},
		"balance":        {"250.00"},
	})
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("edit POST = %d, want redirect/200; body: %s", rec.Code, rec.Body.String())
	}
	// The store now has the new statement + a zero difference; Finalize enabled.
	sum, err := e.st.ReconciliationSummaryFor(context.Background(), id)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.StatementBalance != 25_000 {
		t.Errorf("statement after edit = %d, want 25000", sum.StatementBalance)
	}
	if sum.Difference != 0 {
		t.Errorf("difference after balancing edit = %d, want 0", sum.Difference)
	}
	got, _ := e.st.GetReconciliation(context.Background(), id)
	if got.StatementDate != "2026-03-15" {
		t.Errorf("statement date after edit = %q, want 2026-03-15", got.StatementDate)
	}
	after := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil).Body.String()
	if finalizeDisabled(after) {
		t.Errorf("Finalize should be ENABLED after the balancing edit; body:\n%s", after)
	}

	// A bad balance re-renders the workspace at 422 with a field error (no change).
	bad := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconEditPath(int64(id)), url.Values{
		"statement_date": {"2026-03-15"},
		"balance":        {"not-a-number"},
	})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Errorf("edit with bad balance = %d, want 422", bad.Code)
	}
	if !strings.Contains(bad.Body.String(), `id="recon-edit-form"`) {
		t.Errorf("422 edit response missing the re-rendered edit form")
	}
}

// --- p26.58 discard -------------------------------------------------------

// TestReconDiscardReleasesAndRemovesFromList: discarding an OPEN recon removes it from
// the continue/open list, un-clears its split, and lets a fresh recon start.
func TestReconDiscardReleasesAndRemovesFromList(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0)
	// Clear the deposit split against this recon.
	if rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconTogglePath(int64(id), e.spDep), url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("toggle = %d", rec.Code)
	}

	// The list offers "Continue" for the open recon before discard.
	listBefore := asUser(t, e.h, e.sm, e.reader, http.MethodGet, "/reconciliations", nil).Body.String()
	if !strings.Contains(listBefore, `href="/reconciliations/`+strconv.FormatInt(int64(id), 10)+`"`) {
		t.Fatalf("list should offer Continue for the open recon before discard")
	}

	// Discard.
	rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconDiscardPath(int64(id)), url.Values{})
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("discard POST = %d, want redirect/200; body: %s", rec.Code, rec.Body.String())
	}

	// (a) gone from the open/continue list (no continue link to this recon id).
	listAfter := asUser(t, e.h, e.sm, e.reader, http.MethodGet, "/reconciliations", nil).Body.String()
	if strings.Contains(listAfter, `href="/reconciliations/`+strconv.FormatInt(int64(id), 10)+`"`) {
		t.Errorf("discarded recon still offered as Continue in the list; body:\n%s", listAfter)
	}
	// The account row now shows a START form again (a fresh recon can begin).
	if !strings.Contains(listAfter, `id="recon-start-form-`+strconv.FormatInt(e.checking, 10)+`"`) {
		t.Errorf("list should show a start form for the account after discard; body:\n%s", listAfter)
	}

	// (b) the split is uncleared / available again.
	ws, err := e.st.ReconciliationWorkspaceSplits(context.Background(), id)
	if err != nil {
		t.Fatalf("workspace splits: %v", err)
	}
	for _, sp := range ws {
		if sp.SplitID == e.spDep && sp.Cleared {
			t.Errorf("deposit split still cleared after discard")
		}
	}

	// (c) a fresh recon can be started for the same account+currency via the list POST.
	start := asUser(t, e.h, e.sm, e.writer, http.MethodPost, "/reconciliations", url.Values{
		"account_id":     {strconv.FormatInt(e.checking, 10)},
		"currency":       {"USD"},
		"statement_date": {"2026-04-30"},
		"balance":        {"0.00"},
	})
	if start.Code != http.StatusSeeOther && start.Code != http.StatusOK {
		t.Fatalf("fresh start after discard = %d, want redirect; body: %s", start.Code, start.Body.String())
	}
}

// TestReconDiscardedWorkspaceReadOnly: a discarded recon's workspace renders read-only
// -- no Finalize button, no toggles, no edit/discard forms, and a discarded note.
func TestReconDiscardedWorkspaceReadOnly(t *testing.T) {
	e := newReconWebEnv(t)
	id := e.startRecon(t, 0)
	// Discard it (no need to clear anything).
	if rec := asUser(t, e.h, e.sm, e.writer, http.MethodPost, reconDiscardPath(int64(id)), url.Values{}); rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("discard = %d", rec.Code)
	}
	body := asUser(t, e.h, e.sm, e.reader, http.MethodGet, reconWorkspacePath(int64(id)), nil).Body.String()
	if !strings.Contains(body, "recon-discarded-note") {
		t.Errorf("discarded workspace missing the discarded note; body:\n%s", body)
	}
	if strings.Contains(body, `id="recon-finalize"`) {
		t.Errorf("discarded workspace should not render the Finalize button")
	}
	if strings.Contains(body, "recon-toggle") {
		t.Errorf("discarded workspace should not render cleared toggles (read-only)")
	}
	if strings.Contains(body, `id="recon-edit-form"`) || strings.Contains(body, "recon-discard-form") {
		t.Errorf("discarded workspace should not render the edit/discard forms")
	}
}

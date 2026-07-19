package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/ids"
	"cuento/internal/store"
	"cuento/internal/testutil"
	"cuento/internal/testutil/fixture"
)

// p12.5 funds workspace handler tests. Driven through the REAL mounted router
// (httptest) against a real migrated db (AGENTS testing conventions) -- no
// handler-level store mocks. The synthetic fixture (Appendix D, RULE 11) supplies
// the two funds (Beca Agua, Building Fund) with real balances; the negative-badge
// case builds an OVERSPENT fund inline because the fixture deliberately keeps every
// fund non-negative (Z18 silent).

// fundsFixtureApp builds an app over the synthetic fixture and a write-capable user,
// returning the handler, store, session manager, ids, and the user id.
func fundsFixtureApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager, fixture.IDs, ids.UserID) {
	t.Helper()
	fx := fixture.New(t)
	app := NewApp(Config{Version: "test"}, fx.DB, fx.Store)
	writer := mkUser(t, fx.Store, "writer", "write", false)
	return app.handler, fx.Store, app.sessions, fx.IDs, writer
}

// fundsSimpleApp builds a bare app (no fixture) for the negative-badge construction
// and the perm test.
func fundsSimpleApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, app.sessions
}

// --- LIST: balances, funder, scope --------------------------------------------

// TestFundsListShowsBalancesFunderScope: the list renders each active fund's
// per-currency balance, its funder, and its scope (subsidiary + program names).
func TestFundsListShowsBalancesFunderScope(t *testing.T) {
	h, _, sm, _, writer := fundsFixtureApp(t)

	rec := asUser(t, h, sm, writer, http.MethodGet, "/funds", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /funds = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Both fixture funds are active and named.
	for _, want := range []string{"Beca Agua 2025", "Building Fund"} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing fund %q", want)
		}
	}
	// Funder column (stored proper noun rendered verbatim).
	if !strings.Contains(body, "Fundacion Agua Limpia") {
		t.Errorf("list missing funder for Beca Agua")
	}
	// Scope: Beca Agua scopes to MX + US and program Educacion; the names appear.
	for _, want := range []string{"RV Mexico", "RV Estados Unidos", "Educacion"} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing scope name %q", want)
		}
	}
	// Balance: Beca Agua's USD asset position is +500.00 (200000 receipt - 150000
	// US supplies spend, minor units), MXN is +97,000.00 (10,000,000 receipt -
	// 300,000 supplies). The formatted amounts appear with the per-currency symbol
	// (USD/MXN both prefix "$", no ISO code — rule 10, p26.24).
	if !strings.Contains(body, "$500.00") {
		t.Errorf("list missing Beca Agua USD balance $500.00; body:\n%s", body)
	}
	if !strings.Contains(body, "$97,000.00") {
		t.Errorf("list missing Beca Agua MXN balance $97,000.00")
	}
}

// TestFundsListNegativeBadge: an OVERSPENT restricted fund (asset-side balance < 0,
// Z18) renders the warning badge. Built inline because the fixture keeps funds
// non-negative.
func TestFundsListNegativeBadge(t *testing.T) {
	h, st, sm := fundsSimpleApp(t)
	writer := mkUser(t, st, "writer", "write", false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: writer})

	// A restricted fund on the root subsidiary, plus a checking (asset) and a
	// salaries (expense) account to post the overspend through.
	fund, err := st.CreateFund(ctx, store.CreateFundInput{
		Name: "Overspent Grant", Funder: "Donor X", Restriction: "purpose",
		Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}
	checking, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Checking"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create checking: %v", err)
	}
	mgmt := "management"
	root := int64(1)
	salaries, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Salaries"}, Subsidiaries: []int64{1},
		FunctionalClass: &mgmt, DefaultProgramID: &root,
	})
	if err != nil {
		t.Fatalf("create salaries: %v", err)
	}
	// Receive 500, spend 900 -- each txn fund-balanced, so the restricted cash goes
	// -400 (overspent). (Contributions credit to balance the receipt.)
	contrib, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Contributions"}, Subsidiaries: []int64{1},
		DefaultProgramID: &root,
	})
	if err != nil {
		t.Fatalf("create contrib: %v", err)
	}
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-01-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: checking, Amount: 50000, FundID: &fund, Position: 0},
			{AccountID: contrib, Amount: -50000, FundID: &fund, ProgramID: &root, Position: 1},
		},
	}); err != nil {
		t.Fatalf("post receipt: %v", err)
	}
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-02-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: salaries, Amount: 90000, FundID: &fund, ProgramID: &root, FunctionalClass: &mgmt, Position: 0},
			{AccountID: checking, Amount: -90000, FundID: &fund, Position: 1},
		},
	}); err != nil {
		t.Fatalf("post spend: %v", err)
	}

	rec := asUser(t, h, sm, writer, http.MethodGet, "/funds", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /funds = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "badge-warning") {
		t.Errorf("overspent fund missing negative warning badge; body:\n%s", body)
	}
}

// TestFundsListClosedToggle: a closed fund is hidden on the default (active) list
// and shown under ?closed=1.
func TestFundsListClosedToggle(t *testing.T) {
	h, st, sm, ids, writer := fundsFixtureApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: writer})
	if err := st.CloseFund(ctx, ids.BuildingFund); err != nil {
		t.Fatalf("CloseFund: %v", err)
	}

	active := asUser(t, h, sm, writer, http.MethodGet, "/funds", nil).Body.String()
	if strings.Contains(active, "Building Fund") {
		t.Errorf("closed fund still on the active list")
	}
	if !strings.Contains(active, "Beca Agua 2025") {
		t.Errorf("active fund dropped from the active list")
	}

	closed := asUser(t, h, sm, writer, http.MethodGet, "/funds?closed=1", nil).Body.String()
	if !strings.Contains(closed, "Building Fund") {
		t.Errorf("closed fund missing from the closed toggle")
	}
	if strings.Contains(closed, "Beca Agua 2025") {
		t.Errorf("active fund leaked into the closed toggle")
	}
}

// --- DETAIL: statement with opening/closing -----------------------------------

// TestFundStatementOpeningClosing: the statement lists the fund's splits across all
// accounts and shows opening (0) + closing balances per currency that reconcile to
// the list balance.
func TestFundStatementOpeningClosing(t *testing.T) {
	h, st, sm, ids, writer := fundsFixtureApp(t)

	rec := asUser(t, h, sm, writer, http.MethodGet, fundStatementURL(ids.BecaAgua), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET fund statement = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The statement carries splits across MULTIPLE accounts (receipt on checking,
	// spend on program supplies + government grants).
	for _, want := range []string{"Checking MX", "Program Supplies", "Government Grants"} {
		if !strings.Contains(body, want) {
			t.Errorf("statement missing account %q", want)
		}
	}
	// Opening is 0 for each currency; closing reconciles to the list balance.
	// Amounts carry the per-currency symbol (USD/MXN prefix "$", p26.24).
	if !strings.Contains(body, "$0.00") {
		t.Errorf("statement missing zero opening balances")
	}
	// Closing $500.00 (matches the list) and $97,000.00.
	if !strings.Contains(body, "$500.00") {
		t.Errorf("statement missing USD closing $500.00")
	}

	// RECONCILIATION invariant: the statement's closing per currency EQUALS
	// FundBalancesAsOf for the same fund (both sum the asset splits).
	ledger, err := st.FundLedger(context.Background(), ids.BecaAgua, "2026-12-31")
	if err != nil {
		t.Fatalf("FundLedger: %v", err)
	}
	closing := map[string]int64{}
	for _, r := range ledger {
		closing[r.Currency] = r.RunningBalance
	}
	fb, err := st.FundBalancesAsOf(context.Background(), "2026-12-31", 1)
	if err != nil {
		t.Fatalf("FundBalancesAsOf: %v", err)
	}
	for _, c := range fb {
		if c.FundID == ids.BecaAgua && closing[c.Currency] != c.Amount {
			t.Errorf("closing[%s]=%d != FundBalancesAsOf %d", c.Currency, closing[c.Currency], c.Amount)
		}
	}
}

// --- FORM: subsidiary checklist + program scope -------------------------------

// TestFundNewFormHasChecklistAndProgram: the create form renders the subsidiary
// checklist (sub_<id> checkboxes) and the program-scope select.
func TestFundNewFormHasChecklistAndProgram(t *testing.T) {
	h, _, sm, _, writer := fundsFixtureApp(t)

	rec := asUser(t, h, sm, writer, http.MethodGet, "/funds/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /funds/new = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="sub_1"`) {
		t.Errorf("form missing subsidiary checklist (sub_1)")
	}
	if !strings.Contains(body, `name="program_id"`) {
		t.Errorf("form missing program scope select")
	}
	if !strings.Contains(body, "Educacion") {
		t.Errorf("form missing a program option")
	}
}

// TestFundCreateHappyPath: a writer POSTs a valid create (name, restriction, a
// subsidiary via the checklist, a program) and it appears on the list.
func TestFundCreateHappyPath(t *testing.T) {
	h, st, sm, ids, writer := fundsFixtureApp(t)

	form := url.Values{}
	form.Set("name", "New Scholarship Fund")
	form.Set("funder", "New Donor")
	form.Set("purpose", "Scholarships")
	form.Set("restriction", "purpose")
	form.Set("program_id", itoa(ids.Educacion))
	form.Set("sub_"+itoa(ids.US), itoa(ids.US))

	rec := asUser(t, h, sm, writer, http.MethodPost, "/funds", form)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("POST /funds = %d, want redirect; body:\n%s", rec.Code, rec.Body.String())
	}

	funds, err := st.ListFunds(context.Background())
	if err != nil {
		t.Fatalf("ListFunds: %v", err)
	}
	found := false
	for _, f := range funds {
		if f.Name == "New Scholarship Fund" {
			found = true
			if !f.ProgramID.Valid || f.ProgramID.Int64 != ids.Educacion {
				t.Errorf("created fund program scope = %v, want Educacion", f.ProgramID)
			}
		}
	}
	if !found {
		t.Errorf("created fund not found")
	}
}

// TestFundCreateNoSubsidiaryReRenders: an empty subsidiary checklist yields a 422
// re-render with the localized error (the store's ErrFundNoSubsidiary, mapped).
func TestFundCreateNoSubsidiaryReRenders(t *testing.T) {
	h, _, sm, _, writer := fundsFixtureApp(t)

	form := url.Values{}
	form.Set("name", "No Sub Fund")
	form.Set("restriction", "purpose")
	// No sub_<id> fields -> empty checklist.

	rec := asUser(t, h, sm, writer, http.MethodPost, "/funds", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /funds (no sub) = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "field-error") {
		t.Errorf("422 re-render missing the field error")
	}
}

// TestFundEditPrefillsChecklist: the edit form pre-checks the fund's current
// subsidiaries.
func TestFundEditPrefillsChecklist(t *testing.T) {
	h, _, sm, ids, writer := fundsFixtureApp(t)

	rec := asUser(t, h, sm, writer, http.MethodGet, "/funds/"+itoa(ids.BecaAgua)+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edit = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Beca Agua scopes to MX + US, so both boxes are checked.
	if !strings.Contains(body, `name="sub_`+itoa(ids.US)+`" value="`+itoa(ids.US)+`" checked`) {
		t.Errorf("edit form did not pre-check the US subsidiary")
	}
}

// --- PERMS: TxnRead views but cannot manage -----------------------------------

// TestFundsPermReadCannotManage: a TxnRead user CAN view the list and a statement
// (200) but CANNOT create or close a fund (403) -- the required perm assertion
// (view = TxnRead, manage = TxnWrite).
func TestFundsPermReadCannotManage(t *testing.T) {
	h, st, sm, ids, _ := fundsFixtureApp(t)
	reader := mkUser(t, st, "reader", "read", false)

	// View: 200.
	if rec := asUser(t, h, sm, reader, http.MethodGet, "/funds", nil); rec.Code != http.StatusOK {
		t.Errorf("reader GET /funds = %d, want 200", rec.Code)
	}
	if rec := asUser(t, h, sm, reader, http.MethodGet, fundStatementURL(ids.BecaAgua), nil); rec.Code != http.StatusOK {
		t.Errorf("reader GET statement = %d, want 200", rec.Code)
	}

	// Manage: 403. Create.
	form := url.Values{}
	form.Set("name", "X")
	form.Set("restriction", "purpose")
	form.Set("sub_1", "1")
	if rec := asUser(t, h, sm, reader, http.MethodPost, "/funds", form); rec.Code != http.StatusForbidden {
		t.Errorf("reader POST /funds = %d, want 403", rec.Code)
	}
	// Close.
	if rec := asUser(t, h, sm, reader, http.MethodPost, fundStatementURL(ids.BecaAgua)+"/close", nil); rec.Code != http.StatusForbidden {
		t.Errorf("reader POST close = %d, want 403", rec.Code)
	}
	// The new/edit GET forms are TxnWrite too.
	if rec := asUser(t, h, sm, reader, http.MethodGet, "/funds/new", nil); rec.Code != http.StatusForbidden {
		t.Errorf("reader GET /funds/new = %d, want 403", rec.Code)
	}
}

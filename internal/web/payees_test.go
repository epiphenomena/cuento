package web

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"cuento/internal/store"
)

// p12.3 payee autocomplete + autofill handler tests. Driven through the REAL mounted
// router (httptest) against a real migrated db (AGENTS conventions); no store mocks.
//   TestPayeeSuggestRanking     -> the store test (order is cleanest to assert there)
//   TestPayeeTemplatePrefills   -> here: the /payees/{id}/template partial reflects the
//                                  last txn's account/amount/fund/program/class fields
//   TestAutofillRespectsSubsidiary -> here: out-of-sub splits dropped + notice shown
//   TestAutofillNeverOverwrites -> the JS node test (pure client guard)

// seedPayeeTxn posts a transaction tagged with a new payee `name` in `sub`, returning
// the payee id. splits are the store SplitInputs (already balanced by the caller).
func (e *txnWebEnv) seedPayeeTxn(t *testing.T, name, date string, sub int64, splits []store.SplitInput) int64 {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	pid, err := e.st.CreatePayee(ctx, name)
	must(t, err, "create payee "+name)
	in := store.PostTransactionInput{
		Date: date, SubsidiaryID: sub, Currency: "USD", PayeeID: &pid, Splits: splits,
	}
	_, err = e.st.PostTransaction(ctx, in)
	must(t, err, "seed txn for "+name)
	return pid
}

// TestPayeeTemplatePrefills: /payees/{id}/template?sub= returns rows carrying the
// payee's last non-deleted transaction's splits -- account, memo, amount (signed),
// fund, program, functional class -- as editable grid rows.
func TestPayeeTemplatePrefills(t *testing.T) {
	e := newTxnWebEnv(t)
	prog := e.progRoot

	// A restricted fund scoped to sub1 with NO program scope (so an R/E split may carry
	// it without a fund-program-scope conflict). Both splits are tagged with it so the
	// per-fund zero-sum holds and the fund DIMENSION is exercised in the template.
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	fund, err := e.st.CreateFund(ctx, store.CreateFundInput{
		Name: "Building", Restriction: "purpose", Subsidiaries: []int64{e.sub1},
	})
	must(t, err, "create fund")

	// A payee whose last txn (in sub1): salaries expense 120.00 (program, fund, memo)
	// debit; checking asset -120.00 (same fund) credit.
	pid := e.seedPayeeTxn(t, "Prefill Vendor", "2025-02-01", e.sub1, []store.SplitInput{
		{AccountID: e.salaries, Amount: 12000, ProgramID: &prog, FundID: &fund, FunctionalClass: strptr("program"), Memo: "office", Position: 0},
		{AccountID: e.checking, Amount: -12000, FundID: &fund, Position: 1},
	})

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet,
		"/payees/"+itoa(pid)+"/template?sub="+itoa(e.sub1), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("template: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Two rows, each a txn-row (stable ids txn-account-0 / -1).
	if n := strings.Count(body, `class="txn-row"`); n != 2 {
		t.Fatalf("want 2 template rows, got %d\n%s", n, body)
	}
	// The salaries account is selected on row 0 (account, class, program carried).
	assertSelected(t, body, "txn-account-0", e.salaries)
	assertSelected(t, body, "txn-program-0", e.progRoot)
	// The signed amount is echoed into the amount field of row 0 (120.00).
	if !strings.Contains(body, `id="txn-amount-0" class="txn-amount amount" value="120.00"`) {
		t.Fatalf("row 0 amount not prefilled 120.00\n%s", body)
	}
	// The memo carried.
	if !strings.Contains(body, `id="txn-memo-0" class="txn-memo" value="office"`) {
		t.Fatalf("row 0 memo not prefilled\n%s", body)
	}
	// The functional class (program) selected on row 0.
	if !strings.Contains(body, `<option value="program" selected>`) {
		t.Fatalf("row 0 class not prefilled program\n%s", body)
	}
	// The fund carried on both rows (the fund dimension the spec enumerates).
	assertSelected(t, body, "txn-fund-0", fund)
	assertSelected(t, body, "txn-fund-1", fund)
	// Row 1 (checking credit) carries the -120.00 signed amount.
	assertSelected(t, body, "txn-account-1", e.checking)
	if !strings.Contains(body, `id="txn-amount-1" class="txn-amount amount" value="-120.00"`) {
		t.Fatalf("row 1 amount not prefilled -120.00\n%s", body)
	}
	// No out-of-sub notice for an all-in-sub template.
	if !strings.Contains(body, `id="txn-autofill-notice" hx-swap-oob="true" class="txn-autofill-notice" hidden>`) {
		t.Fatalf("expected hidden (empty) autofill notice\n%s", body)
	}
}

// TestAutofillRespectsSubsidiary: template splits whose account is NOT valid in the
// selected subsidiary are dropped server-side, and the partial shows a notice.
func TestAutofillRespectsSubsidiary(t *testing.T) {
	e := newTxnWebEnv(t)
	prog := e.progRoot

	// The payee's last txn is in sub2: salaries (mapped sub1+sub2) debit; cashB (sub2
	// ONLY) credit. When we fetch the template for SUB1, the cashB split must drop
	// (cashB is not in sub1) and a notice appears; salaries survives (mapped to sub1).
	pid := e.seedPayeeTxn(t, "Cross Sub Vendor", "2025-02-02", e.sub2, []store.SplitInput{
		{AccountID: e.salaries, Amount: 8000, ProgramID: &prog, FunctionalClass: strptr("program"), Position: 0},
		{AccountID: e.cashB, Amount: -8000, Position: 1},
	})

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet,
		"/payees/"+itoa(pid)+"/template?sub="+itoa(e.sub1), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("template: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Exactly ONE row survives (salaries); cashB dropped.
	if n := strings.Count(body, `class="txn-row"`); n != 1 {
		t.Fatalf("want 1 surviving row after drop, got %d\n%s", n, body)
	}
	assertSelected(t, body, "txn-account-0", e.salaries)
	// cashB must not appear selected anywhere.
	if strings.Contains(body, `value="`+itoa(e.cashB)+`" selected`) {
		t.Fatalf("dropped cashB should not be selected\n%s", body)
	}
	// The notice is present (rendered, not hidden) and carries the i18n key's text.
	if strings.Contains(body, `id="txn-autofill-notice" hx-swap-oob="true" class="txn-autofill-notice" hidden>`) {
		t.Fatalf("expected a visible out-of-sub notice, got hidden\n%s", body)
	}
	if !strings.Contains(body, "subsidiary") {
		t.Fatalf("notice text missing\n%s", body)
	}
}

// TestPayeeCreateOnSave: saving a transaction with a TYPED (new) payee name creates
// the payee (find-or-create, versioned) and tags the transaction with it. A second
// save with the same name REUSES the payee (no duplicate).
func TestPayeeCreateOnSave(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.balancedForm("50.00", "-50.00")
	f.Set("payee", "0") // no existing id chosen
	f.Set("payee_name", "Brand New Vendor")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The payee exists and the transaction is tagged with it.
	id := latestTxnID(t, e)
	var payeeID sql.NullInt64
	if err := e.db.QueryRow(`SELECT payee_id FROM transactions WHERE id = ?`, id).Scan(&payeeID); err != nil {
		t.Fatalf("read txn payee: %v", err)
	}
	if !payeeID.Valid {
		t.Fatalf("transaction not tagged with the created payee")
	}
	var name string
	var pvCount int
	if err := e.db.QueryRow(`SELECT name FROM payees WHERE id = ?`, payeeID.Int64).Scan(&name); err != nil {
		t.Fatalf("read payee: %v", err)
	}
	if name != "Brand New Vendor" {
		t.Fatalf("payee name = %q, want Brand New Vendor", name)
	}
	// Versioned (rule 5): a create version row exists.
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM payees_versions WHERE entity_id = ?`, payeeID.Int64).Scan(&pvCount); err != nil {
		t.Fatalf("count payee versions: %v", err)
	}
	if pvCount != 1 {
		t.Fatalf("payee version rows = %d, want 1", pvCount)
	}

	// A SECOND save with the same name reuses the payee (no duplicate row).
	f2 := e.balancedForm("60.00", "-60.00")
	f2.Set("payee", "0")
	f2.Set("payee_name", "brand new vendor") // different case -> same payee (NOCASE)
	rec2 := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f2)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("second create: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var payeeCount int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM payees`).Scan(&payeeCount); err != nil {
		t.Fatalf("count payees: %v", err)
	}
	if payeeCount != 1 {
		t.Fatalf("payee count = %d, want 1 (reused, not duplicated)", payeeCount)
	}
}

// assertSelected checks that some <option value=id ...> in body is marked selected.
// The account option markup spreads attributes across lines (value then, later,
// `selected`), so we match the option element's open tag containing both value=id and
// selected rather than requiring them adjacent.
func assertSelected(t *testing.T, body, field string, id int64) {
	t.Helper()
	if !strings.Contains(body, `id="`+field+`"`) {
		t.Fatalf("field %s absent\n%s", field, body)
	}
	// An <option ...> tag carrying value="id" and, somewhere in the same tag, selected.
	re := regexp.MustCompile(`(?s)<option[^>]*value="` + itoa(id) + `"[^>]*\bselected\b[^>]*>`)
	if !re.MatchString(body) {
		t.Fatalf("field %s: option %d not selected\n%s", field, id, body)
	}
}

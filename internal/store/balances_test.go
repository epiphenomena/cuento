package store

import (
	"context"
	"database/sql"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/testutil"
)

// Balance queries (p08.4) -- read-only aggregates over a SMALL, fully
// hand-computed dataset built through the store (AGENTS testing conventions:
// hand-computed expectations on tiny in-test data). Every expected number below is
// derived by hand from balEnv's transaction list; the deleted transaction (T7) is
// excluded from ALL of them and is asserted so explicitly.
//
// Net-debit signs (D2): asset/expense debits are positive, revenue/liability/equity
// credits are negative. Scope (D18) = a subsidiary consolidated with ALL its
// descendants. Unrestricted fund (D20, NULL) is fund id 0.

// balEnv is the shared dataset. Subsidiaries: root (id 1) + two children US and CA.
// Programs: root (id 1) + educ. Funds: grant (restricted) + unrestricted (NULL).
// Accounts: checking + fxclear (assets), contrib (revenue), salaries (expense).
type balEnv struct {
	t   *testing.T
	s   *Store
	d   *sql.DB
	ctx context.Context

	subUS, subCA ids.SubsidiaryID
	educ         ids.ProgramID
	grant        ids.FundID

	checking, fxclear, contrib, salaries ids.AccountID

	// checking split ids in post order, for cursor assertions.
	t1check, t2check, t5check, t3check, t6check ids.SplitID
	// salaries split ids for the program-filter register.
	t2sal, t3sal ids.SplitID
	// fxclear split ids for the multi-currency register.
	t5fx, t4fx ids.SplitID
}

// newBalEnv builds balEnv exactly, posting the seven transactions (T7 soft-deleted)
// and capturing the split ids the register assertions need.
func newBalEnv(t *testing.T) balEnv {
	t.Helper()
	d := testutil.NewDB(t)
	s := New(d)
	ctx := mutCtx()

	subUS := newSub(t, s, rootID, "US")
	subCA := newSub(t, s, rootID, "CA")

	educ, err := s.CreateProgram(ctx, CreateProgramInput{ParentID: rootProgramID, Name: "Educacion"})
	if err != nil {
		t.Fatalf("CreateProgram: %v", err)
	}
	grant := mkFund(t, s, "Grant A", []ids.SubsidiaryID{subUS, subCA}, nil)

	root := rootProgramID
	mgmt := "management"
	checking := mkAcct(t, s, "asset", "Checking", []ids.SubsidiaryID{subUS, subCA}, nil, nil)
	fxclear := mkAcct(t, s, "asset", "FX Clearing", []ids.SubsidiaryID{subUS}, nil, nil)
	contrib := mkAcct(t, s, "revenue", "Contributions", []ids.SubsidiaryID{subUS, subCA}, nil, &root)
	salaries := mkAcct(t, s, "expense", "Salaries", []ids.SubsidiaryID{subUS, subCA}, &mgmt, &root)

	e := balEnv{
		t: t, s: s, d: d, ctx: ctx,
		subUS: subUS, subCA: subCA, educ: educ, grant: grant,
		checking: checking, fxclear: fxclear, contrib: contrib, salaries: salaries,
	}

	// T1 2025-01-05 subUS USD grant: checking +20000, contrib -20000 (prog root).
	// The grant RECEIPT (donor gives 20000 into checking, restricted).
	_, e.t1check = e.post(t, "2025-01-05", subUS, "USD", "",
		split{checking, 20000, &grant, nil, nil},
		split{contrib, -20000, &grant, &root, nil})

	// T2 2025-01-15 subUS USD unrestricted: salaries +10000 (mgmt, root), checking -10000.
	_, sids := e.postN(t, "2025-01-15", subUS, "USD", "",
		split{salaries, 10000, nil, &root, &mgmt},
		split{checking, -10000, nil, nil, nil})
	e.t2sal, e.t2check = sids[0], sids[1]

	// T3 2025-02-10 subUS USD grant: salaries +6000 (mgmt, educ), checking -6000.
	// The grant SPEND (memo "rent payment" for the text-filter test).
	_, sids = e.postN(t, "2025-02-10", subUS, "USD", "rent payment",
		split{salaries, 6000, &grant, &e.educ, &mgmt},
		split{checking, -6000, &grant, nil, nil})
	e.t3sal, e.t3check = sids[0], sids[1]

	// T4 2025-02-20 subUS MXN unrestricted: fxclear +5000 MXN, contrib -5000 (root).
	_, sids = e.postN(t, "2025-02-20", subUS, "MXN", "",
		split{fxclear, 5000, nil, nil, nil},
		split{contrib, -5000, nil, &root, nil})
	e.t4fx = sids[0]

	// T5 2025-01-15 subUS USD unrestricted: fxclear +3000, checking -3000.
	// Same DATE as T2 -- the same-date cursor boundary case for the checking register.
	_, sids = e.postN(t, "2025-01-15", subUS, "USD", "",
		split{fxclear, 3000, nil, nil, nil},
		split{checking, -3000, nil, nil, nil})
	e.t5fx, e.t5check = sids[0], sids[1]

	// T6 2025-03-05 subCA USD unrestricted: contrib -8000 (root), checking +8000.
	_, sids = e.postN(t, "2025-03-05", subCA, "USD", "",
		split{contrib, -8000, nil, &root, nil},
		split{checking, 8000, nil, nil, nil})
	e.t6check = sids[1]

	// T7 2025-01-10 subUS USD grant: salaries +99999 (mgmt, educ), checking -99999.
	// SOFT-DELETED -- must be excluded from every query below.
	t7, _ := e.postN(t, "2025-01-10", subUS, "USD", "",
		split{salaries, 99999, &grant, &e.educ, &mgmt},
		split{checking, -99999, &grant, nil, nil})
	if err := s.DeleteTransaction(ctx, t7); err != nil {
		t.Fatalf("DeleteTransaction(T7): %v", err)
	}

	return e
}

// split is a compact split spec for the test builder.
type split struct {
	acct   ids.AccountID
	amount int64
	fund   *ids.FundID
	prog   *ids.ProgramID
	fclass *string
}

// post posts a transaction and returns (txnID, firstSplitID). Used when only the
// first split id is captured; postN returns all split ids.
func (e balEnv) post(t *testing.T, date string, sub ids.SubsidiaryID, ccy, memo string, sp ...split) (ids.TransactionID, ids.SplitID) {
	t.Helper()
	id, sids := e.postN(t, date, sub, ccy, memo, sp...)
	return id, sids[0]
}

// postN posts a transaction and returns (txnID, splitIDsInOrder).
func (e balEnv) postN(t *testing.T, date string, sub ids.SubsidiaryID, ccy, memo string, sp ...split) (ids.TransactionID, []ids.SplitID) {
	t.Helper()
	in := PostTransactionInput{Date: date, SubsidiaryID: sub, Currency: ccy, Memo: memo}
	for i, s := range sp {
		si := SplitInput{AccountID: s.acct, Amount: s.amount, Position: int64(i)}
		si.FundID = s.fund
		si.ProgramID = s.prog
		si.FunctionalClass = s.fclass
		in.Splits = append(in.Splits, si)
	}
	id, err := e.s.PostTransaction(e.ctx, in)
	if err != nil {
		t.Fatalf("PostTransaction(%s): %v", date, err)
	}
	sids := splitIDsInOrder(t, e.d, id)
	if len(sids) != len(sp) {
		t.Fatalf("post %s: got %d split ids, want %d", date, len(sids), len(sp))
	}
	return id, sids
}

// splitIDsInOrder returns a transaction's live split ids ordered by (position, id).
func splitIDsInOrder(t *testing.T, d *sql.DB, txnID ids.TransactionID) []ids.SplitID {
	t.Helper()
	rows, err := d.Query(`SELECT id FROM splits WHERE transaction_id = ? ORDER BY position, id`, int64(txnID))
	if err != nil {
		t.Fatalf("splitIDsInOrder: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ids.SplitID
	for rows.Next() {
		var id ids.SplitID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan split id: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// --- assertion helpers ----------------------------------------------------

// acctMap keys AccountCurrencyAmount cells by "account/currency" for lookup.
func acctMap(cells []AccountCurrencyAmount) map[string]int64 {
	m := make(map[string]int64, len(cells))
	for _, c := range cells {
		m[key2(c.AccountID, c.Currency)] = c.Amount
	}
	return m
}

func key2[T ~int64](id T, ccy string) string { return ccyKey(int64(id)) + "/" + ccy }

// ccyKey stringifies an int64 id without importing strconv into a hot path -- kept
// tiny and local.
func ccyKey(id int64) string {
	if id == 0 {
		return "0"
	}
	neg := id < 0
	if neg {
		id = -id
	}
	var b [20]byte
	i := len(b)
	for id > 0 {
		i--
		b[i] = byte('0' + id%10)
		id /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func wantCell[T ~int64](t *testing.T, m map[string]int64, id T, ccy string, want int64) {
	t.Helper()
	got, ok := m[key2(id, ccy)]
	if !ok {
		t.Errorf("cell %d/%s missing, want %d", id, ccy, want)
		return
	}
	if got != want {
		t.Errorf("cell %d/%s = %d, want %d", id, ccy, got, want)
	}
}

func wantNoCell[T ~int64](t *testing.T, m map[string]int64, id T, ccy string) {
	t.Helper()
	if got, ok := m[key2(id, ccy)]; ok {
		t.Errorf("cell %d/%s = %d present, want absent", id, ccy, got)
	}
}

// ============================ SubtreeBalancesAsOf ==========================

func TestSubtreeBalancesAsOf(t *testing.T) {
	e := newBalEnv(t)

	// Root (full consolidation), as of end of year: every non-deleted split.
	root := acctMap(e.subtree("2025-12-31", rootID))
	wantCell(t, root, e.checking, "USD", 9000)  // +20000-10000-6000-3000+8000
	wantCell(t, root, e.contrib, "USD", -28000) // -20000-8000
	wantCell(t, root, e.contrib, "MXN", -5000)  // -5000
	wantCell(t, root, e.fxclear, "USD", 3000)   // +3000
	wantCell(t, root, e.fxclear, "MXN", 5000)   // +5000
	wantCell(t, root, e.salaries, "USD", 16000) // +10000+6000 (T7's +99999 EXCLUDED)
	if len(root) != 6 {
		t.Errorf("root cells = %d, want 6: %+v", len(root), root)
	}

	// Leaf subUS, end of year: T6 (subCA) is out of scope.
	us := acctMap(e.subtree("2025-12-31", e.subUS))
	wantCell(t, us, e.checking, "USD", 1000) // +20000-10000-6000-3000 (no T6 +8000)
	wantCell(t, us, e.contrib, "USD", -20000)
	wantCell(t, us, e.contrib, "MXN", -5000)
	wantCell(t, us, e.fxclear, "USD", 3000)
	wantCell(t, us, e.fxclear, "MXN", 5000)
	wantCell(t, us, e.salaries, "USD", 16000)

	// Leaf subUS, as of end of JANUARY: only T1, T2, T5 (T7 deleted; T3/T4 are Feb).
	usJan := acctMap(e.subtree("2025-01-31", e.subUS))
	wantCell(t, usJan, e.checking, "USD", 7000)  // +20000-10000-3000
	wantCell(t, usJan, e.fxclear, "USD", 3000)   // +3000 (T5)
	wantCell(t, usJan, e.salaries, "USD", 10000) // +10000 (T2 only, T3 is Feb, T7 deleted)
	wantCell(t, usJan, e.contrib, "USD", -20000) // T1
	wantNoCell(t, usJan, e.fxclear, "MXN")       // T4 is Feb
	wantNoCell(t, usJan, e.contrib, "MXN")       // T4 is Feb
	if len(usJan) != 4 {
		t.Errorf("usJan cells = %d, want 4: %+v", len(usJan), usJan)
	}

	// Leaf subCA, end of year: only T6.
	ca := acctMap(e.subtree("2025-12-31", e.subCA))
	wantCell(t, ca, e.checking, "USD", 8000)
	wantCell(t, ca, e.contrib, "USD", -8000)
	if len(ca) != 2 {
		t.Errorf("ca cells = %d, want 2: %+v", len(ca), ca)
	}
}

// ============================ PeriodActivity ==============================

func TestPeriodActivity(t *testing.T) {
	e := newBalEnv(t)

	// February at root: T3 (USD) and T4 (MXN); Jan (T1/T2/T5) and Mar (T6) excluded.
	feb := acctMap(e.period("2025-02-01", "2025-02-28", rootID))
	wantCell(t, feb, e.salaries, "USD", 6000)
	wantCell(t, feb, e.checking, "USD", -6000)
	wantCell(t, feb, e.fxclear, "MXN", 5000)
	wantCell(t, feb, e.contrib, "MXN", -5000)
	if len(feb) != 4 {
		t.Errorf("feb cells = %d, want 4: %+v", len(feb), feb)
	}
}

// ============================ FundBalancesAsOf ============================

func TestFundBalancesAsOf(t *testing.T) {
	e := newBalEnv(t)

	// Asset-side unexpended balances at root, end of year.
	cells := e.fundBalances("2025-12-31", rootID)
	m := make(map[string]int64, len(cells))
	for _, c := range cells {
		m[key2(c.FundID, c.Currency)] = c.Amount
	}
	// grant: received 20000 (T1 checking), spent 6000 (T3 checking) = 14000
	// unexpended. T7's -99999 checking is DELETED and excluded.
	wantCell(t, m, e.grant, "USD", 14000)
	// unrestricted (fund 0) USD: T2 -10000, T5 fxclear +3000, T5 checking -3000, T6 +8000 = -2000.
	wantCell(t, m, ids.FundID(0), "USD", -2000)
	// unrestricted MXN: T4 fxclear +5000.
	wantCell(t, m, ids.FundID(0), "MXN", 5000)
	if len(cells) != 3 {
		t.Errorf("fund cells = %d, want 3: %+v", len(cells), cells)
	}
}

// ============================ FundLedger =================================

func TestFundLedger(t *testing.T) {
	e := newBalEnv(t)

	rows, err := e.s.FundLedger(e.ctx, e.grant, "2025-12-31")
	if err != nil {
		t.Fatalf("FundLedger: %v", err)
	}
	// grant splits (non-deleted): T1 (checking +20000 / contrib -20000) and
	// T3 (salaries +6000 / checking -6000). T7 is soft-deleted -> excluded. So 4
	// rows, ordered by (date, split_id): T1 checking, T1 contrib, T3 salaries,
	// T3 checking.
	if len(rows) != 4 {
		t.Fatalf("fund ledger = %d rows, want 4 (T7 excluded): %+v", len(rows), rows)
	}
	// Running balance tracks the ASSET-side unexpended position per currency:
	//   T1 checking +20000 (asset)        -> 20000
	//   T1 contrib  -20000 (revenue, 0)   -> 20000
	//   T3 salaries +6000  (expense, 0)   -> 20000
	//   T3 checking -6000  (asset)        -> 14000  (closing)
	wantRun := []int64{20000, 20000, 20000, 14000}
	wantAsset := []bool{true, false, false, true}
	for i, r := range rows {
		if r.RunningBalance != wantRun[i] {
			t.Errorf("row %d running = %d, want %d", i, r.RunningBalance, wantRun[i])
		}
		if r.IsAsset != wantAsset[i] {
			t.Errorf("row %d IsAsset = %v, want %v", i, r.IsAsset, wantAsset[i])
		}
	}

	// RECONCILIATION (the coherence invariant): the ledger's CLOSING running
	// balance per currency EQUALS FundBalancesAsOf for the same fund/currency.
	closing := make(map[string]int64)
	for _, r := range rows {
		closing[r.Currency] = r.RunningBalance // last row per currency wins (ordered)
	}
	fb := e.fundBalances("2025-12-31", rootID)
	for _, c := range fb {
		if c.FundID != e.grant {
			continue
		}
		if closing[c.Currency] != c.Amount {
			t.Errorf("closing[%s] = %d, FundBalancesAsOf = %d (must reconcile)",
				c.Currency, closing[c.Currency], c.Amount)
		}
	}
	if closing["USD"] != 14000 {
		t.Errorf("grant closing USD = %d, want 14000", closing["USD"])
	}
}

func TestFundLedgerExcludesOtherFunds(t *testing.T) {
	e := newBalEnv(t)
	// The unrestricted group (NULL fund) is fund 0; FundLedger(0) must return NO
	// rows (the query matches sp.fund_id = 0, and no split carries literal 0).
	rows, err := e.s.FundLedger(e.ctx, 0, "2025-12-31")
	if err != nil {
		t.Fatalf("FundLedger(0): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("FundLedger(0) = %d rows, want 0 (unrestricted is NULL, not 0)", len(rows))
	}
}

// TestFundLedgerAsOfMatchesFundBalances proves the as-of BOUND keeps the statement
// and the list balance in agreement even with a FUTURE-dated split (a post-dated
// payment): FundLedger(asof) and FundBalancesAsOf(asof) must reconcile for EVERY
// as-of, not just one past today.
func TestFundLedgerAsOfMatchesFundBalances(t *testing.T) {
	e := newBalEnv(t)

	// A FUTURE-dated grant asset spend (2099): checking -1000 (grant), salaries
	// +1000 (grant, management class, educ program). Fund-balanced, so no invariant
	// fires; it sits past a mid as-of.
	mgmt := "management"
	e.postN(t, "2099-01-01", e.subUS, "USD", "future spend",
		split{e.salaries, 1000, &e.grant, &e.educ, &mgmt},
		split{e.checking, -1000, &e.grant, nil, nil})

	for _, asof := range []string{"2025-01-01", "2025-12-31", "2099-12-31"} {
		ledger, err := e.s.FundLedger(e.ctx, e.grant, asof)
		if err != nil {
			t.Fatalf("FundLedger as of %s: %v", asof, err)
		}
		closing := map[string]int64{}
		for _, r := range ledger {
			closing[r.Currency] = r.RunningBalance
		}
		fb := e.fundBalances(asof, rootID)
		for _, c := range fb {
			if c.FundID != e.grant {
				continue
			}
			if closing[c.Currency] != c.Amount {
				t.Errorf("as of %s: closing[%s]=%d != FundBalancesAsOf %d",
					asof, c.Currency, closing[c.Currency], c.Amount)
			}
		}
	}
}

// ============================ FunctionalActivity =========================

func TestFunctionalActivity(t *testing.T) {
	e := newBalEnv(t)

	cells, err := e.s.FunctionalActivity(e.ctx, "2025-01-01", "2025-12-31", rootID)
	if err != nil {
		t.Fatalf("FunctionalActivity: %v", err)
	}
	// Only salaries (expense) carries a class; all its splits are 'management'.
	// T2 +10000 + T3 +6000 = 16000; T7 +99999 DELETED and excluded.
	if len(cells) != 1 {
		t.Fatalf("functional cells = %d, want 1: %+v", len(cells), cells)
	}
	c := cells[0]
	if c.AccountID != e.salaries || c.FunctionalClass != "management" || c.Currency != "USD" || c.Amount != 16000 {
		t.Errorf("functional cell = %+v, want salaries/management/USD/16000", c)
	}
}

// ============================ ProgramActivity ============================

func TestProgramActivity(t *testing.T) {
	e := newBalEnv(t)

	cells, err := e.s.ProgramActivity(e.ctx, "2025-01-01", "2025-12-31", rootID)
	if err != nil {
		t.Fatalf("ProgramActivity: %v", err)
	}
	// Key by (program, account, currency).
	type pk struct {
		prog ids.ProgramID
		acct ids.AccountID
		ccy  string
	}
	m := make(map[pk]int64, len(cells))
	for _, c := range cells {
		m[pk{c.ProgramID, c.AccountID, c.Currency}] = c.Amount
	}
	root := rootProgramID
	// salaries: root T2 +10000, educ T3 +6000 (T7 educ +99999 DELETED).
	if got := m[pk{root, e.salaries, "USD"}]; got != 10000 {
		t.Errorf("(root,salaries,USD) = %d, want 10000", got)
	}
	if got := m[pk{e.educ, e.salaries, "USD"}]; got != 6000 {
		t.Errorf("(educ,salaries,USD) = %d, want 6000", got)
	}
	// contrib: root USD T1 -20000 + T6 -8000 = -28000; root MXN T4 -5000.
	if got := m[pk{root, e.contrib, "USD"}]; got != -28000 {
		t.Errorf("(root,contrib,USD) = %d, want -28000", got)
	}
	if got := m[pk{root, e.contrib, "MXN"}]; got != -5000 {
		t.Errorf("(root,contrib,MXN) = %d, want -5000", got)
	}
	if len(cells) != 4 {
		t.Errorf("program cells = %d, want 4: %+v", len(cells), cells)
	}
}

// ============================ RegisterPage ===============================

// noFilter is the empty filter set.
var noFilter = RegisterFilters{}

// firstPage is a no-cursor page.
var firstPage = RegisterCursor{}

func TestRegisterRunningBalance(t *testing.T) {
	e := newBalEnv(t)

	// Whole checking register (single USD partition), no filters, one page.
	page, _, more, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, noFilter, 0)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	if more {
		t.Errorf("more = true, want false (limit 0 = single page)")
	}
	// Display order is DESCENDING (date DESC, split_id DESC), NEWEST on top (p26.9):
	// T6(03-05), T3(02-10), T5(01-15), T2(01-15), T1(01-05). T5 before T2 on the same
	// date because T5's split id is larger. The running balance stays the ASCENDING
	// cumulative (oldest->this-row), so the TOP row carries the latest balance (9000)
	// and it decreases downward to the oldest split.
	wantRun := []struct {
		splitID ids.SplitID
		amount  int64
		running int64
	}{
		{e.t6check, 8000, 9000},
		{e.t3check, -6000, 1000},
		{e.t5check, -3000, 7000},
		{e.t2check, -10000, 10000},
		{e.t1check, 20000, 20000},
	}
	if len(page) != len(wantRun) {
		t.Fatalf("checking page = %d rows, want %d: %+v", len(page), len(wantRun), page)
	}
	for i, w := range wantRun {
		r := page[i]
		if r.SplitID != w.splitID || r.Amount != w.amount || r.RunningBalance != w.running {
			t.Errorf("row %d = {split %d amt %d run %d}, want {split %d amt %d run %d}",
				i, r.SplitID, r.Amount, r.RunningBalance, w.splitID, w.amount, w.running)
		}
	}
}

func TestRegisterMultiCurrencyRunningBalance(t *testing.T) {
	e := newBalEnv(t)

	// fxclear is hit in USD (T5) and MXN (T4) -- two independent running balances.
	page, _, _, err := e.s.RegisterPage(e.ctx, e.fxclear, firstPage, noFilter, 0)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("fxclear page = %d rows, want 2: %+v", len(page), page)
	}
	byCcy := make(map[string]RegisterRow, 2)
	for _, r := range page {
		byCcy[r.Currency] = r
	}
	if r := byCcy["USD"]; r.SplitID != e.t5fx || r.RunningBalance != 3000 {
		t.Errorf("fxclear USD = {split %d run %d}, want {split %d run 3000}", r.SplitID, r.RunningBalance, e.t5fx)
	}
	if r := byCcy["MXN"]; r.SplitID != e.t4fx || r.RunningBalance != 5000 {
		t.Errorf("fxclear MXN = {split %d run %d}, want {split %d run 5000}", r.SplitID, r.RunningBalance, e.t4fx)
	}
}

func TestRegisterKeysetPaging(t *testing.T) {
	e := newBalEnv(t)

	// Page size 2 over the 5-row checking register. Display order is DESCENDING
	// (newest first, p26.9): [t6check, t3check, t5check, t2check, t1check]. Each page
	// walks OLDER as it appends below; the running balance is the ascending cumulative
	// (oldest->this-row), so it DECREASES down the descending sequence but each row's
	// value is unchanged from the single-page view.
	p1, c1, more1, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, noFilter, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !more1 {
		t.Errorf("page1 more = false, want true")
	}
	if len(p1) != 2 || p1[0].SplitID != e.t6check || p1[1].SplitID != e.t3check {
		t.Fatalf("page1 = %+v, want [t6check, t3check]", p1)
	}
	// Cursor is the last (oldest-shown) row of page1: t3check on 2025-02-10.
	if c1.Date != "2025-02-10" || c1.SplitID != e.t3check {
		t.Fatalf("cursor1 = %+v, want {2025-02-10, %d}", c1, e.t3check)
	}

	// Page 2 seeks past the cursor to STRICTLY OLDER rows -- t5check and t2check share
	// the date 2025-01-15, with t5check's larger split id sorting first under DESC.
	// Running balances are the same ascending cumulative values (7000, 10000): no
	// restart, no dup of the boundary row.
	p2, c2, more2, err := e.s.RegisterPage(e.ctx, e.checking, c1, noFilter, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if !more2 {
		t.Errorf("page2 more = false, want true")
	}
	if len(p2) != 2 || p2[0].SplitID != e.t5check || p2[1].SplitID != e.t2check {
		t.Fatalf("page2 = %+v, want [t5check, t2check]", p2)
	}
	if p2[0].RunningBalance != 7000 || p2[1].RunningBalance != 10000 {
		t.Errorf("page2 running = [%d,%d], want [7000,10000]", p2[0].RunningBalance, p2[1].RunningBalance)
	}

	// Page 3: the last (oldest) row, no more.
	p3, _, more3, err := e.s.RegisterPage(e.ctx, e.checking, c2, noFilter, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if more3 {
		t.Errorf("page3 more = true, want false (last page)")
	}
	if len(p3) != 1 || p3[0].SplitID != e.t1check || p3[0].RunningBalance != 20000 {
		t.Fatalf("page3 = %+v, want [t1check run 20000]", p3)
	}

	// Page 4: past the end -- empty.
	cLast := RegisterCursor{Date: p3[0].Date, SplitID: p3[0].SplitID}
	p4, _, more4, err := e.s.RegisterPage(e.ctx, e.checking, cLast, noFilter, 2)
	if err != nil {
		t.Fatalf("page4: %v", err)
	}
	if len(p4) != 0 || more4 {
		t.Errorf("page4 = %+v more=%v, want empty page, more=false", p4, more4)
	}
}

func TestRegisterFilters(t *testing.T) {
	e := newBalEnv(t)
	root := rootProgramID

	// Fund filter: checking splits tagged the grant -- T1 (+20000), T3 (-6000).
	// Displayed newest-first (p26.9): T3 on top, then T1. Running balances stay the
	// ascending cumulative (T1=20000, then T3=14000), so the top row shows 14000.
	grantID := e.grant
	fp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{FundID: &grantID}, 0)
	if err != nil {
		t.Fatalf("fund filter: %v", err)
	}
	if len(fp) != 2 || fp[0].SplitID != e.t3check || fp[1].SplitID != e.t1check {
		t.Fatalf("fund page = %+v, want [t3check, t1check]", fp)
	}
	if fp[0].RunningBalance != 14000 || fp[1].RunningBalance != 20000 {
		t.Errorf("fund running = [%d,%d], want [14000,20000]", fp[0].RunningBalance, fp[1].RunningBalance)
	}

	// Subsidiary filter: checking in subCA -- only T6 (+8000).
	subCA := int64(e.subCA)
	sp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{Subsidiary: &subCA}, 0)
	if err != nil {
		t.Fatalf("sub filter: %v", err)
	}
	if len(sp) != 1 || sp[0].SplitID != e.t6check || sp[0].RunningBalance != 8000 {
		t.Fatalf("sub page = %+v, want [t6check run 8000]", sp)
	}

	// Date filter: checking from 2025-02-01 -- T3 (-6000), T6 (+8000). Displayed
	// newest-first (p26.9): T6 on top, then T3. The window still runs over only the
	// filtered set (opening at T3), so running = [T6=2000, T3=-6000].
	dp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{From: "2025-02-01"}, 0)
	if err != nil {
		t.Fatalf("date filter: %v", err)
	}
	if len(dp) != 2 || dp[0].SplitID != e.t6check || dp[1].SplitID != e.t3check {
		t.Fatalf("date page = %+v, want [t6check, t3check]", dp)
	}
	if dp[0].RunningBalance != 2000 || dp[1].RunningBalance != -6000 {
		t.Errorf("date running = [%d,%d], want [2000,-6000]", dp[0].RunningBalance, dp[1].RunningBalance)
	}

	// Text filter: T3's memo is "rent payment" -- only T3 checking matches.
	tp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{Text: "rent"}, 0)
	if err != nil {
		t.Fatalf("text filter: %v", err)
	}
	if len(tp) != 1 || tp[0].SplitID != e.t3check {
		t.Fatalf("text page = %+v, want [t3check]", tp)
	}

	// Program filter on the SALARIES register (assets carry no program): educ -> T3
	// (+6000); root -> T2 (+10000). T7's educ salary is deleted, excluded.
	educ := e.educ
	pe, _, _, err := e.s.RegisterPage(e.ctx, e.salaries, firstPage, RegisterFilters{ProgramID: &educ}, 0)
	if err != nil {
		t.Fatalf("program filter educ: %v", err)
	}
	if len(pe) != 1 || pe[0].SplitID != e.t3sal || pe[0].Amount != 6000 {
		t.Fatalf("program educ page = %+v, want [t3sal amt 6000]", pe)
	}
	pr, _, _, err := e.s.RegisterPage(e.ctx, e.salaries, firstPage, RegisterFilters{ProgramID: &root}, 0)
	if err != nil {
		t.Fatalf("program filter root: %v", err)
	}
	if len(pr) != 1 || pr[0].SplitID != e.t2sal || pr[0].Amount != 10000 {
		t.Fatalf("program root page = %+v, want [t2sal amt 10000]", pr)
	}
}

// TestRegisterParentRollup proves p26.6: a PLACEHOLDER (parent) account's register
// rolls up ALL its descendant leaf splits into one merged, date-ordered sequence
// with a single combined running balance per currency. A parent cannot hold its own
// splits (ErrPlaceholderAccount + ledger Z2), so its register is otherwise empty.
func TestRegisterParentRollup(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := mutCtx()

	sub := newSub(t, s, rootID, "US")
	grant := mkFund(t, s, "Grant A", []ids.SubsidiaryID{sub}, nil)

	// Parent "Cash" with two leaf children BOA + WF, all mapped to sub. The children
	// hold the splits; the parent is a placeholder.
	parent, err := s.CreateAccount(ctx, CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash"), Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	boa, err := s.CreateAccount(ctx, CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD", Names: enName("BOA"), Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("create BOA: %v", err)
	}
	wf, err := s.CreateAccount(ctx, CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD", Names: enName("WF"), Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("create WF: %v", err)
	}
	// An unrelated revenue leaf (the counter side of receipts).
	contrib := mkAcct(t, s, "revenue", "Contributions", []ids.SubsidiaryID{sub}, nil, nil)
	root := rootProgramID

	e := balEnv{t: t, s: s, d: d, ctx: ctx}

	// R1 2025-01-05: BOA +10000 (grant), contrib -10000.
	_, boa1 := e.post(t, "2025-01-05", sub, "USD", "",
		split{boa, 10000, &grant, nil, nil},
		split{contrib, -10000, &grant, &root, nil})
	// R2 2025-02-10: WF +5000, contrib -5000.
	_, wf1 := e.post(t, "2025-02-10", sub, "USD", "",
		split{wf, 5000, nil, nil, nil},
		split{contrib, -5000, nil, &root, nil})
	// R3 2025-03-01: an intra-parent transfer BOA -> WF (both children of the parent),
	// same txn. In the parent register this txn appears as TWO rows (one per child) whose
	// counter-accounts differ (each names the OTHER leaf). BOA -3000, WF +3000.
	_, r3 := e.postN(t, "2025-03-01", sub, "USD", "",
		split{boa, -3000, nil, nil, nil},
		split{wf, 3000, nil, nil, nil})
	boa2, wf2 := r3[0], r3[1]

	// Parent register: merged, date-ordered, single running balance per currency.
	page, _, more, err := s.RegisterPage(ctx, parent, firstPage, noFilter, 0)
	if err != nil {
		t.Fatalf("parent RegisterPage: %v", err)
	}
	if more {
		t.Errorf("more = true, want false")
	}
	// Display order is DESCENDING (newest first, p26.9): wf2(03-01), boa2(03-01),
	// wf1(02-10), boa1(01-05). wf2 before boa2 on the same date (larger split id sorts
	// first under DESC). The running balance is still the ASCENDING cumulative across
	// the merged descendant sequence (each row's value unchanged), so the top row shows
	// the latest combined balance (15000).
	want := []struct {
		split           ids.SplitID
		amount, running int64
		acct            ids.AccountID
	}{
		{wf2, 3000, 15000, wf},
		{boa2, -3000, 12000, boa},
		{wf1, 5000, 15000, wf},
		{boa1, 10000, 10000, boa},
	}
	if len(page) != len(want) {
		t.Fatalf("parent page = %d rows, want %d: %+v", len(page), len(want), page)
	}
	for i, w := range want {
		r := page[i]
		if r.SplitID != w.split || r.Amount != w.amount || r.RunningBalance != w.running || r.AccountID != w.acct {
			t.Errorf("row %d = {split %d amt %d run %d acct %d}, want {split %d amt %d run %d acct %d}",
				i, r.SplitID, r.Amount, r.RunningBalance, r.AccountID,
				w.split, w.amount, w.running, w.acct)
		}
	}

	// A fund filter still narrows: only the grant-tagged BOA split (R1) remains.
	fp, _, _, err := s.RegisterPage(ctx, parent, firstPage, RegisterFilters{FundID: &grant}, 0)
	if err != nil {
		t.Fatalf("parent fund filter: %v", err)
	}
	if len(fp) != 1 || fp[0].SplitID != boa1 || fp[0].RunningBalance != 10000 {
		t.Fatalf("parent fund page = %+v, want [boa1 run 10000]", fp)
	}

	// A LEAF register still rolls up only BOA's own two splits, now displayed
	// newest-first (p26.9): boa2 on top, then boa1. Running balances stay ascending
	// cumulative (boa1=10000, boa2=7000), so the top row shows 7000.
	lp, _, _, err := s.RegisterPage(ctx, boa, firstPage, noFilter, 0)
	if err != nil {
		t.Fatalf("leaf RegisterPage: %v", err)
	}
	if len(lp) != 2 || lp[0].SplitID != boa2 || lp[1].SplitID != boa1 {
		t.Fatalf("leaf page = %+v, want [boa2, boa1]", lp)
	}
	if lp[0].RunningBalance != 7000 || lp[1].RunningBalance != 10000 {
		t.Errorf("leaf running = [%d,%d], want [7000,10000]", lp[0].RunningBalance, lp[1].RunningBalance)
	}
	if lp[0].AccountID != boa || lp[1].AccountID != boa {
		t.Errorf("leaf rows account = [%d,%d], want both %d", lp[0].AccountID, lp[1].AccountID, boa)
	}
}

// ============================ deleted exclusion ==========================

// TestDeletedTransactionExcluded proves T7 (soft-deleted) is absent from EVERY
// query. Each expected number in the other tests already excludes T7; this test
// makes the exclusion explicit by checking the cells T7 would have perturbed.
func TestDeletedTransactionExcluded(t *testing.T) {
	e := newBalEnv(t)

	// SubtreeBalancesAsOf: salaries USD would be 16000+99999 if T7 counted.
	sb := acctMap(e.subtree("2025-12-31", rootID))
	wantCell(t, sb, e.salaries, "USD", 16000)
	wantCell(t, sb, e.checking, "USD", 9000)

	// PeriodActivity over January (T7 is 2025-01-10) must not include it.
	jan := acctMap(e.period("2025-01-01", "2025-01-31", rootID))
	wantCell(t, jan, e.salaries, "USD", 10000) // T2 only
	wantCell(t, jan, e.checking, "USD", 7000)  // T1+T2+T5 (no T7 -99999)

	// FundBalancesAsOf: grant would be 14000-99999 if T7 counted.
	fb := e.fundBalances("2025-12-31", rootID)
	var grantUSD int64
	found := false
	for _, c := range fb {
		if c.FundID == e.grant && c.Currency == "USD" {
			grantUSD = c.Amount
			found = true
		}
	}
	if !found || grantUSD != 14000 {
		t.Errorf("grant USD = %d (found %v), want 14000 (T7 excluded)", grantUSD, found)
	}

	// FunctionalActivity: salaries/management would be 16000+99999.
	fa, err := e.s.FunctionalActivity(e.ctx, "2025-01-01", "2025-12-31", rootID)
	if err != nil {
		t.Fatalf("FunctionalActivity: %v", err)
	}
	if len(fa) != 1 || fa[0].Amount != 16000 {
		t.Errorf("functional = %+v, want single 16000 cell (T7 excluded)", fa)
	}

	// ProgramActivity: (educ, salaries) would be 6000+99999.
	pa, err := e.s.ProgramActivity(e.ctx, "2025-01-01", "2025-12-31", rootID)
	if err != nil {
		t.Fatalf("ProgramActivity: %v", err)
	}
	for _, c := range pa {
		if c.ProgramID == e.educ && c.AccountID == e.salaries && c.Amount != 6000 {
			t.Errorf("(educ,salaries) = %d, want 6000 (T7 excluded)", c.Amount)
		}
	}

	// RegisterPage on checking: T7's -99999 split must not appear.
	page, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, noFilter, 0)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	if len(page) != 5 {
		t.Errorf("checking register = %d rows, want 5 (T7 excluded)", len(page))
	}
	for _, r := range page {
		if r.Amount == -99999 {
			t.Errorf("register contains T7's deleted split %+v", r)
		}
	}
}

// --- small result unwrappers -----------------------------------------------
//
// subtree/period/fund wrap a query call + error check into ONE value so the
// existing map helpers can consume them (Go forbids f(t, g()) when g is
// multivalue).
func (e balEnv) subtree(asof string, scope ids.SubsidiaryID) []AccountCurrencyAmount {
	e.t.Helper()
	cells, err := e.s.SubtreeBalancesAsOf(e.ctx, asof, scope)
	if err != nil {
		e.t.Fatalf("SubtreeBalancesAsOf: %v", err)
	}
	return cells
}

func (e balEnv) period(from, to string, scope ids.SubsidiaryID) []AccountCurrencyAmount {
	e.t.Helper()
	cells, err := e.s.PeriodActivity(e.ctx, from, to, scope)
	if err != nil {
		e.t.Fatalf("PeriodActivity: %v", err)
	}
	return cells
}

func (e balEnv) fundBalances(asof string, scope ids.SubsidiaryID) []FundCurrencyAmount {
	e.t.Helper()
	cells, err := e.s.FundBalancesAsOf(e.ctx, asof, scope)
	if err != nil {
		e.t.Fatalf("FundBalancesAsOf: %v", err)
	}
	return cells
}

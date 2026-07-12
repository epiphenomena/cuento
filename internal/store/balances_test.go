package store

import (
	"context"
	"database/sql"
	"testing"

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

	subUS, subCA int64
	educ         int64
	grant        int64

	checking, fxclear, contrib, salaries int64

	// checking split ids in post order, for cursor assertions.
	t1check, t2check, t5check, t3check, t6check int64
	// salaries split ids for the program-filter register.
	t2sal, t3sal int64
	// fxclear split ids for the multi-currency register.
	t5fx, t4fx int64
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
	grant := mkFund(t, s, "Grant A", []int64{subUS, subCA}, nil)

	root := rootProgramID
	mgmt := "management"
	checking := mkAcct(t, s, "asset", "Checking", []int64{subUS, subCA}, nil, nil)
	fxclear := mkAcct(t, s, "asset", "FX Clearing", []int64{subUS}, nil, nil)
	contrib := mkAcct(t, s, "revenue", "Contributions", []int64{subUS, subCA}, nil, &root)
	salaries := mkAcct(t, s, "expense", "Salaries", []int64{subUS, subCA}, &mgmt, &root)

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
	acct   int64
	amount int64
	fund   *int64
	prog   *int64
	fclass *string
}

// post posts a transaction and returns (txnID, firstSplitID). Used when only the
// first split id is captured; postN returns all split ids.
func (e balEnv) post(t *testing.T, date string, sub int64, ccy, memo string, sp ...split) (int64, int64) {
	t.Helper()
	id, ids := e.postN(t, date, sub, ccy, memo, sp...)
	return id, ids[0]
}

// postN posts a transaction and returns (txnID, splitIDsInOrder).
func (e balEnv) postN(t *testing.T, date string, sub int64, ccy, memo string, sp ...split) (int64, []int64) {
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
	ids := splitIDsInOrder(t, e.d, id)
	if len(ids) != len(sp) {
		t.Fatalf("post %s: got %d split ids, want %d", date, len(ids), len(sp))
	}
	return id, ids
}

// splitIDsInOrder returns a transaction's live split ids ordered by (position, id).
func splitIDsInOrder(t *testing.T, d *sql.DB, txnID int64) []int64 {
	t.Helper()
	rows, err := d.Query(`SELECT id FROM splits WHERE transaction_id = ? ORDER BY position, id`, txnID)
	if err != nil {
		t.Fatalf("splitIDsInOrder: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
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

func key2(id int64, ccy string) string { return ccyKey(id) + "/" + ccy }

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

func wantCell(t *testing.T, m map[string]int64, id int64, ccy string, want int64) {
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

func wantNoCell(t *testing.T, m map[string]int64, id int64, ccy string) {
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
	wantCell(t, m, 0, "USD", -2000)
	// unrestricted MXN: T4 fxclear +5000.
	wantCell(t, m, 0, "MXN", 5000)
	if len(cells) != 3 {
		t.Errorf("fund cells = %d, want 3: %+v", len(cells), cells)
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
		prog, acct int64
		ccy        string
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
	// Order (date, split_id): T1(01-05), T2(01-15), T5(01-15), T3(02-10), T6(03-05).
	// T2 before T5 on the same date because T2's split id is smaller (posted first).
	wantRun := []struct {
		splitID int64
		amount  int64
		running int64
	}{
		{e.t1check, 20000, 20000},
		{e.t2check, -10000, 10000},
		{e.t5check, -3000, 7000},
		{e.t3check, -6000, 1000},
		{e.t6check, 8000, 9000},
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

	// Page size 2 over the 5-row checking register.
	p1, c1, more1, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, noFilter, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !more1 {
		t.Errorf("page1 more = false, want true")
	}
	if len(p1) != 2 || p1[0].SplitID != e.t1check || p1[1].SplitID != e.t2check {
		t.Fatalf("page1 = %+v, want [t1check, t2check]", p1)
	}
	// Cursor is the last row of page1 (same-date boundary: t2check on 2025-01-15).
	if c1.Date != "2025-01-15" || c1.SplitID != e.t2check {
		t.Fatalf("cursor1 = %+v, want {2025-01-15, %d}", c1, e.t2check)
	}

	// Page 2 seeks past the cursor -- t5check shares the date but has a larger split
	// id, so it is the first row (the same-date boundary case). Running balance
	// CONTINUES from page1's last (10000 -> 7000), not restart.
	p2, c2, more2, err := e.s.RegisterPage(e.ctx, e.checking, c1, noFilter, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if !more2 {
		t.Errorf("page2 more = false, want true")
	}
	if len(p2) != 2 || p2[0].SplitID != e.t5check || p2[1].SplitID != e.t3check {
		t.Fatalf("page2 = %+v, want [t5check, t3check]", p2)
	}
	if p2[0].RunningBalance != 7000 {
		t.Errorf("page2 first running = %d, want 7000 (continues from page1's 10000)", p2[0].RunningBalance)
	}

	// Page 3: the last row, no more.
	p3, _, more3, err := e.s.RegisterPage(e.ctx, e.checking, c2, noFilter, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if more3 {
		t.Errorf("page3 more = true, want false (last page)")
	}
	if len(p3) != 1 || p3[0].SplitID != e.t6check || p3[0].RunningBalance != 9000 {
		t.Fatalf("page3 = %+v, want [t6check run 9000]", p3)
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
	grantID := e.grant
	fp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{FundID: &grantID}, 0)
	if err != nil {
		t.Fatalf("fund filter: %v", err)
	}
	if len(fp) != 2 || fp[0].SplitID != e.t1check || fp[1].SplitID != e.t3check {
		t.Fatalf("fund page = %+v, want [t1check, t3check]", fp)
	}
	if fp[0].RunningBalance != 20000 || fp[1].RunningBalance != 14000 {
		t.Errorf("fund running = [%d,%d], want [20000,14000]", fp[0].RunningBalance, fp[1].RunningBalance)
	}

	// Subsidiary filter: checking in subCA -- only T6 (+8000).
	subCA := e.subCA
	sp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{Subsidiary: &subCA}, 0)
	if err != nil {
		t.Fatalf("sub filter: %v", err)
	}
	if len(sp) != 1 || sp[0].SplitID != e.t6check || sp[0].RunningBalance != 8000 {
		t.Fatalf("sub page = %+v, want [t6check run 8000]", sp)
	}

	// Date filter: checking from 2025-02-01 -- T3 (-6000), T6 (+8000).
	dp, _, _, err := e.s.RegisterPage(e.ctx, e.checking, firstPage, RegisterFilters{From: "2025-02-01"}, 0)
	if err != nil {
		t.Fatalf("date filter: %v", err)
	}
	if len(dp) != 2 || dp[0].SplitID != e.t3check || dp[1].SplitID != e.t6check {
		t.Fatalf("date page = %+v, want [t3check, t6check]", dp)
	}
	if dp[0].RunningBalance != -6000 || dp[1].RunningBalance != 2000 {
		t.Errorf("date running = [%d,%d], want [-6000,2000]", dp[0].RunningBalance, dp[1].RunningBalance)
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
func (e balEnv) subtree(asof string, scope int64) []AccountCurrencyAmount {
	e.t.Helper()
	cells, err := e.s.SubtreeBalancesAsOf(e.ctx, asof, scope)
	if err != nil {
		e.t.Fatalf("SubtreeBalancesAsOf: %v", err)
	}
	return cells
}

func (e balEnv) period(from, to string, scope int64) []AccountCurrencyAmount {
	e.t.Helper()
	cells, err := e.s.PeriodActivity(e.ctx, from, to, scope)
	if err != nil {
		e.t.Fatalf("PeriodActivity: %v", err)
	}
	return cells
}

func (e balEnv) fundBalances(asof string, scope int64) []FundCurrencyAmount {
	e.t.Helper()
	cells, err := e.s.FundBalancesAsOf(e.ctx, asof, scope)
	if err != nil {
		e.t.Fatalf("FundBalancesAsOf: %v", err)
	}
	return cells
}

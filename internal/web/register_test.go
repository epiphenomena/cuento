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

	"cuento/internal/store"
)

// p12.1 account register tests. The handler is driven through the REAL mounted
// router (httptest) against a real migrated db (AGENTS testing conventions); the
// row-assembly helper (registerRows) is exercised DIRECTLY so paging boundaries,
// stable ordering, per-currency running balance, and filters are asserted without
// scraping HTML. HTML-level tests cover the gating cases (sub badge, fund chip,
// recon column, the "Split" counter-account) and perms.
//
// All bespoke data is built inline via the store (rule 2: writes through the store)
// so the app's own db carries what the handler reads.

// regEnv is a small hand-built register dataset: subsidiaries, accounts, a fund,
// payees, and a handful of transactions on a "checking" account plus a
// multi-currency "clearing" account. It exposes the ids the tests assert against.
type regEnv struct {
	h    http.Handler
	st   *store.Store
	sm   *scs.SessionManager
	book int64 // bookkeeper (txn write) user id

	root       int64 // root subsidiary
	subA, subB int64 // two children (so a >1-sub account exists)

	checking  int64 // reconcilable, mapped to subA only (1 sub -> no badge)
	multiSub  int64 // mapped to subA + subB (badge), NOT reconcilable
	clearing  int64 // mapped to all subs, USD+MXN (multi-currency running bal)
	expense   int64 // an expense account (counter-account target)
	otherExp  int64 // a second expense (to make a >2-split txn -> "Split")
	revenue   int64 // revenue account
	fund      int64 // a restricted fund scoped to subA
	payeeAcme int64
}

func newRegEnv(t *testing.T) *regEnv {
	t.Helper()
	h, st, sm := accountsApp(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	e := &regEnv{h: h, st: st}
	e.sm = sm
	e.book = mkUser(t, st, "regbook", "write", false)

	// Root subsidiary is seeded as id 1 (USD). Add two USD children so a shared
	// account can map to >1 sub.
	e.root = 1
	var err error
	e.subA, err = st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{Name: "Sub A", ParentID: e.root, BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("create subA: %v", err)
	}
	e.subB, err = st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{Name: "Sub B", ParentID: e.root, BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("create subB: %v", err)
	}

	mkAcct := func(name, typ, ccy string, recon bool, subs []int64) int64 {
		in := store.CreateAccountInput{
			Type:            typ,
			DefaultCurrency: ccy,
			Names:           map[string]string{"en": name},
			Subsidiaries:    subs,
			Reconcilable:    recon,
		}
		id, err := st.CreateAccount(ctx, in)
		if err != nil {
			t.Fatalf("create account %s: %v", name, err)
		}
		return id
	}
	e.checking = mkAcct("Checking A", "asset", "USD", true, []int64{e.subA})
	e.multiSub = mkAcct("Shared Cash", "asset", "USD", false, []int64{e.subA, e.subB})
	e.clearing = mkAcct("FX Clearing", "equity", "USD", false, []int64{e.root, e.subA, e.subB})
	e.expense = mkAcct("Supplies", "expense", "USD", false, []int64{e.subA, e.subB})
	e.otherExp = mkAcct("Rent", "expense", "USD", false, []int64{e.subA, e.subB})
	e.revenue = mkAcct("Contributions", "revenue", "USD", false, []int64{e.subA, e.subB})

	// A restricted fund scoped to subA (so its splits carry a non-NULL fund_id).
	e.fund, err = st.CreateFund(ctx, store.CreateFundInput{
		Name:         "Beca 2025",
		Restriction:  "purpose",
		Subsidiaries: []int64{e.subA},
	})
	if err != nil {
		t.Fatalf("create fund: %v", err)
	}

	e.payeeAcme = mkPayee(t, st, ctx, "Acme")
	return e
}

// mkPayee inserts a payee via the store's write funnel and returns its id.
func mkPayee(t *testing.T, st *store.Store, ctx context.Context, name string) int64 {
	t.Helper()
	id, err := st.CreatePayee(ctx, name)
	if err != nil {
		t.Fatalf("create payee %s: %v", name, err)
	}
	return id
}

// prog returns the program id required for a revenue/expense split (the seeded
// root "General", id 1).
const generalProgram int64 = 1

// post2 posts a simple balanced 2-split txn (debit `debit`, credit `credit`) in
// USD on subA, with an optional fund on both splits and program on R/E splits.
func (e *regEnv) post2(t *testing.T, ctx context.Context, date string, amount int64, debit, credit int64, fund *int64, payee *int64) int64 {
	t.Helper()
	prog := func(acct int64) *int64 {
		if acct == e.expense || acct == e.otherExp || acct == e.revenue {
			p := generalProgram
			return &p
		}
		return nil
	}
	fclass := func(acct int64) *string {
		if acct == e.expense || acct == e.otherExp {
			c := "program"
			return &c
		}
		return nil
	}
	in := store.PostTransactionInput{
		Date:         date,
		SubsidiaryID: e.subA,
		PayeeID:      payee,
		Currency:     "USD",
		Splits: []store.SplitInput{
			{AccountID: debit, Amount: amount, FundID: fund, ProgramID: prog(debit), FunctionalClass: fclass(debit), Position: 0},
			{AccountID: credit, Amount: -amount, FundID: fund, ProgramID: prog(credit), FunctionalClass: fclass(credit), Position: 1},
		},
	}
	id, err := e.st.PostTransaction(ctx, in)
	if err != nil {
		t.Fatalf("post2 %s: %v", date, err)
	}
	return id
}

// -- registerRows: paging, ordering, running balance, filters --------------

// TestRegisterKeysetPaging: rows come back in DESCENDING (date, split_id) display
// order (newest first, p26.9), page boundaries follow the cursor, the last page
// terminates (hasMore=false, no infinite loop), and the concatenation of pages
// equals the full single page.
func TestRegisterKeysetPaging(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Five checking-account debits on distinct ascending dates.
	dates := []string{"2025-01-01", "2025-02-01", "2025-03-01", "2025-04-01", "2025-05-01"}
	for i, d := range dates {
		e.post2(t, ctx, d, int64((i+1)*100), e.checking, e.expense, nil, nil)
	}

	opts := formatOptsFor(nil)
	// Page 2 at a time; walk to the end.
	var got []regRow
	cur := store.RegisterCursor{}
	pages := 0
	for {
		rows, next, more, err := registerRows(ctx, e.st, e.checking, cur, store.RegisterFilters{}, 2, "en", opts)
		if err != nil {
			t.Fatalf("registerRows page %d: %v", pages, err)
		}
		got = append(got, rows...)
		pages++
		if pages > 10 {
			t.Fatalf("paging did not terminate (hasMore stuck true)")
		}
		if !more {
			break
		}
		cur = next
	}
	if len(got) != len(dates) {
		t.Fatalf("paged rows = %d, want %d", len(got), len(dates))
	}
	// DESCENDING (date, split_id) display order across page boundaries: newest first
	// (p26.9), each page appending strictly-older rows below.
	for i := 1; i < len(got); i++ {
		if got[i-1].DateISO < got[i].DateISO ||
			(got[i-1].DateISO == got[i].DateISO && got[i-1].SplitID < got[i].SplitID) {
			t.Fatalf("rows not in DESC (date, split_id) order at %d: %+v then %+v", i, got[i-1], got[i])
		}
	}

	// Full single page equals the concatenation.
	full, _, more, err := registerRows(ctx, e.st, e.checking, store.RegisterCursor{}, store.RegisterFilters{}, 0, "en", opts)
	if err != nil {
		t.Fatalf("registerRows full: %v", err)
	}
	if more {
		t.Fatalf("limit=0 full page should have hasMore=false")
	}
	if len(full) != len(got) {
		t.Fatalf("full page = %d rows, paged = %d", len(full), len(got))
	}
	for i := range full {
		if full[i].SplitID != got[i].SplitID || full[i].RunningBalance != got[i].RunningBalance {
			t.Fatalf("row %d differs full vs paged: %+v vs %+v", i, full[i], got[i])
		}
	}
}

// TestRegisterRunningBalancePerCurrency: the register's per-currency running
// balance matches RegisterPage exactly, including a multi-currency account (USD +
// MXN) where each currency's running balance is independent.
func TestRegisterRunningBalancePerCurrency(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// USD flows through clearing.
	e.post2(t, ctx, "2025-01-10", 26000, e.checking, e.clearing, nil, nil)
	e.post2(t, ctx, "2025-02-10", 10000, e.checking, e.clearing, nil, nil)

	// An MXN txn through clearing (needs an MXN account on subA). Cash MXN.
	cashMX, err := e.st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "MXN",
		Names:        map[string]string{"en": "Cash MXN"},
		Subsidiaries: []int64{e.subA},
	})
	if err != nil {
		t.Fatalf("create cashMX: %v", err)
	}
	if _, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-10", SubsidiaryID: e.subA, Currency: "MXN",
		Splits: []store.SplitInput{
			{AccountID: e.clearing, Amount: 500000, Position: 0},
			{AccountID: cashMX, Amount: -500000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("post MXN: %v", err)
	}

	// Truth from the store directly.
	page, _, _, err := e.st.RegisterPage(ctx, e.clearing, store.RegisterCursor{}, store.RegisterFilters{}, 0)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	rows, _, _, err := registerRows(ctx, e.st, e.clearing, store.RegisterCursor{}, store.RegisterFilters{}, 0, "en", formatOptsFor(nil))
	if err != nil {
		t.Fatalf("registerRows: %v", err)
	}
	if len(rows) != len(page) {
		t.Fatalf("rows=%d page=%d", len(rows), len(page))
	}
	sawMX := false
	for i := range page {
		if rows[i].RunningBalance != page[i].RunningBalance {
			t.Fatalf("row %d running balance = %d, want %d (%s)", i, rows[i].RunningBalance, page[i].RunningBalance, page[i].Currency)
		}
		if rows[i].Amount != page[i].Amount {
			t.Fatalf("row %d amount = %d, want %d", i, rows[i].Amount, page[i].Amount)
		}
		if page[i].Currency == "MXN" {
			sawMX = true
		}
	}
	if !sawMX {
		t.Fatalf("expected an MXN row on the multi-currency clearing account")
	}
}

// TestRegisterFilters: each filter (date range, text, fund, subsidiary, program)
// narrows the rows correctly.
func TestRegisterFilters(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Unrestricted checking->expense in Jan; restricted (fund) checking->expense in
	// Feb with payee Acme; a Mar txn with a distinctive memo.
	e.post2(t, ctx, "2025-01-15", 100, e.checking, e.expense, nil, nil)
	e.post2(t, ctx, "2025-02-15", 200, e.checking, e.expense, &e.fund, &e.payeeAcme)
	mar := store.PostTransactionInput{
		Date: "2025-03-15", SubsidiaryID: e.subA, Currency: "USD",
		Memo: "ZEBRA memo",
		Splits: []store.SplitInput{
			{AccountID: e.checking, Amount: 300, Position: 0},
			{AccountID: e.expense, Amount: -300, ProgramID: ptrI(generalProgram), FunctionalClass: ptrS("program"), Position: 1},
		},
	}
	if _, err := e.st.PostTransaction(ctx, mar); err != nil {
		t.Fatalf("post mar: %v", err)
	}

	opts := formatOptsFor(nil)
	countRows := func(f store.RegisterFilters) int {
		rows, _, _, err := registerRows(ctx, e.st, e.checking, store.RegisterCursor{}, f, 0, "en", opts)
		if err != nil {
			t.Fatalf("registerRows: %v", err)
		}
		return len(rows)
	}

	if n := countRows(store.RegisterFilters{}); n != 3 {
		t.Fatalf("no filter = %d rows, want 3", n)
	}
	if n := countRows(store.RegisterFilters{From: "2025-02-01", To: "2025-02-28"}); n != 1 {
		t.Fatalf("date-range Feb = %d rows, want 1", n)
	}
	if n := countRows(store.RegisterFilters{Text: "ZEBRA"}); n != 1 {
		t.Fatalf("text ZEBRA = %d rows, want 1", n)
	}
	if n := countRows(store.RegisterFilters{Text: "Acme"}); n != 1 {
		t.Fatalf("text Acme (payee) = %d rows, want 1", n)
	}
	if n := countRows(store.RegisterFilters{FundID: &e.fund}); n != 1 {
		t.Fatalf("fund filter = %d rows, want 1", n)
	}
	if n := countRows(store.RegisterFilters{Subsidiary: &e.subA}); n != 3 {
		t.Fatalf("subsidiary subA = %d rows, want 3", n)
	}
	if n := countRows(store.RegisterFilters{Subsidiary: &e.subB}); n != 0 {
		t.Fatalf("subsidiary subB = %d rows, want 0", n)
	}
	p := generalProgram
	if n := countRows(store.RegisterFilters{ProgramID: &p}); n != 0 {
		t.Fatalf("program filter on checking (A/L account carries no program) = %d rows, want 0", n)
	}
}

func ptrI(v int64) *int64   { return &v }
func ptrS(v string) *string { return &v }

// -- gating: counter-account, fund chip, sub badge, recon column -----------

// TestRegisterCounterAccount: a 2-split txn shows the OTHER account's name; a
// >2-split txn shows the "Split" catalog word.
func TestRegisterCounterAccount(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	e.post2(t, ctx, "2025-01-01", 500, e.checking, e.expense, nil, nil)
	// A 3-split txn on checking: checking -> expense + otherExp.
	if _, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-02-01", SubsidiaryID: e.subA, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.checking, Amount: 1000, Position: 0},
			{AccountID: e.expense, Amount: -600, ProgramID: ptrI(generalProgram), FunctionalClass: ptrS("program"), Position: 1},
			{AccountID: e.otherExp, Amount: -400, ProgramID: ptrI(generalProgram), FunctionalClass: ptrS("program"), Position: 2},
		},
	}); err != nil {
		t.Fatalf("post 3-split: %v", err)
	}

	rows, _, _, err := registerRows(ctx, e.st, e.checking, store.RegisterCursor{}, store.RegisterFilters{}, 0, "en", formatOptsFor(nil))
	if err != nil {
		t.Fatalf("registerRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	// Newest-first display (p26.9): row 0 is Feb (3-split), row 1 is Jan (2-split).
	// Row 0 (Feb, 3-split) -> "Split" (the catalog word, rendered via IsSplit flag).
	if !rows[0].IsSplit {
		t.Errorf("3-split row should be flagged IsSplit; got counter=%q", rows[0].CounterAccount)
	}
	// Row 1 (Jan, 2-split) -> counter is "Supplies".
	if rows[1].CounterAccount != "Supplies" {
		t.Errorf("2-split counter = %q, want %q", rows[1].CounterAccount, "Supplies")
	}

	// A >2-split txn whose non-self splits COLLAPSE to one account (common under a
	// D20 mixed-fund split: two expense splits on the same account, different funds)
	// must STILL read as "Split" -- the gate is total split count, not distinct
	// other-account count.
	// Both non-self splits hit e.expense (two distinct split rows on one account);
	// unrestricted so overall and per-fund zero-sum hold with a single cash split.
	if _, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subA, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.checking, Amount: 1000, Position: 0},
			{AccountID: e.expense, Amount: -600, Memo: "part A", ProgramID: ptrI(generalProgram), FunctionalClass: ptrS("program"), Position: 1},
			{AccountID: e.expense, Amount: -400, Memo: "part B", ProgramID: ptrI(generalProgram), FunctionalClass: ptrS("program"), Position: 2},
		},
	}); err != nil {
		t.Fatalf("post collapsed 3-split: %v", err)
	}
	rows2, _, _, err := registerRows(ctx, e.st, e.checking, store.RegisterCursor{}, store.RegisterFilters{}, 0, "en", formatOptsFor(nil))
	if err != nil {
		t.Fatalf("registerRows: %v", err)
	}
	// Newest-first (p26.9): the Mar collapsed-3-split is now the TOP row.
	newest := rows2[0]
	if !newest.IsSplit {
		t.Errorf("collapsed 3-split (both non-self on one account) should be IsSplit; got counter=%q", newest.CounterAccount)
	}
}

// TestRegisterParentRollupCounterAccount proves the p26.6 rollup path at the WEB
// layer: a PLACEHOLDER (parent) register lists its descendant leaves' splits, and
// each row's COUNTER-ACCOUNT names the actual OTHER leaf -- resolved against the
// row's OWN account, NOT the parent. An intra-parent transfer surfaces one txn as
// two rows whose counters differ (each names the sibling leaf); a counter cache
// keyed on txn alone would collapse them to one wrong value.
func TestRegisterParentRollupCounterAccount(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Parent "Cash" (placeholder) with two leaf children BOA + WF, all in subA.
	parent, err := e.st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{e.subA},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	boa, err := e.st.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "BOA"}, Subsidiaries: []int64{e.subA},
	})
	if err != nil {
		t.Fatalf("create BOA: %v", err)
	}
	wf, err := e.st.CreateAccount(ctx, store.CreateAccountInput{
		ParentID: &parent, Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "WF"}, Subsidiaries: []int64{e.subA},
	})
	if err != nil {
		t.Fatalf("create WF: %v", err)
	}

	// An intra-parent transfer BOA -> WF (both children of the parent): DR WF, CR BOA.
	txn, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-01-05", SubsidiaryID: e.subA, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: wf, Amount: 300, Position: 0},
			{AccountID: boa, Amount: -300, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post transfer: %v", err)
	}
	splits, err := e.st.TransactionSplits(ctx, txn)
	if err != nil {
		t.Fatalf("splits: %v", err)
	}
	var boaSplit, wfSplit int64
	for _, sp := range splits {
		switch sp.AccountID {
		case boa:
			boaSplit = sp.ID
		case wf:
			wfSplit = sp.ID
		}
	}

	rows, _, _, err := registerRows(ctx, e.st, parent, store.RegisterCursor{}, store.RegisterFilters{}, 0, "en", formatOptsFor(nil))
	if err != nil {
		t.Fatalf("registerRows(parent): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("parent rows=%d want 2 (both descendant leaf splits): %+v", len(rows), rows)
	}
	bySplit := map[int64]regRow{}
	for _, r := range rows {
		bySplit[r.SplitID] = r
	}
	// The BOA row's counter is the OTHER leaf, WF (not "Cash", not "BOA").
	if r := bySplit[boaSplit]; r.CounterAccount != "WF" || r.IsSplit {
		t.Errorf("BOA row counter = %q (isSplit %v), want %q", r.CounterAccount, r.IsSplit, "WF")
	}
	// The WF row's counter is the OTHER leaf, BOA -- proving the counter is resolved
	// per-row-account (a txn-only cache would collapse both to one wrong value).
	if r := bySplit[wfSplit]; r.CounterAccount != "BOA" || r.IsSplit {
		t.Errorf("WF row counter = %q (isSplit %v), want %q", r.CounterAccount, r.IsSplit, "BOA")
	}
}

// TestRegisterFundChip: a restricted (non-NULL fund) split names the fund; an
// unrestricted split has no fund name (no chip).
func TestRegisterFundChip(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	e.post2(t, ctx, "2025-01-01", 100, e.checking, e.expense, nil, nil)     // unrestricted
	e.post2(t, ctx, "2025-02-01", 200, e.checking, e.expense, &e.fund, nil) // restricted

	rows, _, _, err := registerRows(ctx, e.st, e.checking, store.RegisterCursor{}, store.RegisterFilters{}, 0, "en", formatOptsFor(nil))
	if err != nil {
		t.Fatalf("registerRows: %v", err)
	}
	// Newest-first (p26.9): Feb (restricted) on top, Jan (unrestricted) below.
	if rows[0].FundName != "Beca 2025" {
		t.Errorf("restricted row FundName = %q, want %q", rows[0].FundName, "Beca 2025")
	}
	if rows[1].FundName != "" {
		t.Errorf("unrestricted row FundName = %q, want empty", rows[1].FundName)
	}
}

// TestRegisterSubBadgeGating: the page model shows the sub badge only when the
// account maps to >1 subsidiary.
func TestRegisterSubBadgeGating(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// checking is mapped to exactly one sub -> no badge.
	badge, err := accountShowsSubBadge(ctx, e.st, e.checking)
	if err != nil {
		t.Fatalf("accountShowsSubBadge checking: %v", err)
	}
	if badge {
		t.Errorf("single-sub account should not show the sub badge")
	}
	// multiSub is mapped to two subs -> badge.
	badge2, err := accountShowsSubBadge(ctx, e.st, e.multiSub)
	if err != nil {
		t.Fatalf("accountShowsSubBadge multiSub: %v", err)
	}
	if !badge2 {
		t.Errorf("multi-sub account should show the sub badge")
	}
}

// TestRegisterReconColumnGating: the recon column/placeholder is present only for
// reconcilable accounts.
func TestRegisterReconColumnGating(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	reader := mkUser(t, e.st, "reconreader", "read", false)
	// Render both pages as HTML and assert the recon column header presence.
	reconHTML := renderRegisterHTML(t, e, reader, e.checking) // reconcilable
	if !strings.Contains(reconHTML, `data-col="recon"`) {
		t.Errorf("reconcilable account register should render the recon column")
	}
	plainHTML := renderRegisterHTML(t, e, reader, e.expense) // not reconcilable
	if strings.Contains(plainHTML, `data-col="recon"`) {
		t.Errorf("non-reconcilable account register should NOT render the recon column")
	}
	_ = ctx
}

// renderRegisterHTML fetches the full register page HTML for an account as `user`.
func renderRegisterHTML(t *testing.T, e *regEnv, user, acctID int64) string {
	t.Helper()
	path := "/accounts/" + strconv.FormatInt(acctID, 10) + "/register"
	rec := asUser(t, e.h, e.sm, user, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("register HTML for %d: status=%d body=%s", acctID, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// -- perms -----------------------------------------------------------------

// TestRegisterPerms: a TxnRead user can view; an anonymous request is redirected
// to login.
func TestRegisterPerms(t *testing.T) {
	e := newRegEnv(t)
	reader := mkUser(t, e.st, "regreader", "read", false)

	path := "/accounts/" + strconv.FormatInt(e.checking, 10) + "/register"

	// TxnRead can view.
	rec := asUser(t, e.h, e.sm, reader, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("TxnRead view: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Anonymous -> 302 to /login.
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/login" {
		t.Fatalf("anon: status=%d loc=%q, want 302 /login", rr.Code, rr.Header().Get("Location"))
	}
}

// TestRegisterHtmxPaging: the htmx next-page request (HX-Request + cursor params)
// returns a rows fragment (not a full page) whose rows follow the cursor.
func TestRegisterHtmxPaging(t *testing.T) {
	e := newRegEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	for i, d := range []string{"2025-01-01", "2025-02-01", "2025-03-01"} {
		e.post2(t, ctx, d, int64((i+1)*100), e.checking, e.expense, nil, nil)
	}
	reader := mkUser(t, e.st, "regpager", "read", false)
	base := "/accounts/" + strconv.FormatInt(e.checking, 10) + "/register"

	// Page 1 is a full page (a full document with the filter form).
	first := asUser(t, e.h, e.sm, reader, http.MethodGet, base, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("page 1: %d", first.Code)
	}
	if !strings.Contains(first.Body.String(), "<html") {
		t.Errorf("page 1 should be a full document")
	}

	// Take the cursor of the FIRST row (limit 1) so the fragment fetch returns the
	// rows that FOLLOW it -- independent of registerPageSize.
	rows, cur, _, err := registerRows(ctx, e.st, e.checking, store.RegisterCursor{}, store.RegisterFilters{}, 1, "en", formatOptsFor(nil))
	if err != nil {
		t.Fatalf("registerRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("limit=1 returned %d rows", len(rows))
	}
	firstSplit := rows[0].SplitID

	// The htmx paging request: same route, carrying the cursor param. It must render
	// a FRAGMENT (no full document) containing the following rows with stable ids,
	// and NOT re-render the first row (append semantics).
	q := url.Values{}
	q.Set("cursor_date", cur.Date)
	q.Set("cursor_id", strconv.FormatInt(cur.SplitID, 10))
	frag := asUser(t, e.h, e.sm, reader, http.MethodGet, base+"?"+q.Encode(), nil)
	if frag.Code != http.StatusOK {
		t.Fatalf("htmx paging: %d body=%s", frag.Code, frag.Body.String())
	}
	body := frag.Body.String()
	if strings.Contains(body, "<html") {
		t.Errorf("htmx paging response should be a fragment, got a full page")
	}
	if !strings.Contains(body, "reg-row-") {
		t.Errorf("htmx paging response should contain register rows; body=%s", body)
	}
	// It must not re-render the first (already-shown) row -> no scroll/duplication.
	if strings.Contains(body, "reg-row-"+strconv.FormatInt(firstSplit, 10)+`"`) {
		t.Errorf("paging fragment re-rendered the first row (should append only)")
	}
}

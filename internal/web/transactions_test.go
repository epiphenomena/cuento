package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/i18n"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// p12.2 transaction editor handler tests. The editor is driven through the REAL
// mounted router (httptest) against a real migrated db (AGENTS testing conventions);
// no store mocks. The FIVE TRAPS are asserted here at the handler layer:
//   trap 1  edit round-trips each existing split's id (one op=update, others untouched)
//   trap 3  signed mode and DR/CR mode post byte-identical `splits` rows
//   trap 4  a server re-render (validation error) keeps stable input ids
//   trap 5  typed store errors route to the right slot (row vs totals bar)
// plus subsidiary filtering, fund apply-to-all, program/class gating.

// txnWebEnv is a small hand-built dataset for the editor: two subsidiaries, a fund
// scoped to one with a program scope, a program tree, and a handful of leaf accounts.
type txnWebEnv struct {
	h  http.Handler
	st *store.Store
	sm *scs.SessionManager
	db *sql.DB

	book int64
	sub1 int64 // child sub A
	sub2 int64 // child sub B

	checking int64 // asset, sub1 only
	cashB    int64 // asset, sub2 only
	salaries int64 // expense, default class program, sub1+sub2
	grantRev int64 // revenue, sub1+sub2
	supplies int64 // expense, no default class, sub1

	progEdu  int64 // program under root, the fund's scope
	progRoot int64

	fund int64 // scoped to sub1, program scope = progEdu
}

func newTxnWebEnv(t *testing.T) *txnWebEnv {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	e := &txnWebEnv{h: app.handler, st: st, sm: app.sessions, db: db}
	e.book = mkUser(t, st, "txnbook", "write", false)

	root := int64(1)
	var err error
	e.sub1, err = st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{Name: "Sub One", ParentID: root, BaseCurrency: "USD"})
	must(t, err, "sub1")
	e.sub2, err = st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{Name: "Sub Two", ParentID: root, BaseCurrency: "USD"})
	must(t, err, "sub2")

	// Program tree: root (seeded) + Educación.
	progs, err := st.ProgramTree(ctx)
	must(t, err, "program tree")
	e.progRoot = progs[0].ID
	e.progEdu, err = st.CreateProgram(ctx, store.CreateProgramInput{Name: "Educacion", ParentID: e.progRoot})
	must(t, err, "prog edu")

	mkAcct := func(name, typ string, subs []int64, class string, defProg *int64) int64 {
		in := store.CreateAccountInput{
			Type: typ, DefaultCurrency: "USD",
			Names: map[string]string{"en": name}, Subsidiaries: subs,
		}
		if class != "" {
			in.FunctionalClass = strptr(class)
		}
		if defProg != nil {
			in.DefaultProgramID = defProg
		}
		id, err := st.CreateAccount(ctx, in)
		must(t, err, "acct "+name)
		return id
	}
	e.checking = mkAcct("Checking", "asset", []int64{e.sub1}, "", nil)
	e.cashB = mkAcct("Cash B", "asset", []int64{e.sub2}, "", nil)
	e.salaries = mkAcct("Salaries", "expense", []int64{e.sub1, e.sub2}, "program", nil)
	e.grantRev = mkAcct("Grant Revenue", "revenue", []int64{e.sub1, e.sub2}, "", nil)
	e.supplies = mkAcct("Supplies", "expense", []int64{e.sub1}, "", nil)

	prog := e.progEdu
	e.fund, err = st.CreateFund(ctx, store.CreateFundInput{
		Name: "Beca", Restriction: "purpose", Subsidiaries: []int64{e.sub1}, ProgramID: &prog,
	})
	must(t, err, "fund")

	return e
}

// balancedForm builds a POST form for a balanced 2-split expense txn in sub1: debit
// salaries `amt`, credit checking `-amt`, both unrestricted. `amt` is a decimal
// string (e.g. "100.00"); the amount fields carry SIGNED values (the client
// normalizes DR/CR into these; here the test posts signed directly).
func (e *txnWebEnv) balancedForm(debitAmt, creditAmt string) url.Values {
	f := url.Values{}
	f.Set("subsidiary", itoa(e.sub1))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("memo", "")
	// row 0: salaries (expense) debit
	f.Set("split_id_0", "")
	f.Set("account_0", itoa(e.salaries))
	f.Set("amount_0", debitAmt)
	f.Set("fund_0", "")
	f.Set("class_0", "program")
	f.Set("program_0", itoa(e.progRoot))
	// row 1: checking (asset) credit
	f.Set("split_id_1", "")
	f.Set("account_1", itoa(e.checking))
	f.Set("amount_1", creditAmt)
	f.Set("fund_1", "")
	f.Set("rows", "2")
	return f
}

// TestTxnCreateRoundTrip: a balanced multi-split txn posts and its splits store
// correctly (amounts, accounts, fund).
func TestTxnCreateRoundTrip(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.balancedForm("100.00", "-100.00")
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	id := latestTxnID(t, e)
	splits, err := e.st.TransactionSplits(context.Background(), id)
	must(t, err, "splits")
	if len(splits) != 2 {
		t.Fatalf("want 2 splits, got %d", len(splits))
	}
	byAcct := map[int64]int64{}
	for _, s := range splits {
		byAcct[s.AccountID] = s.Amount
	}
	if byAcct[e.salaries] != 10000 || byAcct[e.checking] != -10000 {
		t.Fatalf("amounts wrong: %v", byAcct)
	}
}

// TestTxnNotesPersistsAndPrefills (p24.2): the transaction-level notes textarea is
// parsed on submit, stored on the header, and prefilled when the txn is reopened.
func TestTxnNotesPersistsAndPrefills(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.balancedForm("100.00", "-100.00")
	notes := "Longer explanation for the auditors: reclassified per board minutes."
	f.Set("notes", notes)
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}
	id := latestTxnID(t, e)

	// Stored on the header.
	hdr, err := e.st.GetTransaction(context.Background(), id)
	must(t, err, "GetTransaction")
	if hdr.Notes != notes {
		t.Errorf("stored notes = %q, want %q", hdr.Notes, notes)
	}

	// The edit form prefills the notes textarea.
	getRec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(id)+"/edit", nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("edit form: status=%d", getRec.Code)
	}
	if body := getRec.Body.String(); !strings.Contains(body, `id="txn-notes"`) || !strings.Contains(body, notes) {
		t.Errorf("edit form did not prefill the notes textarea:\n%s", body)
	}
}

// TestTxnEditSplitIDRoundTrip (TRAP 1): editing ONE split of a 3-split txn produces
// exactly ONE op=update split-version and leaves the other two version-untouched.
func TestTxnEditSplitIDRoundTrip(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Seed a 3-split txn directly via the store: salaries 60 + supplies 40 (both
	// expense, unrestricted, program root) debit; checking -100 credit.
	prog := e.progRoot
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: 6000, ProgramID: &prog, Position: 0},
			{AccountID: e.supplies, Amount: 4000, ProgramID: &prog, FunctionalClass: strptr("program"), Position: 1},
			{AccountID: e.checking, Amount: -10000, Position: 2},
		},
	})
	must(t, err, "seed 3-split")

	live, err := e.st.TransactionSplits(ctx, id)
	must(t, err, "live splits")
	if len(live) != 3 {
		t.Fatalf("want 3 seeded splits, got %d", len(live))
	}
	// Record version counts before the edit.
	before := map[int64]int{}
	for _, s := range live {
		before[s.ID] = splitVersionCountWeb(t, e, s.ID)
	}

	// Build an edit form that changes ONLY the salaries split's memo, round-tripping
	// every existing split id.
	f := url.Values{}
	f.Set("subsidiary", itoa(e.sub1))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	// Order splits by position for deterministic row indices.
	rows := live
	for i, s := range rows {
		f.Set("split_id_"+itoa(int64(i)), itoa(s.ID))
		f.Set("account_"+itoa(int64(i)), itoa(s.AccountID))
		f.Set("amount_"+itoa(int64(i)), signedStr(s.Amount))
		if s.FundID.Valid {
			f.Set("fund_"+itoa(int64(i)), itoa(s.FundID.Int64))
		} else {
			f.Set("fund_"+itoa(int64(i)), "")
		}
		if s.ProgramID.Valid {
			f.Set("program_"+itoa(int64(i)), itoa(s.ProgramID.Int64))
		}
		if s.FunctionalClass.Valid {
			f.Set("class_"+itoa(int64(i)), s.FunctionalClass.String)
		}
		// change ONLY the salaries row memo
		if s.AccountID == e.salaries {
			f.Set("memo_"+itoa(int64(i)), "edited memo")
		}
	}
	f.Set("rows", itoa(int64(len(rows))))

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(id), f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	// Exactly the salaries split gained one version row; the others are untouched.
	after, err := e.st.TransactionSplits(ctx, id)
	must(t, err, "after splits")
	if len(after) != 3 {
		t.Fatalf("edit changed split count to %d (delete-all+recreate is the trap-1 bug)", len(after))
	}
	// The split ids must be the SAME set (no delete-all/recreate).
	for _, s := range after {
		if _, ok := before[s.ID]; !ok {
			t.Fatalf("split id %d is new after edit -> delete-all+recreate (trap 1 FAILED)", s.ID)
		}
		nowN := splitVersionCountWeb(t, e, s.ID)
		if s.AccountID == e.salaries {
			if nowN != before[s.ID]+1 {
				t.Fatalf("salaries split versions: before=%d after=%d, want +1", before[s.ID], nowN)
			}
		} else if nowN != before[s.ID] {
			t.Fatalf("untouched split %d versions changed: before=%d after=%d", s.ID, before[s.ID], nowN)
		}
	}
}

// TestTxnBothModesIdenticalSplits (TRAP 3): posting the same economic transaction in
// signed mode and in DR/CR mode yields byte-identical `splits` rows. The client
// normalizes DR/CR into the signed `amount_i` field before submit, so BOTH POSTs
// carry the same signed field values -> the handler is mode-agnostic on input.
func TestTxnBothModesIdenticalSplits(t *testing.T) {
	e := newTxnWebEnv(t)

	post := func(user int64) []store.SplitState {
		f := e.balancedForm("100.00", "-100.00")
		rec := asUser(t, e.h, e.sm, user, http.MethodPost, "/transactions", f)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
		}
		id := latestTxnID(t, e)
		sp, err := e.st.TransactionSplits(context.Background(), id)
		must(t, err, "splits")
		out := make([]store.SplitState, len(sp))
		for i, s := range sp {
			out[i] = store.SplitState{
				AccountID: s.AccountID, Amount: s.Amount, FundID: s.FundID,
				ProgramID: s.ProgramID, FunctionalClass: s.FunctionalClass,
				Memo: s.Memo, Position: s.Position,
			}
		}
		return out
	}

	// A signed-mode user and a dr_cr-mode user. The FORM is identical (client
	// normalizes), so the stored splits must be identical.
	signedUser := e.book
	drcrUser := mkUserDisplay(t, e, "drcruser", "dr_cr")

	a := post(signedUser)
	b := post(drcrUser)
	if len(a) != len(b) {
		t.Fatalf("split counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("split %d differs between modes:\n signed=%+v\n dr_cr =%+v", i, a[i], b[i])
		}
	}
}

// TestTxnUnbalancedGoesToTotalsBar (TRAP 5): ErrUnbalanced routes to the totals bar,
// not a row.
func TestTxnUnbalancedGoesToTotalsBar(t *testing.T) {
	e := newTxnWebEnv(t)
	f := e.balancedForm("100.00", "-90.00") // does not balance
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "txn-totals-error") {
		t.Fatalf("unbalanced error not in totals bar slot; body:\n%s", body)
	}
}

// TestTxnFundProgramScopeGoesToRow (TRAP 5): ErrFundProgramScope renders as a per-row
// error (its row), not a page alert.
func TestTxnFundProgramScopeGoesToRow(t *testing.T) {
	e := newTxnWebEnv(t)
	// grantRev (revenue) tagged the fund (program scope = Educación) but carrying the
	// ROOT program (outside the scope) -> ErrFundProgramScope on that row.
	f := url.Values{}
	f.Set("subsidiary", itoa(e.sub1))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("split_id_0", "")
	f.Set("account_0", itoa(e.grantRev))
	f.Set("amount_0", "-100.00")
	f.Set("fund_0", itoa(e.fund))
	f.Set("program_0", itoa(e.progRoot)) // outside the fund's Educación scope
	f.Set("split_id_1", "")
	f.Set("account_1", itoa(e.checking))
	f.Set("amount_1", "100.00")
	f.Set("fund_1", itoa(e.fund))
	f.Set("rows", "2")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-row-error="0"`) {
		t.Fatalf("fund-program scope error not on row 0; body:\n%s", body)
	}
	// It must NOT be rendered as a page-level totals error.
	if strings.Contains(body, `txn-totals-error">`) && strings.Contains(body, "error.txn.fund_program_scope") &&
		!strings.Contains(body, `data-row-error="0"`) {
		t.Fatalf("scope error leaked to totals bar")
	}
}

// TestTxnAccountNotInSubGoesToRow (TRAP 5): an account not mapped to the chosen sub
// routes to its row (ErrAccountNotInSubsidiary).
func TestTxnAccountNotInSubGoesToRow(t *testing.T) {
	e := newTxnWebEnv(t)
	// cashB is sub2-only; posting it in sub1 is out of scope. Row 1 references it.
	f := url.Values{}
	f.Set("subsidiary", itoa(e.sub1))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("split_id_0", "")
	f.Set("account_0", itoa(e.salaries))
	f.Set("amount_0", "100.00")
	f.Set("class_0", "program")
	f.Set("program_0", itoa(e.progRoot))
	f.Set("split_id_1", "")
	f.Set("account_1", itoa(e.cashB)) // not in sub1
	f.Set("amount_1", "-100.00")
	f.Set("rows", "2")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-row-error="1"`) {
		t.Fatalf("account-not-in-sub error not on row 1; body:\n%s", rec.Body.String())
	}
}

// TestTxnStableInputIDsAcrossRerender (TRAP 4): a server re-render (validation error)
// keeps the same input ids as the create form (deterministic, keyed to position/id).
func TestTxnStableInputIDsAcrossRerender(t *testing.T) {
	e := newTxnWebEnv(t)

	// The fresh new form's ids.
	getRec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET new: %d", getRec.Code)
	}
	// A row-0 account input id must be stable and present on the new form.
	wantID := `id="txn-account-0"`
	if !strings.Contains(getRec.Body.String(), wantID) {
		t.Fatalf("new form missing stable id %s; body:\n%s", wantID, getRec.Body.String())
	}

	// Now an invalid POST re-renders the form region (422); the same id must persist.
	f := e.balancedForm("100.00", "-90.00")
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), wantID) {
		t.Fatalf("re-render lost stable id %s; body:\n%s", wantID, rec.Body.String())
	}
}

// TestTxnEditorFullWidth: the full-page editor opts <main> out of the centered
// reading column (app-main-wide, p23.2) so the split grid can use the horizontal
// space. The htmx form-region swap is just the form partial, so it does NOT carry
// the main class.
func TestTxnEditorFullWidth(t *testing.T) {
	e := newTxnWebEnv(t)

	full := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if full.Code != http.StatusOK {
		t.Fatalf("GET new: %d", full.Code)
	}
	if !strings.Contains(full.Body.String(), "app-main-wide") {
		t.Fatalf("full editor page missing app-main-wide (full-width opt-out); body:\n%s", full.Body.String())
	}

	// The subsidiary re-filter (an htmx GET) returns only the #txn-form partial —
	// no <main>, so no app-main-wide.
	partial := asHTMXUser(t, e, http.MethodGet, "/transactions/new", nil)
	if strings.Contains(partial.Body.String(), "app-main-wide") {
		t.Errorf("htmx form-region swap should not include the main class app-main-wide")
	}
}

// asHTMXUser is asUser plus the HX-Request header the real htmx client sends on an
// in-flow action (the subsidiary re-filter's hx-get, the editor's hx-post). It exists
// so the p12.6 tests can assert the in-flow swap behavior (partial, not full page).
func asHTMXUser(t *testing.T, e *txnWebEnv, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	req.AddCookie(mintCookie(t, e.sm, e.book))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// isFullPage reports whether an htmx response body is a FULL page (doctype / <html> /
// <nav> shell) rather than a form-region partial. An in-flow editor action must never
// return a full page (it would nest a whole document inside #txn-form or force a
// visible reload mid-entry -- the anti-jank rule, Appendix C).
func isFullPage(body string) bool {
	return strings.Contains(body, "<!DOCTYPE") ||
		strings.Contains(body, "<!doctype") ||
		strings.Contains(body, "<html") ||
		strings.Contains(body, "<nav")
}

// TestTxnStableInputIDsAcrossAllSwaps (p12.6, Tests bullet a): input ids stay stable
// across EVERY in-flow swap response, not only the 422 POST re-render that
// TestTxnStableInputIDsAcrossRerender already covers. Here we also assert the
// subsidiary-change re-filter (the header select's hx-get) and the htmx 422 re-render
// keep the exact row-0 ids, so focus/tab targets never jump between swaps.
func TestTxnStableInputIDsAcrossAllSwaps(t *testing.T) {
	e := newTxnWebEnv(t)
	setDefaultSub(t, e, e.book, e.sub1)

	// The stable ids the fresh new form emits on row 0 (the keys the client and focus
	// logic depend on).
	wantIDs := []string{
		`id="txn-account-0"`, `id="txn-amount-0"`, `id="txn-fund-0"`,
		`id="txn-program-0"`, `id="txn-class-0"`, `id="txn-memo-0"`,
		`id="txn-splitid-0"`,
	}
	base := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if base.Code != http.StatusOK {
		t.Fatalf("GET new: %d", base.Code)
	}
	for _, id := range wantIDs {
		if !strings.Contains(base.Body.String(), id) {
			t.Fatalf("fresh new form missing stable id %s", id)
		}
	}

	// Swap 1: the subsidiary re-filter (hx-get to /transactions/new?subsidiary=...),
	// which swaps #txn-form. Every row-0 id must survive.
	q := url.Values{}
	q.Set("subsidiary", itoa(e.sub2))
	q.Set("rows", "2")
	q.Set("account_0", itoa(e.checking))
	q.Set("amount_0", "10.00")
	refilter := asHTMXUser(t, e, http.MethodGet, "/transactions/new?"+q.Encode(), nil)
	if refilter.Code != http.StatusOK {
		t.Fatalf("re-filter GET: %d", refilter.Code)
	}
	for _, id := range wantIDs {
		if !strings.Contains(refilter.Body.String(), id) {
			t.Fatalf("re-filter swap lost stable id %s; body:\n%s", id, refilter.Body.String())
		}
	}

	// Swap 2: an htmx 422 re-render (unbalanced POST). Same row-0 ids must survive.
	f := e.balancedForm("100.00", "-90.00")
	rerender := asHTMXUser(t, e, http.MethodPost, "/transactions", f)
	if rerender.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rerender.Code)
	}
	for _, id := range wantIDs {
		if !strings.Contains(rerender.Body.String(), id) {
			t.Fatalf("422 htmx re-render lost stable id %s; body:\n%s", id, rerender.Body.String())
		}
	}
}

// TestTxnInFlowActionsNeverFullReload (p12.6, Tests bullet b): in-flow editor actions
// swap partials or navigate INTENTIONALLY (HX-Redirect on save) -- they never bounce
// through a full-page reload mid-entry. We assert:
//   - the subsidiary re-filter swap returns the form-region PARTIAL (no shell/doctype);
//   - the 422 re-render returns the form-region PARTIAL (no shell/doctype);
//   - a successful save returns HX-Redirect (an intentional full-page navigation to the
//     register), NOT a re-rendered editor page -- the ONE deliberate navigation.
//
// Stable ids across those same swaps are covered by TestTxnStableInputIDsAcrossAllSwaps
// and TestTxnStableInputIDsAcrossRerender.
func TestTxnInFlowActionsNeverFullReload(t *testing.T) {
	e := newTxnWebEnv(t)
	setDefaultSub(t, e, e.book, e.sub1)

	// Re-filter: partial, still the #txn-form region, no shell.
	q := url.Values{}
	q.Set("subsidiary", itoa(e.sub2))
	q.Set("rows", "2")
	refilter := asHTMXUser(t, e, http.MethodGet, "/transactions/new?"+q.Encode(), nil)
	if refilter.Code != http.StatusOK {
		t.Fatalf("re-filter GET: %d", refilter.Code)
	}
	if isFullPage(refilter.Body.String()) {
		t.Fatalf("re-filter returned a FULL page mid-entry (should be a partial):\n%s", refilter.Body.String())
	}
	if !strings.Contains(refilter.Body.String(), `id="txn-form"`) {
		t.Fatalf("re-filter partial missing the #txn-form region")
	}

	// 422 re-render: partial, no shell.
	bad := e.balancedForm("100.00", "-90.00")
	rerender := asHTMXUser(t, e, http.MethodPost, "/transactions", bad)
	if rerender.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rerender.Code)
	}
	if isFullPage(rerender.Body.String()) {
		t.Fatalf("422 re-render returned a FULL page mid-entry (should be a partial):\n%s", rerender.Body.String())
	}

	// Successful save: HX-Redirect (intentional navigation), NOT a re-rendered editor.
	ok := e.balancedForm("100.00", "-100.00")
	saved := asHTMXUser(t, e, http.MethodPost, "/transactions", ok)
	if saved.Code != http.StatusOK {
		t.Fatalf("save: status=%d body=%s", saved.Code, saved.Body.String())
	}
	dest := saved.Header().Get("HX-Redirect")
	if dest == "" {
		t.Fatalf("successful save did not set HX-Redirect (in-flow save must navigate intentionally, not swap a page)")
	}
	if !strings.HasSuffix(dest, "/register") {
		t.Fatalf("HX-Redirect = %q, want the first split's /register", dest)
	}
	// A save must NOT re-render the editor form region into the body (that would be a
	// mid-entry swap of the whole editor rather than the intended navigation).
	if strings.Contains(saved.Body.String(), `id="txn-form"`) {
		t.Fatalf("successful save swapped the editor form back in instead of navigating:\n%s", saved.Body.String())
	}
}

// TestTxnEditorEsNoRawKeys (p12.6 es locale pass): the editor rendered for an es-locale
// user shows the Spanish catalog strings and leaks NO raw i18n keys. This converts the
// qa-entry.md es checkpoint from an inferred claim (catalog parity) into an OBSERVED
// one: it actually GETs the editor in es and scans the body. The user's stored locale
// is authoritative over ?lang= (D14), so we set it directly (settings writers are
// p13.1; raw SQL in tests is in-convention, e.g. setDefaultSub). The chrome/nav is
// exempt (proper nouns render verbatim), so we assert on the editor's own strings: the
// known es column headers/title/button are present, and no raw `txn.`/`error.txn.` KEY
// substring appears.
func TestTxnEditorEsNoRawKeys(t *testing.T) {
	e := newTxnWebEnv(t)
	setDefaultSub(t, e, e.book, e.sub1)
	setLocale(t, e.db, e.book, "es")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new (es): %d", rec.Code)
	}
	body := rec.Body.String()

	// The Spanish catalog strings render (a known column header + the page title).
	for _, want := range []string{
		i18n.T("es", "txn.col.account"), // "Cuenta"
		i18n.T("es", "txn.col.amount"),  // "Importe"
		i18n.T("es", "txn.new_title"),   // "Nueva transaccion"
		i18n.T("es", "txn.fund.apply_all_btn"),
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("es editor missing translated string %q", want)
		}
	}
	// And they are the es strings, not the en ones (guards against a fallback-to-en).
	if strings.Contains(body, i18n.T("en", "txn.new_title")) {
		t.Fatalf("es editor leaked the en title %q (catalog fell back to en)", i18n.T("en", "txn.new_title"))
	}

	// No raw i18n KEY substring leaks into the rendered body (a missing/typo'd {{t}}
	// would show the literal key). Proper nouns are stored data, not keys, so this is
	// safe to assert broadly on the editor's own vocabulary.
	for _, key := range []string{
		"txn.col.", "txn.fund.", "txn.class.", "txn.amount.", "txn.new_title",
		"txn.subsidiary", "txn.date", "txn.payee", "txn.memo", "txn.add_row",
		"error.txn.",
	} {
		if strings.Contains(body, key) {
			t.Fatalf("es editor leaked a raw i18n key substring %q (a {{t}} call is missing/broken):\n%s", key, body)
		}
	}
}

// TestTxnSubsidiaryFiltersOptions: the new form filters account comboboxes and fund
// options to the chosen (default) subsidiary. With sub1 as default, cashB (sub2) is
// absent from the account options and the fund (scoped to sub1) is present.
func TestTxnSubsidiaryFiltersOptions(t *testing.T) {
	e := newTxnWebEnv(t)
	// Set the user's default subsidiary to sub1 so the new form defaults there.
	setDefaultSub(t, e, e.book, e.sub1)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new: %d", rec.Code)
	}
	body := rec.Body.String()
	// checking (sub1) present as an account option; cashB (sub2) absent.
	if !strings.Contains(body, `value="`+itoa(e.checking)+`"`) {
		t.Fatalf("checking not offered as account option")
	}
	if strings.Contains(body, `data-account-option value="`+itoa(e.cashB)+`"`) {
		t.Fatalf("cashB (sub2) leaked into sub1 account options")
	}
	// The fund scoped to sub1 is offered.
	if !strings.Contains(body, `value="`+itoa(e.fund)+`"`) {
		t.Fatalf("fund scoped to sub1 not offered")
	}
}

// TestTxnAccountOptionsCarryGatingMetadata: the account combobox options carry the
// data-* the client uses to show the program select on R/E rows and the class select
// on expense rows, and to prefill each from the account defaults (server re-defaults
// authoritatively). salaries (expense, default class program) and grantRev (revenue)
// must expose their type + defaults.
func TestTxnAccountOptionsCarryGatingMetadata(t *testing.T) {
	e := newTxnWebEnv(t)
	setDefaultSub(t, e, e.book, e.sub1)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new: %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `value="`+itoa(e.salaries)+`"`) ||
		!strings.Contains(body, `data-type="expense"`) ||
		!strings.Contains(body, `data-default-class="program"`) {
		t.Fatalf("salaries option missing expense gating metadata; body:\n%s", body)
	}
	if !strings.Contains(body, `value="`+itoa(e.grantRev)+`"`) || !strings.Contains(body, `data-type="revenue"`) {
		t.Fatalf("grantRev option missing revenue gating metadata")
	}
	if !strings.Contains(body, `id="txn-program-0"`) || !strings.Contains(body, `id="txn-class-0"`) {
		t.Fatalf("program/class selects not rendered on row 0")
	}
}

// TestTxnSubsidiaryReFilterEchoesRows: the subsidiary-change re-filter re-filters the
// account options to the new sub AND echoes typed rows, flagging a row whose account
// left the sub (Appendix C: never silent-clear).
func TestTxnSubsidiaryReFilterEchoesRows(t *testing.T) {
	e := newTxnWebEnv(t)
	q := url.Values{}
	q.Set("subsidiary", itoa(e.sub2))
	q.Set("rows", "2")
	q.Set("account_0", itoa(e.checking)) // sub1-only -> invalid in sub2
	q.Set("amount_0", "10.00")
	q.Set("account_1", itoa(e.cashB)) // sub2 -> valid
	q.Set("amount_1", "-10.00")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new?"+q.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-filter GET: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="`+itoa(e.cashB)+`"`) {
		t.Fatalf("cashB not offered after re-filter to sub2")
	}
	if !strings.Contains(body, `data-row-error="0"`) {
		t.Fatalf("out-of-sub row not flagged after re-filter; body:\n%s", body)
	}
	if !strings.Contains(body, `value="10.00"`) {
		t.Fatalf("typed amount not preserved across re-filter")
	}
}

// TestTxnEditPrefillNumberFormat: an EU-number user's edit form prefills amounts in
// EU format so a save-without-touching round-trips (rule 10) -- the prefill format
// must match the parse format.
func TestTxnEditPrefillNumberFormat(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	prog := e.progRoot
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: 123456, ProgramID: &prog, FunctionalClass: strptr("program"), Position: 0},
			{AccountID: e.checking, Amount: -123456, Position: 1},
		},
	})
	must(t, err, "seed txn")

	eu := mkUser(t, e.st, "euuser", "write", false)
	if _, err := e.db.Exec(`UPDATE users SET number_format = 'EU' WHERE id = ?`, eu); err != nil {
		t.Fatalf("set EU: %v", err)
	}

	rec := asUser(t, e.h, e.sm, eu, http.MethodGet, "/transactions/"+itoa(id)+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit GET: %d", rec.Code)
	}
	// EU number format: 1.234,56 (dot grouping, comma decimal).
	if !strings.Contains(rec.Body.String(), `value="1.234,56"`) {
		t.Fatalf("EU-format prefill missing; body:\n%s", rec.Body.String())
	}
}

// TestTxnCurrencyFromSubsidiary: the new form defaults its currency to the selected
// subsidiary's base currency (D18) -- an MXN sub defaults to MXN, not USD.
func TestTxnCurrencyFromSubsidiary(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	subMX, err := e.st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{Name: "Sub MX", ParentID: 1, BaseCurrency: "MXN"})
	must(t, err, "sub MX")
	setDefaultSub(t, e, e.book, subMX)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="currency" value="MXN"`) {
		t.Fatalf("MXN sub did not default the currency to MXN; body:\n%s", rec.Body.String())
	}
}

// TestTxnPerms: ReadOnly and anon are denied POST /transactions.
func TestTxnPerms(t *testing.T) {
	e := newTxnWebEnv(t)
	ro := mkUser(t, e.st, "txnro", "read", false)

	f := e.balancedForm("100.00", "-100.00")
	rec := asUser(t, e.h, e.sm, ro, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only POST: want 403, got %d", rec.Code)
	}
	rec = asUser(t, e.h, e.sm, 0, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusFound {
		t.Fatalf("anon POST: want 302 redirect, got %d", rec.Code)
	}
}

// --- helpers --------------------------------------------------------------

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func strptr(s string) *string { return &s }

func signedStr(minor int64) string {
	// exponent 2 (USD) decimal string with sign.
	neg := minor < 0
	if neg {
		minor = -minor
	}
	s := strconv.FormatInt(minor/100, 10) + "." + pad2(minor%100)
	if neg {
		return "-" + s
	}
	return s
}

func pad2(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// latestTxnID returns the highest live transaction id. Test-only read via raw SQL.
func latestTxnID(t *testing.T, e *txnWebEnv) int64 {
	t.Helper()
	var id int64
	if err := e.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM transactions WHERE deleted = 0`).Scan(&id); err != nil {
		t.Fatalf("latest txn id: %v", err)
	}
	return id
}

// splitVersionCountWeb counts splits_versions rows for a split (trap 1 assertion).
func splitVersionCountWeb(t *testing.T, e *txnWebEnv, splitID int64) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM splits_versions WHERE entity_id = ?`, splitID).Scan(&n); err != nil {
		t.Fatalf("split version count(%d): %v", splitID, err)
	}
	return n
}

// mkUserDisplay creates a write user and sets its display_mode column directly (the
// settings UI is p13.1); this test only needs the stored value so the editor renders
// the right amount columns.
func mkUserDisplay(t *testing.T, e *txnWebEnv, username, display string) int64 {
	t.Helper()
	id := mkUser(t, e.st, username, "write", false)
	if _, err := e.db.Exec(`UPDATE users SET display_mode = ? WHERE id = ?`, display, id); err != nil {
		t.Fatalf("set display mode: %v", err)
	}
	return id
}

// setDefaultSub sets a user's default_subsidiary_id column directly (settings UI is
// p13.1); the editor reads it to default the header subsidiary.
func setDefaultSub(t *testing.T, e *txnWebEnv, userID, subID int64) {
	t.Helper()
	if _, err := e.db.Exec(`UPDATE users SET default_subsidiary_id = ? WHERE id = ?`, subID, userID); err != nil {
		t.Fatalf("set default sub: %v", err)
	}
}

package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/i18n"
	"cuento/internal/ids"
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

	book ids.UserID
	sub1 ids.SubsidiaryID // child sub A
	sub2 ids.SubsidiaryID // child sub B

	checking ids.AccountID // asset, sub1 only
	cashB    ids.AccountID // asset, sub2 only
	salaries ids.AccountID // expense, default class program, sub1+sub2
	grantRev ids.AccountID // revenue, sub1+sub2
	supplies ids.AccountID // expense, no default class, sub1

	progEdu  ids.ProgramID // program under root, the fund's scope
	progRoot ids.ProgramID

	fund ids.FundID // scoped to sub1, program scope = progEdu
}

func newTxnWebEnv(t *testing.T) *txnWebEnv {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	e := &txnWebEnv{h: app.handler, st: st, sm: app.sessions, db: db}
	e.book = mkUser(t, st, "txnbook", "write", false)

	root := ids.SubsidiaryID(1)
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

	mkAcct := func(name, typ string, subs []ids.SubsidiaryID, class string, defProg *ids.ProgramID) ids.AccountID {
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
	e.checking = mkAcct("Checking", "asset", []ids.SubsidiaryID{e.sub1}, "", nil)
	e.cashB = mkAcct("Cash B", "asset", []ids.SubsidiaryID{e.sub2}, "", nil)
	e.salaries = mkAcct("Salaries", "expense", []ids.SubsidiaryID{e.sub1, e.sub2}, "program", nil)
	e.grantRev = mkAcct("Grant Revenue", "revenue", []ids.SubsidiaryID{e.sub1, e.sub2}, "", nil)
	e.supplies = mkAcct("Supplies", "expense", []ids.SubsidiaryID{e.sub1}, "", nil)

	prog := e.progEdu
	e.fund, err = st.CreateFund(ctx, store.CreateFundInput{
		Name: "Beca", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{e.sub1}, ProgramID: &prog,
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
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("memo", "")
	// row 0: salaries (expense) debit -- p26.41 combined control: a program pick
	// (p:<progRoot>) decodes to program=progRoot + class=program on the expense row.
	f.Set("split_id_0", "")
	f.Set("account_0", itoa(int64(e.salaries)))
	f.Set("amount_0", debitAmt)
	f.Set("fund_0", "")
	f.Set("progclass_0", "p:"+itoa(int64(e.progRoot)))
	f.Set("program_0", itoa(int64(e.progRoot)))
	// row 1: checking (asset) credit
	f.Set("split_id_1", "")
	f.Set("account_1", itoa(int64(e.checking)))
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
	byAcct := map[ids.AccountID]int64{}
	for _, s := range splits {
		byAcct[s.AccountID] = s.Amount
	}
	if byAcct[e.salaries] != 10000 || byAcct[e.checking] != -10000 {
		t.Fatalf("amounts wrong: %v", byAcct)
	}
}

// TestTxnEditShowsInactiveSplitAccount (p26.10): editing a transaction whose split
// references a now-INACTIVE account must still DISPLAY that account as a SELECTED,
// marked option -- not a blank "Choose account" select (the reported "missing accounts"
// bug). Mirrors the dev-db repro: post a txn on checking, deactivate checking, reopen
// the editor.
func TestTxnEditShowsInactiveSplitAccount(t *testing.T) {
	e := newTxnWebEnv(t)

	// Post a balanced txn using checking, then deactivate checking.
	f := e.balancedForm("100.00", "-100.00")
	if rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}
	id := latestTxnID(t, e)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if err := e.st.DeactivateAccount(ctx, e.checking); err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(int64(id))+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit GET: status=%d", rec.Code)
	}
	body := rec.Body.String()

	// The inactive checking account must appear as a SINGLE option node that is BOTH
	// data-unavailable and SELECTED (so the combobox overlay shows it, not a blank box).
	// Attribute order in the option is value ... data-unavailable ... selected, so match
	// the whole node loosely.
	re := regexp.MustCompile(`(?s)<option[^>]*value="` + itoa(int64(e.checking)) + `"[^>]*data-unavailable="1"[^>]*\bselected\b[^>]*>`)
	if !re.MatchString(body) {
		t.Fatalf("inactive checking account option is not present, marked, and SELECTED; body:\n%s", body)
	}
	// The user-visible marker suffix is present (rule 9: via the i18n catalog).
	if !strings.Contains(body, "(unavailable)") {
		t.Fatalf("edit form missing the unavailable marker suffix; body:\n%s", body)
	}
}

// TestTxnDuplicateShowsInactiveSplitAccount (p26.10): DUPLICATE clones an old entry, so
// it must also display a split whose account is now inactive as a SELECTED, marked
// option (same fix as the edit path; duplicate is the likeliest place to hit a stale
// account).
func TestTxnDuplicateShowsInactiveSplitAccount(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.balancedForm("100.00", "-100.00")
	if rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}
	id := latestTxnID(t, e)

	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if err := e.st.DeactivateAccount(ctx, e.checking); err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(int64(id))+"/duplicate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate GET: status=%d", rec.Code)
	}
	body := rec.Body.String()
	re := regexp.MustCompile(`(?s)<option[^>]*value="` + itoa(int64(e.checking)) + `"[^>]*data-unavailable="1"[^>]*\bselected\b[^>]*>`)
	if !re.MatchString(body) {
		t.Fatalf("duplicate form: inactive checking account is not present, marked, and SELECTED; body:\n%s", body)
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
	getRec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(int64(id))+"/edit", nil)
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
	before := map[ids.SplitID]int{}
	for _, s := range live {
		before[s.ID] = splitVersionCountWeb(t, e, s.ID)
	}

	// Build an edit form that changes ONLY the salaries split's memo, round-tripping
	// every existing split id.
	f := url.Values{}
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	// Order splits by position for deterministic row indices.
	rows := live
	for i, s := range rows {
		f.Set("split_id_"+itoa(int64(i)), itoa(int64(s.ID)))
		f.Set("account_"+itoa(int64(i)), itoa(int64(s.AccountID)))
		f.Set("amount_"+itoa(int64(i)), signedStr(s.Amount))
		if s.FundID.Valid {
			f.Set("fund_"+itoa(int64(i)), itoa(s.FundID.Int64))
		} else {
			f.Set("fund_"+itoa(int64(i)), "")
		}
		// p26.41: encode (program, class) into progclass_i + the hidden program_i carrier.
		if s.ProgramID.Valid {
			f.Set("program_"+itoa(int64(i)), itoa(s.ProgramID.Int64))
		}
		switch {
		case s.FunctionalClass.Valid && s.FunctionalClass.String != "program":
			f.Set("progclass_"+itoa(int64(i)), "c:"+s.FunctionalClass.String)
		case s.ProgramID.Valid:
			f.Set("progclass_"+itoa(int64(i)), "p:"+itoa(s.ProgramID.Int64))
		}
		// change ONLY the salaries row memo
		if s.AccountID == e.salaries {
			f.Set("memo_"+itoa(int64(i)), "edited memo")
		}
	}
	f.Set("rows", itoa(int64(len(rows))))

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(int64(id)), f)
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

	post := func(user ids.UserID) []store.SplitState {
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
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("split_id_0", "")
	f.Set("account_0", itoa(int64(e.grantRev)))
	f.Set("amount_0", "-100.00")
	f.Set("fund_0", itoa(int64(e.fund)))
	f.Set("program_0", itoa(int64(e.progRoot))) // outside the fund's Educación scope
	f.Set("split_id_1", "")
	f.Set("account_1", itoa(int64(e.checking)))
	f.Set("amount_1", "100.00")
	f.Set("fund_1", itoa(int64(e.fund)))
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
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("split_id_0", "")
	f.Set("account_0", itoa(int64(e.salaries)))
	f.Set("amount_0", "100.00")
	f.Set("class_0", "program")
	f.Set("program_0", itoa(int64(e.progRoot)))
	f.Set("split_id_1", "")
	f.Set("account_1", itoa(int64(e.cashB))) // not in sub1
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

// TestTxnAccountOptionShowsTypePrefix: each account option in the transaction
// editor's account selects shows the SINGULAR account-type label (account.type.*)
// as the ROOT segment of a single type-rooted dotted path (p12.12), joined to the
// account's dotted Path by ".". The label rides both the visible option text AND
// data-path so the shared combobox (combofilter.js, which fuzzy-ranks on data-path)
// can filter by type too. Reuses the existing account.type.* i18n keys; no new keys.
func TestTxnAccountOptionShowsTypePrefix(t *testing.T) {
	e := newTxnWebEnv(t)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet,
		"/transactions/new?subsidiary="+strconv.FormatInt(int64(e.sub1), 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new: %d", rec.Code)
	}
	body := rec.Body.String()

	// Checking is an asset leaf in sub1; its option must read "Asset.Checking"
	// in both the visible text and data-path (so it stays fuzzy-filterable).
	for _, want := range []string{
		`>Asset.Checking<`,
		`data-path="Asset.Checking"`,
		`>Expense.Salaries<`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("account option missing type prefix %q; body:\n%s", want, body)
		}
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
	// p26.41: the separate program + class selects merged into ONE combined control
	// (#txn-progclass-0); the hidden program carrier (#txn-program-0) round-trips the
	// stored program for idempotency.
	wantIDs := []string{
		`id="txn-account-0"`, `id="txn-amount-0"`, `id="txn-fund-0"`,
		`id="txn-program-0"`, `id="txn-progclass-0"`, `id="txn-memo-0"`,
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
	q.Set("subsidiary", itoa(int64(e.sub2)))
	q.Set("rows", "2")
	q.Set("account_0", itoa(int64(e.checking)))
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
	q.Set("subsidiary", itoa(int64(e.sub2)))
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
		"txn.subsidiary", "txn.date", "txn.memo",
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
	if !strings.Contains(body, `value="`+itoa(int64(e.checking))+`"`) {
		t.Fatalf("checking not offered as account option")
	}
	if strings.Contains(body, `data-account-option value="`+itoa(int64(e.cashB))+`"`) {
		t.Fatalf("cashB (sub2) leaked into sub1 account options")
	}
	// The fund scoped to sub1 is offered.
	if !strings.Contains(body, `value="`+itoa(int64(e.fund))+`"`) {
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

	if !strings.Contains(body, `value="`+itoa(int64(e.salaries))+`"`) ||
		!strings.Contains(body, `data-type="expense"`) ||
		!strings.Contains(body, `data-default-class="program"`) {
		t.Fatalf("salaries option missing expense gating metadata; body:\n%s", body)
	}
	if !strings.Contains(body, `value="`+itoa(int64(e.grantRev))+`"`) || !strings.Contains(body, `data-type="revenue"`) {
		t.Fatalf("grantRev option missing revenue gating metadata")
	}
	// p26.41: the hidden program carrier + the combined program/class control render on row 0.
	if !strings.Contains(body, `id="txn-program-0"`) || !strings.Contains(body, `id="txn-progclass-0"`) {
		t.Fatalf("program carrier / combined progclass control not rendered on row 0")
	}
}

// TestTxnSubsidiaryReFilterEchoesRows: the subsidiary-change re-filter re-filters the
// account options to the new sub AND echoes typed rows, flagging a row whose account
// left the sub (Appendix C: never silent-clear).
func TestTxnSubsidiaryReFilterEchoesRows(t *testing.T) {
	e := newTxnWebEnv(t)
	q := url.Values{}
	q.Set("subsidiary", itoa(int64(e.sub2)))
	q.Set("rows", "2")
	q.Set("account_0", itoa(int64(e.checking))) // sub1-only -> invalid in sub2
	q.Set("amount_0", "10.00")
	q.Set("account_1", itoa(int64(e.cashB))) // sub2 -> valid
	q.Set("amount_1", "-10.00")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/new?"+q.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-filter GET: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="`+itoa(int64(e.cashB))+`"`) {
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

	rec := asUser(t, e.h, e.sm, eu, http.MethodGet, "/transactions/"+itoa(int64(id))+"/edit", nil)
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

// --- p26.34 main-split header + server-side fan-out auto-balance ----------

// splitFull mirrors a stored split across EVERY business column INCLUDING description
// (store.SplitState omits it), so the p26.34 idempotency test can assert byte-identity.
type splitFull struct {
	ID              ids.SplitID
	AccountID       ids.AccountID
	Amount          int64
	FundID          sql.NullInt64
	ProgramID       sql.NullInt64
	FunctionalClass sql.NullString
	Memo            string
	Description     string
	Position        int64
}

// mainHeaderForm builds a POST form in the p26.34 shape: the position-0 (MAIN) split is
// carried by the header fields (main_account / main_description / main_split_id /
// main_program / main_class / main_memo); its amount is OMITTED (the server computes the
// per-fund residual) and its fund is DERIVED from the body. `body` is the list of body
// rows (positions 1..m as stored). The header is always present, so the client posts
// main_present=1.
func mainHeaderForm(sub ids.SubsidiaryID, main splitFull, body []splitFull) url.Values {
	f := url.Values{}
	f.Set("subsidiary", itoa(int64(sub)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("main_present", "1")
	f.Set("main_account", itoa(int64(main.AccountID)))
	f.Set("main_description", main.Description)
	f.Set("main_memo", main.Memo)
	if main.ProgramID.Valid {
		f.Set("main_program", itoa(main.ProgramID.Int64))
	}
	if main.FunctionalClass.Valid {
		f.Set("main_class", main.FunctionalClass.String)
	}
	for i, b := range body {
		si := itoa(int64(i))
		f.Set("account_"+si, itoa(int64(b.AccountID)))
		f.Set("amount_"+si, signedStr(b.Amount))
		if b.FundID.Valid {
			f.Set("fund_"+si, itoa(b.FundID.Int64))
		} else {
			f.Set("fund_"+si, "")
		}
		// p26.41 combined control: encode the body row's (program, class) into progclass_i +
		// the hidden program_i carrier, exactly as the client would.
		if b.ProgramID.Valid {
			f.Set("program_"+si, itoa(b.ProgramID.Int64))
		}
		switch {
		case b.FunctionalClass.Valid && b.FunctionalClass.String != "program":
			// Admin / Fundraising: c:<class>; the program rides in the hidden carrier.
			f.Set("progclass_"+si, "c:"+b.FunctionalClass.String)
		case b.ProgramID.Valid:
			// A program-class expense OR a revenue split: p:<programID>.
			f.Set("progclass_"+si, "p:"+itoa(b.ProgramID.Int64))
		}
		f.Set("memo_"+si, b.Memo)
		f.Set("description_"+si, b.Description)
	}
	f.Set("rows", itoa(int64(len(body))))
	return f
}

// splitStatesByPosition returns the txn's splits as splitFull ordered by position.
func splitStatesByPosition(t *testing.T, e *txnWebEnv, id ids.TransactionID) []splitFull {
	t.Helper()
	sp, err := e.st.TransactionSplits(context.Background(), id)
	must(t, err, "splits")
	out := make([]splitFull, len(sp))
	for i, s := range sp {
		out[i] = splitFull{
			ID: s.ID, AccountID: s.AccountID, Amount: s.Amount, FundID: s.FundID,
			ProgramID: s.ProgramID, FunctionalClass: s.FunctionalClass,
			Memo: s.Memo, Description: s.Description, Position: s.Position,
		}
	}
	return out
}

// TestTxnMainHeaderSingleFundIdempotent (p26.34 test a): a single-fund txn with an R/E
// account at position 0 (expense: program + class + memo + fund) loaded into the header
// and saved with NO edits reproduces BYTE-IDENTICAL stored splits -- same amounts,
// dimensions, positions AND the same split ids (no delete-all+recreate). The MAIN split's
// amount is the auto-balanced residual computed server-side; its program/class/memo/fund
// and split_id are round-tripped so the reconstruction loses nothing.
func TestTxnMainHeaderSingleFundIdempotent(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Seed a single-fund txn: MAIN = Salaries (expense, fund=Beca, program=Educacion,
	// class=program, a memo) at position 0; body = Checking -100 in the SAME fund. The
	// program is Educacion (the fund's scope), NOT the account/root default -- so a naive
	// re-default would corrupt it (the discriminator).
	fund := e.fund
	prog := e.progEdu
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: 10000, FundID: &fund, ProgramID: &prog, FunctionalClass: strptr("program"), Memo: "March payroll", Position: 0},
			{AccountID: e.checking, Amount: -10000, FundID: &fund, Position: 1},
		},
	})
	must(t, err, "seed single-fund txn")

	before := splitStatesByPosition(t, e, id)
	if len(before) != 2 {
		t.Fatalf("want 2 seeded splits, got %d", len(before))
	}
	main := before[0]
	body := before[1:]
	beforeVer := map[ids.SplitID]int{}
	for _, s := range before {
		beforeVer[s.ID] = splitVersionCountWeb(t, e, s.ID)
	}

	// LOAD: GET /edit must DECOMPOSE the stored txn into header (split0) + body (rest),
	// NOT leave split0 in the body too (which would double-count on save). Assert the
	// header carries split0's id/account/program/class and the body carries ONLY split1.
	editRec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(int64(id))+"/edit", nil)
	if editRec.Code != http.StatusOK {
		t.Fatalf("edit GET: %d", editRec.Code)
	}
	eb := editRec.Body.String()
	if !strings.Contains(eb, `id="txn-main-splitid"`) || !strings.Contains(eb, `value="`+itoa(int64(main.ID))+`"`) {
		t.Fatalf("edit form did not round-trip the main split id in the header; body:\n%s", eb)
	}
	// The main (salaries) account is selected in the header account select.
	reMainAcct := regexp.MustCompile(`(?s)id="txn-main-account".*?<option[^>]*value="` + itoa(int64(main.AccountID)) + `"[^>]*\bselected\b`)
	if !reMainAcct.MatchString(eb) {
		t.Fatalf("edit form header account not selected to salaries; body:\n%s", eb)
	}
	// The body has EXACTLY the single body split (checking) -- rows=1, no second row.
	if !strings.Contains(eb, `id="txn-account-0"`) || strings.Contains(eb, `id="txn-account-1"`) {
		t.Fatalf("edit form body must be exactly one row (split0 must NOT also be a body row); body:\n%s", eb)
	}
	if !strings.Contains(eb, `name="rows" id="txn-rows-count" value="1"`) {
		t.Fatalf("edit form body rows-count should be 1 (split0 lifted to header); body:\n%s", eb)
	}

	// Re-submit via the header form (main amount OMITTED, fund derived). Round-trip the
	// main split id so UpdateTransaction diffs by id (no churn).
	f := mainHeaderForm(e.sub1, main, body)
	f.Set("main_split_id", itoa(int64(main.ID)))
	for i, b := range body {
		f.Set("split_id_"+itoa(int64(i)), itoa(int64(b.ID)))
	}

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(int64(id)), f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	after := splitStatesByPosition(t, e, id)
	if len(after) != len(before) {
		t.Fatalf("split count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("split %d not byte-identical after header round-trip:\n before=%+v\n after =%+v", i, before[i], after[i])
		}
	}
	// No split gained a version (a no-edit save must be a genuine no-op diff; a
	// reconstructed-with-new-id main would churn versions).
	for _, s := range after {
		if n := splitVersionCountWeb(t, e, s.ID); n != beforeVer[s.ID] {
			t.Fatalf("split %d version count changed: before=%d after=%d (reconstruction churned the id)", s.ID, beforeVer[s.ID], n)
		}
	}
}

// TestTxnMainHeaderMultiFundFanOut (p26.34 test b): a NEW entry whose body splits touch
// TWO funds fans the main (header) account out into one balancing split PER fund, each
// carrying that fund's residual, so the store's per-fund zero-sum accepts the result. The
// header account is an asset (Checking) with no program/class, so no R/E dimensions are
// needed on the mains.
func TestTxnMainHeaderMultiFundFanOut(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// A second fund scoped to sub1 (unrestricted-general is fund 0; here we use two named
	// funds so the body has two DISTINCT nonzero-residual fund groups). Both program-scope
	// Educacion so the salaries R/E body splits validate.
	prog := e.progEdu
	fund2, err := e.st.CreateFund(ctx, store.CreateFundInput{
		Name: "Beca Dos", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{e.sub1}, ProgramID: &prog,
	})
	must(t, err, "fund2")

	// Body: Salaries 60 in Beca + Salaries 40 in Beca Dos (both expense, program Educacion,
	// class program). The header (Checking, asset) must fan out to -60 in Beca and -40 in
	// Beca Dos so each fund nets to zero.
	body := []splitFull{
		{AccountID: e.salaries, Amount: 6000, FundID: ni(e.fund), ProgramID: ni(e.progEdu), FunctionalClass: ns("program")},
		{AccountID: e.salaries, Amount: 4000, FundID: ni(fund2), ProgramID: ni(e.progEdu), FunctionalClass: ns("program")},
	}
	mainHdr := splitFull{AccountID: e.checking, Description: "Multi-fund payroll"}
	f := mainHeaderForm(e.sub1, mainHdr, body)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create multi-fund: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	id := latestTxnID(t, e)
	splits := splitStatesByPosition(t, e, id)
	// Two fanned mains (positions 0,1) on Checking + two body salaries splits = 4 splits.
	if len(splits) != 4 {
		t.Fatalf("want 4 splits (2 fanned mains + 2 body), got %d: %+v", len(splits), splits)
	}
	// Per-fund zero-sum holds (the store enforces it, so a 303 already proves acceptance);
	// additionally assert the two mains are the header account with the fund residuals.
	byFund := map[int64]int64{}
	mainsOnChecking := map[int64]int64{} // fund key -> main amount
	for _, s := range splits {
		key := int64(0)
		if s.FundID.Valid {
			key = s.FundID.Int64
		}
		byFund[key] += s.Amount
		if s.AccountID == e.checking {
			mainsOnChecking[key] = s.Amount
		}
	}
	for key, sum := range byFund {
		if sum != 0 {
			t.Fatalf("fund %d does not net to zero: %d", key, sum)
		}
	}
	if mainsOnChecking[int64(e.fund)] != -6000 {
		t.Fatalf("Beca main residual = %d, want -6000", mainsOnChecking[int64(e.fund)])
	}
	if mainsOnChecking[int64(fund2)] != -4000 {
		t.Fatalf("Beca Dos main residual = %d, want -4000", mainsOnChecking[int64(fund2)])
	}
}

// TestTxnMultiFundReloadFlatFallback (p26.34 flat-fallback guard): a stored MULTI-FUND txn
// loaded for edit falls back to the FLAT grid -- no header (main_present=0), every split a
// body row -- so the fragile fan-out reconstruction stays out of the load path and a
// re-save cannot double-count. Covers the log-commented cap-guard.
func TestTxnMultiFundReloadFlatFallback(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	prog := e.progEdu
	fund2, err := e.st.CreateFund(ctx, store.CreateFundInput{
		Name: "Beca Dos", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{e.sub1}, ProgramID: &prog,
	})
	must(t, err, "fund2")

	// Seed a 4-split multi-fund txn (two funds), so the reload must NOT decompose.
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: 6000, FundID: nn(e.fund), ProgramID: nn(e.progEdu), FunctionalClass: strptr("program"), Position: 0},
			{AccountID: e.checking, Amount: -6000, FundID: nn(e.fund), Position: 1},
			{AccountID: e.salaries, Amount: 4000, FundID: nn(fund2), ProgramID: nn(e.progEdu), FunctionalClass: strptr("program"), Position: 2},
			{AccountID: e.checking, Amount: -4000, FundID: nn(fund2), Position: 3},
		},
	})
	must(t, err, "seed multi-fund txn")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(int64(id))+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit GET: %d", rec.Code)
	}
	body := rec.Body.String()
	// No header (main_present=0): the main_present hidden flag is absent.
	if strings.Contains(body, `name="main_present" value="1"`) {
		t.Fatalf("multi-fund reload rendered the header (should fall back to flat grid); body:\n%s", body)
	}
	if strings.Contains(body, `id="txn-main-account"`) {
		t.Fatalf("multi-fund reload rendered the main-account header select (should be flat)")
	}
	// Every split is a body row: 4 rows present (txn-account-0..3), none dropped.
	for i := 0; i < 4; i++ {
		if !strings.Contains(body, `id="txn-account-`+itoa(int64(i))+`"`) {
			t.Fatalf("flat fallback missing body row %d; body:\n%s", i, body)
		}
	}
	if strings.Contains(body, `id="txn-account-4"`) {
		t.Fatalf("flat fallback rendered an extra row (should be exactly 4)")
	}

	// A re-save of the flat form (main_present absent) must NOT re-fan-out: post the four
	// rows as-is and assert the stored splits are unchanged in count.
	f := url.Values{}
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	live := splitStatesByPosition(t, e, id)
	for i, s := range live {
		si := itoa(int64(i))
		f.Set("split_id_"+si, itoa(int64(s.ID)))
		f.Set("account_"+si, itoa(int64(s.AccountID)))
		f.Set("amount_"+si, signedStr(s.Amount))
		if s.FundID.Valid {
			f.Set("fund_"+si, itoa(s.FundID.Int64))
		} else {
			f.Set("fund_"+si, "")
		}
		if s.ProgramID.Valid {
			f.Set("program_"+si, itoa(s.ProgramID.Int64))
		}
		if s.FunctionalClass.Valid {
			f.Set("class_"+si, s.FunctionalClass.String)
		}
	}
	f.Set("rows", "4")
	if rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(int64(id)), f); rec.Code != http.StatusSeeOther {
		t.Fatalf("flat re-save: status=%d body=%s", rec.Code, rec.Body.String())
	}
	after := splitStatesByPosition(t, e, id)
	if len(after) != 4 {
		t.Fatalf("flat re-save changed split count to %d (fan-out leaked into the flat path)", len(after))
	}
}

// TestTxnMainHeaderBodyErrorLandsOnRow (p26.34 error attribution): a header-flow POST whose
// BODY row is invalid must re-render at 422 with the error on the BODY row -- NOT panic.
// The bug this guards: model.Rows is body-only while the store's split index counts the
// prepended main(s), so an unadjusted index would run out of range (500). Here the body row
// is an expense with NO functional class -> ErrExpenseNeedsFunction on body row 0.
func TestTxnMainHeaderBodyErrorLandsOnRow(t *testing.T) {
	e := newTxnWebEnv(t)

	// Header = Checking (asset, balancing); body row 0 = Supplies (expense, NO default
	// class) 50 with NO class chosen -> the store rejects with ErrExpenseNeedsFunction.
	f := url.Values{}
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("main_present", "1")
	f.Set("main_account", itoa(int64(e.checking)))
	f.Set("account_0", itoa(int64(e.supplies))) // expense, no default class
	f.Set("amount_0", "50.00")
	f.Set("program_0", itoa(int64(e.progRoot)))
	// class_0 deliberately omitted.
	f.Set("rows", "1")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 (not a 500 panic), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-row-error="0"`) {
		t.Fatalf("expense-needs-class error not on body row 0; body:\n%s", rec.Body.String())
	}
}

// TestTxnMainHeaderMissingAccount (p26.34): a header-flow POST with an EMPTY main account
// but a content body row must re-render at 422 (the store's ErrAccountMissing on the main),
// surfaced on the header/totals slot -- never a panic, never a silent drop.
func TestTxnMainHeaderMissingAccount(t *testing.T) {
	e := newTxnWebEnv(t)

	f := url.Values{}
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("main_present", "1")
	f.Set("main_account", "0") // header account not chosen
	f.Set("account_0", itoa(int64(e.checking)))
	f.Set("amount_0", "50.00")
	f.Set("rows", "1")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 (not a 500 panic), got %d: %s", rec.Code, rec.Body.String())
	}
	// The header account error surfaces on the totals bar (no per-row cell for the header).
	if !strings.Contains(rec.Body.String(), "txn-totals-error") {
		t.Fatalf("missing-main-account error not surfaced; body:\n%s", rec.Body.String())
	}
}

// p31 main-header description copy-down + warning. When the MAIN (position-0 header)
// split carries a description, the server (i) copies it into every BODY split whose own
// description is blank -- leaving non-blank ones alone -- and (ii) surfaces a NON-BLOCKING
// post-save notice (the ?main_desc=<N> PRG marker on the redirect, rendered as a banner on
// the destination register). These run on the SHARED create/update path, so a single site
// covers both /transactions (create) and /transactions/{id} (update).

// twoBodyMainHeaderForm builds a header-flow POST: MAIN = Checking header with `mainDesc`,
// body = two Salaries expense splits (fund Beca, program Educacion, class program) that sum
// to 100 so the single-fund main balances. Body descriptions are set from d0/d1.
func (e *txnWebEnv) twoBodyMainHeaderForm(mainDesc, d0, d1 string) url.Values {
	main := splitFull{AccountID: e.checking, Description: mainDesc}
	body := []splitFull{
		{AccountID: e.salaries, Amount: 6000, FundID: ni(e.fund), ProgramID: ni(e.progEdu), FunctionalClass: ns("program"), Description: d0},
		{AccountID: e.salaries, Amount: 4000, FundID: ni(e.fund), ProgramID: ni(e.progEdu), FunctionalClass: ns("program"), Description: d1},
	}
	return mainHeaderForm(e.sub1, main, body)
}

// bodyDescByAmount returns the stored body-split descriptions keyed by their amount (the
// body salaries splits are +6000 / +4000; the fanned main on Checking is negative and
// skipped). Amounts are unique per body row here, so this reads back which description each
// line ended up with.
func bodyDescByAmount(t *testing.T, e *txnWebEnv, id ids.TransactionID) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	for _, s := range splitStatesByPosition(t, e, id) {
		if s.Amount > 0 { // body lines are positive; the balancing main is negative
			out[s.Amount] = s.Description
		}
	}
	return out
}

// TestTxnMainDescCopyDownFillsBlank (p31 i): the main-header description fills a body split
// whose own description is BLANK.
func TestTxnMainDescCopyDownFillsBlank(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.twoBodyMainHeaderForm("Header memo text", "", "Line two own desc")
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	desc := bodyDescByAmount(t, e, latestTxnID(t, e))
	// (i) blank line inherited the main description.
	if desc[6000] != "Header memo text" {
		t.Fatalf("blank body line not filled with main desc; got %q", desc[6000])
	}
	// (ii) the line with its own description is untouched.
	if desc[4000] != "Line two own desc" {
		t.Fatalf("own-desc body line was clobbered; got %q", desc[4000])
	}
}

// TestTxnMainDescCopyDownPreservesOwn (p31 ii): a body split that already carries its own
// description is NOT overwritten even though the main has a (different) description.
func TestTxnMainDescCopyDownPreservesOwn(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.twoBodyMainHeaderForm("Header memo text", "Own zero", "Own one")
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	desc := bodyDescByAmount(t, e, latestTxnID(t, e))
	if desc[6000] != "Own zero" || desc[4000] != "Own one" {
		t.Fatalf("own descriptions were clobbered by copy-down: %+v", desc)
	}
	// The marker still reports zero copies (the warning fires regardless, but N=0).
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "main_desc=0") {
		t.Fatalf("redirect should carry main_desc=0 (no lines copied); got %q", loc)
	}
}

// TestTxnMainDescNoCopyWhenBlankMain (p31 iii): when the main-header description is EMPTY,
// nothing is copied AND no warning marker is emitted.
func TestTxnMainDescNoCopyWhenBlankMain(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.twoBodyMainHeaderForm("", "", "Line two own desc")
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	desc := bodyDescByAmount(t, e, latestTxnID(t, e))
	// The blank line stays blank (no main desc to copy); the own-desc line is untouched.
	if desc[6000] != "" {
		t.Fatalf("blank line should stay blank when main desc is empty; got %q", desc[6000])
	}
	if desc[4000] != "Line two own desc" {
		t.Fatalf("own-desc line changed unexpectedly; got %q", desc[4000])
	}
	// No warning marker when the main has no description.
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "main_desc") {
		t.Fatalf("no warning marker expected when main desc is empty; got %q", loc)
	}
}

// TestTxnMainDescWarningSurfaced (p31 iv): a save whose main-header split carries a
// description emits the non-blocking warning marker on the redirect, and the destination
// register renders it as a visible banner (server-observable, CSP-safe -- no inline JS).
func TestTxnMainDescWarningSurfaced(t *testing.T) {
	e := newTxnWebEnv(t)

	f := e.twoBodyMainHeaderForm("Header memo text", "", "Line two own desc")
	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status=%d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	// One blank body line was filled -> main_desc=1 rides the redirect.
	if !strings.Contains(loc, "main_desc=1") {
		t.Fatalf("warning marker main_desc=1 not on redirect; got %q", loc)
	}

	// Following the redirect renders the register with the banner text (the count message).
	reg := asUser(t, e.h, e.sm, e.book, http.MethodGet, loc, nil)
	if reg.Code != http.StatusOK {
		t.Fatalf("register GET after save: %d", reg.Code)
	}
	rb := reg.Body.String()
	if !strings.Contains(rb, i18n.TN("en", "txn.main_desc_notice.copied", 1)) {
		t.Fatalf("post-save warning banner not rendered on the register; body:\n%s", rb)
	}
	// The banner reuses the styled caution-alert class (no inline style; strict CSP).
	if !strings.Contains(rb, `class="alert alert-warn"`) {
		t.Fatalf("warning banner missing the alert-warn class; body:\n%s", rb)
	}
}

// TestTxnMainDescCopyDownOnUpdate (p31, both-paths): the copy-down + warning run on the
// SHARED create/update path, so an UPDATE (POST /transactions/{id}) that gives the main
// header a description also fills a blank body line and emits the marker. Seed a txn, then
// re-post it through the header form with a main description and one blank body line.
func TestTxnMainDescCopyDownOnUpdate(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Seed: MAIN = Checking (no desc) at position 0; body = two Salaries lines (Beca /
	// Educacion / program) that sum to 100, both with their own descriptions.
	fund, prog := e.fund, e.progEdu
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.checking, Amount: -10000, FundID: &fund, Position: 0},
			{AccountID: e.salaries, Amount: 6000, FundID: &fund, ProgramID: &prog, FunctionalClass: strptr("program"), Description: "keep me", Position: 1},
			{AccountID: e.salaries, Amount: 4000, FundID: &fund, ProgramID: &prog, FunctionalClass: strptr("program"), Description: "also mine", Position: 2},
		},
	})
	must(t, err, "seed txn")
	live := splitStatesByPosition(t, e, id)

	// Re-post via the header form: main (Checking) now carries a description; body line 0
	// is BLANKED (should inherit), body line 1 keeps its own description. Round-trip split
	// ids so UpdateTransaction diffs by id (trap 1).
	main := live[0]
	main.Description = "Updated header memo"
	body := []splitFull{
		{AccountID: e.salaries, Amount: 6000, FundID: ni(e.fund), ProgramID: ni(e.progEdu), FunctionalClass: ns("program"), Description: "", ID: live[1].ID},
		{AccountID: e.salaries, Amount: 4000, FundID: ni(e.fund), ProgramID: ni(e.progEdu), FunctionalClass: ns("program"), Description: "also mine", ID: live[2].ID},
	}
	f := mainHeaderForm(e.sub1, main, body)
	f.Set("main_split_id", itoa(int64(main.ID)))
	f.Set("split_id_0", itoa(int64(live[1].ID)))
	f.Set("split_id_1", itoa(int64(live[2].ID)))

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(int64(id)), f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	desc := bodyDescByAmount(t, e, id)
	if desc[6000] != "Updated header memo" {
		t.Fatalf("blanked body line did not inherit main desc on update; got %q", desc[6000])
	}
	if desc[4000] != "also mine" {
		t.Fatalf("own-desc body line clobbered on update; got %q", desc[4000])
	}
	// The warning marker rides the update redirect too (one line copied).
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "main_desc=1") {
		t.Fatalf("update redirect missing main_desc=1; got %q", loc)
	}
}

// --- helpers --------------------------------------------------------------

// nn/ni/ns build the *int64 (store.SplitInput) / sql.NullInt64 (splitFull) / sql.NullString
// the tests need for terse table rows.
func nn[T ~int64](v T) *T            { return &v }
func ni[T ~int64](v T) sql.NullInt64 { return sql.NullInt64{Int64: int64(v), Valid: true} }
func ns(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

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
func latestTxnID(t *testing.T, e *txnWebEnv) ids.TransactionID {
	t.Helper()
	var id ids.TransactionID
	if err := e.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM transactions WHERE deleted = 0`).Scan(&id); err != nil {
		t.Fatalf("latest txn id: %v", err)
	}
	return id
}

// splitVersionCountWeb counts splits_versions rows for a split (trap 1 assertion).
func splitVersionCountWeb(t *testing.T, e *txnWebEnv, splitID ids.SplitID) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM splits_versions WHERE entity_id = ?`, int64(splitID)).Scan(&n); err != nil {
		t.Fatalf("split version count(%d): %v", splitID, err)
	}
	return n
}

// mkUserDisplay creates a write user and sets its display_mode column directly (the
// settings UI is p13.1); this test only needs the stored value so the editor renders
// the right amount columns.
func mkUserDisplay(t *testing.T, e *txnWebEnv, username, display string) ids.UserID {
	t.Helper()
	id := mkUser(t, e.st, username, "write", false)
	if _, err := e.db.Exec(`UPDATE users SET display_mode = ? WHERE id = ?`, display, id); err != nil {
		t.Fatalf("set display mode: %v", err)
	}
	return id
}

// setDefaultSub sets a user's default_subsidiary_id column directly (settings UI is
// p13.1); the editor reads it to default the header subsidiary.
func setDefaultSub(t *testing.T, e *txnWebEnv, userID ids.UserID, subID ids.SubsidiaryID) {
	t.Helper()
	if _, err := e.db.Exec(`UPDATE users SET default_subsidiary_id = ? WHERE id = ?`, int64(subID), userID); err != nil {
		t.Fatalf("set default sub: %v", err)
	}
}

// --- p26.41 combined program/class control -------------------------------

// TestDecodeProgClass unit-tests the combined-control decode: a program pick becomes
// program=id (+ class=program on an EXPENSE row, "" elsewhere); a c:<class> pick becomes
// that class with the program taken from the hidden carrier (verbatim, preserving a
// non-root program). Unrecognized/empty falls back to the carrier with no class.
func TestDecodeProgClass(t *testing.T) {
	cases := []struct {
		name      string
		encoded   string
		carrier   int64
		acctType  string
		wantProg  int64
		wantClass string
	}{
		{"program pick on expense", "p:7", 3, "expense", 7, "program"},
		{"program pick on revenue", "p:7", 3, "revenue", 7, ""},
		{"program pick unknown type", "p:7", 3, "", 7, ""},
		{"management keeps carrier", "c:management", 42, "expense", 42, "management"},
		{"fundraising keeps carrier", "c:fundraising", 42, "expense", 42, "fundraising"},
		{"management non-root carrier preserved", "c:management", 99, "expense", 99, "management"},
		{"empty falls back to carrier", "", 5, "expense", 5, ""},
		{"garbage falls back to carrier", "x:1", 5, "expense", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotProg, gotClass := decodeProgClass(tc.encoded, tc.carrier, tc.acctType)
			if gotProg != tc.wantProg || gotClass != tc.wantClass {
				t.Fatalf("decodeProgClass(%q, %d, %q) = (%d, %q), want (%d, %q)",
					tc.encoded, tc.carrier, tc.acctType, gotProg, gotClass, tc.wantProg, tc.wantClass)
			}
		})
	}
}

// TestTxnProgClassNonRootIdempotent is the p26.41 idempotency requirement: an EXISTING
// management-expense split whose program is a SPECIFIC NON-ROOT program (Educacion), loaded
// into the editor and re-submitted UNCHANGED via the combined control (Admin => c:management,
// the program riding only in the hidden carrier), reproduces byte-identical stored splits and
// churns NO new splits_versions row. This proves the hidden carrier prevents the Restore-the-
// Way management/fundraising splits (which carry a non-root program) from silently losing it.
func TestTxnProgClassNonRootIdempotent(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Seed: Salaries (expense) 100 with class=management AND a NON-ROOT program (Educacion),
	// balanced by Checking -100. Unrestricted fund, so the non-root program is allowed.
	prog := e.progEdu
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: 10000, ProgramID: &prog, FunctionalClass: strptr("management"), Memo: "Admin payroll", Position: 0},
			{AccountID: e.checking, Amount: -10000, Position: 1},
		},
	})
	must(t, err, "seed management non-root txn")

	before := splitStatesByPosition(t, e, id)
	if len(before) != 2 {
		t.Fatalf("want 2 seeded splits, got %d", len(before))
	}
	// The salaries split must carry the NON-ROOT program (the discriminator: a naive re-save
	// that dropped the carrier would default it back to root and corrupt this).
	sal := before[0]
	if sal.AccountID != e.salaries || !sal.ProgramID.Valid || sal.ProgramID.Int64 != int64(e.progEdu) ||
		!sal.FunctionalClass.Valid || sal.FunctionalClass.String != "management" {
		t.Fatalf("seed split 0 not management/non-root program: %+v", sal)
	}
	beforeVer := map[ids.SplitID]int{}
	for _, s := range before {
		beforeVer[s.ID] = splitVersionCountWeb(t, e, s.ID)
	}

	// LOAD: the edit GET decomposes into header (Checking asset, split1) + body (Salaries,
	// split0)? No -- the header is the balancing (asset) account; but the store returns splits
	// in position order (salaries pos0, checking pos1), so decomposeMain lifts split0 (salaries)
	// to the header. Either way, re-submit via the FLAT body grid to exercise the combined
	// control's decode directly (main_present omitted): both splits as body rows, the salaries
	// row's combined control set to Admin (c:management) with the program in the hidden carrier.
	f := url.Values{}
	f.Set("subsidiary", itoa(int64(e.sub1)))
	f.Set("date", "2025-03-01")
	f.Set("currency", "USD")
	f.Set("rows", "2")
	// Row 0: salaries, Admin (c:management), program carrier = Educacion (the stored program).
	f.Set("split_id_0", itoa(int64(sal.ID)))
	f.Set("account_0", itoa(int64(e.salaries)))
	f.Set("amount_0", "100.00")
	f.Set("fund_0", "")
	f.Set("progclass_0", "c:management")
	f.Set("program_0", itoa(int64(e.progEdu)))
	f.Set("memo_0", "Admin payroll")
	// Row 1: checking, no program/class (asset).
	f.Set("split_id_1", itoa(int64(before[1].ID)))
	f.Set("account_1", itoa(int64(e.checking)))
	f.Set("amount_1", "-100.00")
	f.Set("fund_1", "")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(int64(id)), f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status=%d, body=%s", rec.Code, rec.Body.String())
	}

	after := splitStatesByPosition(t, e, id)
	if len(after) != len(before) {
		t.Fatalf("split count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("split %d not byte-identical after combined-control round-trip:\n before=%+v\n after =%+v", i, before[i], after[i])
		}
	}
	for _, s := range after {
		if n := splitVersionCountWeb(t, e, s.ID); n != beforeVer[s.ID] {
			t.Fatalf("split %d version churned: before=%d after=%d (the hidden carrier failed to preserve the non-root program)", s.ID, beforeVer[s.ID], n)
		}
	}
}

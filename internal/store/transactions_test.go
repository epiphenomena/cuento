package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"cuento/internal/testutil"
)

// Transaction operations (p08.2) -- the CORE financial logic (D2, D18, D20, D21,
// D24). These tests build tiny bespoke charts inline (AGENTS testing conventions);
// the canonical Fixture arrives in p09.1. The seeded root subsidiary is id 1
// (rootID) and the seeded root program is id 1 (rootProgramID, from programs_test).

// txnEnv is a small chart the transaction tests share: a US subsidiary, a checking
// asset, a salaries expense (with a default functional class + default program), a
// contributions revenue, an equity account, and a program tree. Accounts map to
// the US sub. Callers pick which accounts a given test uses.
type txnEnv struct {
	s     *Store
	d     *sql.DB
	subUS int64

	checking int64 // asset, US
	credit   int64 // liability, US
	salaries int64 // expense, default class=management, default program=root
	supplies int64 // expense, NO default class, default program=root
	contrib  int64 // revenue, default program=root
	equity   int64 // equity, US

	educ int64 // program under root
}

func newTxnEnv(t *testing.T) txnEnv {
	t.Helper()
	d := testutil.NewDB(t)
	s := New(d)
	subUS := newSub(t, s, rootID, "US")

	educ, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Educacion"})
	if err != nil {
		t.Fatalf("CreateProgram: %v", err)
	}

	env := txnEnv{s: s, d: d, subUS: subUS, educ: educ}
	root := rootProgramID
	mgmt := "management"
	env.checking = mkAcct(t, s, "asset", "Checking", []int64{subUS}, nil, nil)
	env.credit = mkAcct(t, s, "liability", "Credit Card", []int64{subUS}, nil, nil)
	env.salaries = mkAcct(t, s, "expense", "Salaries", []int64{subUS}, &mgmt, &root)
	env.supplies = mkAcct(t, s, "expense", "Supplies", []int64{subUS}, nil, nil)
	env.contrib = mkAcct(t, s, "revenue", "Contributions", []int64{subUS}, nil, nil)
	env.equity = mkAcct(t, s, "equity", "Opening Balances", []int64{subUS}, nil, nil)
	return env
}

// rootProgramMarker points at the root program id; tests take its address when a
// split explicitly tags the root program.
var rootProgramMarker int64 = rootProgramID

// mkAcct creates a leaf account of the given type mapped to subs, optionally with a
// default functional class and default program, and returns its id.
func mkAcct(t *testing.T, s *Store, typ, name string, subs []int64, fClass *string, defProg *int64) int64 {
	t.Helper()
	in := CreateAccountInput{
		Type: typ, DefaultCurrency: "USD", Names: enName(name), Subsidiaries: subs,
	}
	if fClass != nil {
		v := *fClass
		in.FunctionalClass = &v
	}
	if defProg != nil {
		p := *defProg
		in.DefaultProgramID = &p
	}
	id, err := s.CreateAccount(mutCtx(), in)
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", name, err)
	}
	return id
}

// txnSplits reads a transaction's live split rows for assertions.
func txnSplits(t *testing.T, d *sql.DB, txnID int64) []SplitState {
	t.Helper()
	rows, err := d.Query(`SELECT id, account_id, amount, fund_id, program_id, functional_class, memo, position
		FROM splits WHERE transaction_id = ? ORDER BY position, id`, txnID)
	if err != nil {
		t.Fatalf("txnSplits: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SplitState
	for rows.Next() {
		var sp SplitState
		if err := rows.Scan(&sp.ID, &sp.AccountID, &sp.Amount, &sp.FundID, &sp.ProgramID, &sp.FunctionalClass, &sp.Memo, &sp.Position); err != nil {
			t.Fatalf("scan split: %v", err)
		}
		out = append(out, sp)
	}
	return out
}

// splitVersionCount returns how many splits_versions rows exist for one split id.
func splitVersionCount(t *testing.T, d *sql.DB, splitID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM splits_versions WHERE entity_id = ?`, splitID).Scan(&n); err != nil {
		t.Fatalf("splitVersionCount(%d): %v", splitID, err)
	}
	return n
}

// balancedInput is a simple two-split balanced txn: debit expense, credit checking,
// same fund (nil = unrestricted). amount is the expense debit (positive).
func (e txnEnv) balancedInput(amount int64) PostTransactionInput {
	return PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: amount, Position: 0},
			{AccountID: e.checking, Amount: -amount, Position: 1},
		},
	}
}

// assertRejected posts `in`, asserts the error IS wantErr, AND that the funnel
// rolled back (the changes count is unchanged -- no reject leaves an audit trace).
func (e txnEnv) assertRejected(t *testing.T, in PostTransactionInput, wantErr error) {
	t.Helper()
	before := countChanges(t, e.d)
	_, err := e.s.PostTransaction(mutCtx(), in)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected post leaves no trace)", n, before)
	}
}

// --- Post: happy path + versioning ---------------------------------------

func TestPostBalanced(t *testing.T) {
	e := newTxnEnv(t)
	before := countChanges(t, e.d)

	id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(10_000))
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	if id <= 0 {
		t.Fatalf("PostTransaction returned id %d", id)
	}
	// Exactly one change for the whole post.
	if n := countChanges(t, e.d); n != before+1 {
		t.Fatalf("changes = %d, want %d (one change for the post)", n, before+1)
	}
	// The txn and EVERY split are versioned op=create under that one change.
	testutil.AssertVersioned(t, e.d, "transactions", id, "create")
	sps := txnSplits(t, e.d, id)
	if len(sps) != 2 {
		t.Fatalf("live splits = %d, want 2", len(sps))
	}
	for _, sp := range sps {
		testutil.AssertVersioned(t, e.d, "splits", sp.ID, "create")
	}
}

// TestTransactionNotesRoundTrip (p24.2): the header-level notes field persists on
// post, reconstructs as-of (Z3 snapshot parity), updates in place, and the change
// surfaces as a FieldNotes header diff in the timeline.
func TestTransactionNotesRoundTrip(t *testing.T) {
	e := newTxnEnv(t)

	in := e.balancedInput(10_000)
	in.Notes = "Board-approved travel reimbursement; see attached itinerary."
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// Live header carries the notes.
	hdr, err := e.s.GetTransaction(mutCtx(), id)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if hdr.Notes != in.Notes {
		t.Errorf("live notes = %q, want %q", hdr.Notes, in.Notes)
	}

	// As-of reconstruction sees the same notes (the versions snapshot includes it).
	st, err := e.s.TransactionAsOf(mutCtx(), id, time.Now())
	if err != nil {
		t.Fatalf("TransactionAsOf: %v", err)
	}
	if !st.Present || st.Notes != in.Notes {
		t.Errorf("as-of notes = %q (present=%v), want %q", st.Notes, st.Present, in.Notes)
	}

	// Update the notes (same balanced splits); the live header reflects the new value.
	upd := e.balancedInput(10_000)
	upd.Notes = "Corrected: reimbursement, not an advance."
	if err := e.s.UpdateTransaction(mutCtx(), id, upd); err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}
	hdr2, err := e.s.GetTransaction(mutCtx(), id)
	if err != nil {
		t.Fatalf("GetTransaction (post-update): %v", err)
	}
	if hdr2.Notes != upd.Notes {
		t.Errorf("updated notes = %q, want %q", hdr2.Notes, upd.Notes)
	}

	// The timeline records the notes change as a FieldNotes header diff.
	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory: %v", err)
	}
	found := false
	for _, ent := range entries {
		for _, d := range ent.HeaderDiffs {
			if d.Field == FieldNotes && d.New.Text == upd.Notes {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("history has no FieldNotes diff with the updated notes text")
	}
}

// TestSplitDescriptionRoundTrip (p26.15): a per-split free-text description survives
// post, the editor-state read (TransactionSplits), and an edit; and the
// splits_versions snapshot carries it (rule 5). INERT: no read OUTPUT consumes it yet.
func TestSplitDescriptionRoundTrip(t *testing.T) {
	e := newTxnEnv(t)

	in := e.balancedInput(10_000)
	in.Splits[0].Description = "Q1 airfare to the field office"
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// The editor-state read (TransactionSplits -> sqlc.Split) round-trips it.
	splits, err := e.s.TransactionSplits(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionSplits: %v", err)
	}
	if len(splits) != 2 {
		t.Fatalf("splits = %d, want 2", len(splits))
	}
	if splits[0].Description != in.Splits[0].Description {
		t.Errorf("split[0] description = %q, want %q", splits[0].Description, in.Splits[0].Description)
	}
	if splits[1].Description != "" {
		t.Errorf("split[1] description = %q, want empty", splits[1].Description)
	}

	// The splits_versions snapshot for split[0] carries the description (rule 5).
	testutil.AssertVersioned(t, e.d, "splits", splits[0].ID, "create")
	var snapDesc string
	if err := e.d.QueryRow(
		`SELECT description FROM splits_versions WHERE entity_id = ? ORDER BY id DESC LIMIT 1`,
		splits[0].ID,
	).Scan(&snapDesc); err != nil {
		t.Fatalf("read splits_versions description: %v", err)
	}
	if snapDesc != in.Splits[0].Description {
		t.Errorf("snapshot description = %q, want %q", snapDesc, in.Splits[0].Description)
	}

	// Editing the description updates the live split + appends an 'update' version.
	upd := e.balancedInput(10_000)
	upd.Splits[0].ID = &splits[0].ID
	upd.Splits[0].Description = "Corrected: Q1 airfare, coach"
	upd.Splits[1].ID = &splits[1].ID
	if err := e.s.UpdateTransaction(mutCtx(), id, upd); err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}
	splits2, err := e.s.TransactionSplits(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionSplits (post-update): %v", err)
	}
	if splits2[0].Description != upd.Splits[0].Description {
		t.Errorf("updated split[0] description = %q, want %q", splits2[0].Description, upd.Splits[0].Description)
	}
	testutil.AssertVersioned(t, e.d, "splits", splits[0].ID, "update")

	// Z3 stays clean with a POPULATED description (current live row == latest snapshot).
	assertLedgerClean(t, e.d)
}

// --- Post: zero-sum rejections (two distinct errors) ---------------------

func TestPostUnbalancedRejected(t *testing.T) {
	e := newTxnEnv(t)
	before := countChanges(t, e.d)
	// All unrestricted so only OVERALL can trip. 10,000 debit vs -9,000 credit.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 10_000, Position: 0},
			{AccountID: e.checking, Amount: -9_000, Position: 1},
		},
	}
	_, err := e.s.PostTransaction(mutCtx(), in)
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("err = %v, want ErrUnbalanced", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected post leaves no trace)", n, before)
	}
}

func TestPostFundUnbalancedRejected(t *testing.T) {
	e := newTxnEnv(t)
	// Two funds both scoped to US; overall balanced but each fund skewed.
	fundA := mkFund(t, e.s, "Grant A", []int64{e.subUS}, nil)
	fundB := mkFund(t, e.s, "Grant B", []int64{e.subUS}, nil)
	before := countChanges(t, e.d)
	// Overall: +10000 -10000 = 0. Fund A: +10000 (salaries) -0. Fund B: -10000.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 10_000, FundID: &fundA, Position: 0},
			{AccountID: e.checking, Amount: -10_000, FundID: &fundB, Position: 1},
		},
	}
	_, err := e.s.PostTransaction(mutCtx(), in)
	if !errors.Is(err, ErrFundUnbalanced) {
		t.Fatalf("err = %v, want ErrFundUnbalanced", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected)", n, before)
	}
}

func TestPostMixedFundsBalanced(t *testing.T) {
	e := newTxnEnv(t)
	grant := mkFund(t, e.s, "Grant", []int64{e.subUS}, nil)
	// 60/40 grant/unrestricted expense with a correspondingly split cash side.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 6_000, FundID: &grant, Position: 0},
			{AccountID: e.checking, Amount: -6_000, FundID: &grant, Position: 1},
			{AccountID: e.salaries, Amount: 4_000, Position: 2},
			{AccountID: e.checking, Amount: -4_000, Position: 3},
		},
	}
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction (mixed funds): %v", err)
	}
	if len(txnSplits(t, e.d, id)) != 4 {
		t.Errorf("want 4 splits")
	}
}

// --- Post: structural rejections -----------------------------------------

func TestPostSingleSplitRejected(t *testing.T) {
	e := newTxnEnv(t)
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{{AccountID: e.checking, Amount: 0, Position: 0}},
	}
	e.assertRejected(t, in, ErrTooFewSplits)
}

func TestPostPlaceholderRejected(t *testing.T) {
	e := newTxnEnv(t)
	// Make `checking` a placeholder by giving it a child.
	child := mkAcct(t, e.s, "asset", "Sub-checking", []int64{e.subUS}, nil, nil)
	if err := e.s.UpdateAccount(mutCtx(), child, UpdateAccountInput{ParentID: &e.checking}); err != nil {
		t.Fatalf("reparent child under checking: %v", err)
	}
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 100, Position: 0},
			{AccountID: e.checking, Amount: -100, Position: 1},
		},
	}
	e.assertRejected(t, in, ErrPlaceholderAccount)
}

func TestPostInactiveAccountRejected(t *testing.T) {
	e := newTxnEnv(t)
	if err := e.s.DeactivateAccount(mutCtx(), e.checking); err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}
	e.assertRejected(t, e.balancedInput(100), ErrInactiveAccount)
}

func TestPostAccountNotInSubsidiary(t *testing.T) {
	e := newTxnEnv(t)
	// An account mapped only to a DIFFERENT sub.
	other := newSub(t, e.s, rootID, "Other")
	foreign := mkAcct(t, e.s, "asset", "Foreign Cash", []int64{other}, nil, nil)
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 100, Position: 0},
			{AccountID: foreign, Amount: -100, Position: 1},
		},
	}
	e.assertRejected(t, in, ErrAccountNotInSubsidiary)
}

func TestPostInactiveSubsidiaryRejected(t *testing.T) {
	e := newTxnEnv(t)
	// Deactivate a fresh leaf subsidiary (root can't be deactivated / has children).
	dead := newSub(t, e.s, rootID, "Dead")
	acct := mkAcct(t, e.s, "asset", "Dead Cash", []int64{dead}, nil, nil)
	acct2 := mkAcct(t, e.s, "asset", "Dead Savings", []int64{dead}, nil, nil)
	if err := e.s.DeactivateSubsidiary(mutCtx(), dead); err != nil {
		t.Fatalf("DeactivateSubsidiary: %v", err)
	}
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: dead, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: acct, Amount: 100, Position: 0},
			{AccountID: acct2, Amount: -100, Position: 1},
		},
	}
	e.assertRejected(t, in, ErrInactiveSubsidiary)
}

func TestPostFundSubsidiaryScope(t *testing.T) {
	e := newTxnEnv(t)
	// A fund scoped to US only; a third sub is out of scope.
	subMX := newSub(t, e.s, rootID, "MX")
	fund := mkFund(t, e.s, "Scoped", []int64{e.subUS, subMX}, nil)
	// Posts fine in US.
	in := e.balancedInput(100)
	in.Splits[1].FundID = &fund
	in.Splits[0].FundID = &fund
	if _, err := e.s.PostTransaction(mutCtx(), in); err != nil {
		t.Fatalf("post in scoped sub US: %v", err)
	}
	// A third sub not in the fund's scope is rejected.
	subZ := newSub(t, e.s, rootID, "Z")
	acctZ := mkAcct(t, e.s, "asset", "Z Cash", []int64{subZ}, nil, nil)
	acctZ2 := mkAcct(t, e.s, "expense", "Z Exp", []int64{subZ}, nil, nil)
	inZ := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: subZ, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: acctZ2, Amount: 100, FundID: &fund, Position: 0},
			{AccountID: acctZ, Amount: -100, FundID: &fund, Position: 1},
		},
	}
	e.assertRejected(t, inZ, ErrFundSubsidiaryScope)
}

func TestPostInactiveFundRejected(t *testing.T) {
	e := newTxnEnv(t)
	fund := mkFund(t, e.s, "Closed", []int64{e.subUS}, nil)
	if err := e.s.CloseFund(mutCtx(), fund); err != nil {
		t.Fatalf("CloseFund: %v", err)
	}
	in := e.balancedInput(100)
	in.Splits[0].FundID = &fund
	in.Splits[1].FundID = &fund
	e.assertRejected(t, in, ErrInactiveFund)
}

// --- Post: functional class / program ------------------------------------

func TestPostExpenseRequiresFunction(t *testing.T) {
	e := newTxnEnv(t)
	// supplies is an expense account with NO default functional class; the split
	// omits it -> ErrExpenseNeedsFunction.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.supplies, Amount: 100, Position: 0},
			{AccountID: e.checking, Amount: -100, Position: 1},
		},
	}
	e.assertRejected(t, in, ErrExpenseNeedsFunction)
	// Providing the class explicitly makes it post.
	cls := "program"
	in.Splits[0].FunctionalClass = &cls
	if _, err := e.s.PostTransaction(mutCtx(), in); err != nil {
		t.Fatalf("post with explicit class: %v", err)
	}
}

func TestPostNonExpenseFunctionRejected(t *testing.T) {
	e := newTxnEnv(t)
	cls := "program"
	in := e.balancedInput(100)
	in.Splits[1].FunctionalClass = &cls // checking is an asset
	e.assertRejected(t, in, ErrNonExpenseFunction)
}

func TestPostProgramDefaulted(t *testing.T) {
	e := newTxnEnv(t)
	// contrib (revenue) has NO default program -> root. salaries default program is
	// root too. Use a revenue account with a default program to test the account
	// default branch.
	feeAcct := mkAcctDefProg(t, e.s, "revenue", "Program Fees", []int64{e.subUS}, e.educ)

	// Split omits program: revenue with account default -> educ; salaries -> root.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 100, Position: 0},
			{AccountID: feeAcct, Amount: -100, Position: 1},
		},
	}
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	for _, sp := range txnSplits(t, e.d, id) {
		if sp.AccountID == feeAcct {
			if !sp.ProgramID.Valid || sp.ProgramID.Int64 != e.educ {
				t.Errorf("fee split program = %v, want educ %d (account default)", sp.ProgramID, e.educ)
			}
		}
	}

	// A salaries expense with no default program (root) defaults to root.
	in2 := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 100, Position: 0},
			{AccountID: e.checking, Amount: -100, Position: 1},
		},
	}
	id2, err := e.s.PostTransaction(mutCtx(), in2)
	if err != nil {
		t.Fatalf("PostTransaction 2: %v", err)
	}
	for _, sp := range txnSplits(t, e.d, id2) {
		if sp.AccountID == e.salaries {
			if !sp.ProgramID.Valid || sp.ProgramID.Int64 != rootProgramID {
				t.Errorf("salaries split program = %v, want root %d", sp.ProgramID, rootProgramID)
			}
		}
	}
}

func TestPostProgramOnBalanceSheetRejected(t *testing.T) {
	e := newTxnEnv(t)
	in := e.balancedInput(100)
	in.Splits[1].ProgramID = &e.educ // checking is an asset -> must have no program
	e.assertRejected(t, in, ErrProgramOnBalanceSheet)
}

func TestPostInactiveProgramRejected(t *testing.T) {
	e := newTxnEnv(t)
	// Deactivate educ, then post an R/E split explicitly tagged educ.
	if err := e.s.DeactivateProgram(mutCtx(), e.educ); err != nil {
		t.Fatalf("DeactivateProgram: %v", err)
	}
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 100, ProgramID: &e.educ, Position: 0},
			{AccountID: e.checking, Amount: -100, Position: 1},
		},
	}
	e.assertRejected(t, in, ErrInactiveProgram)
}

func TestPostFundProgramScope(t *testing.T) {
	e := newTxnEnv(t)
	// Fund scoped to program subtree educ. An R/E split tagged the fund with a
	// program OUTSIDE educ's subtree (root) is rejected.
	fund := mkFund(t, e.s, "Educ Grant", []int64{e.subUS}, &e.educ)
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 100, FundID: &fund, ProgramID: &rootProgramMarker, Position: 0},
			{AccountID: e.checking, Amount: -100, FundID: &fund, Position: 1},
		},
	}
	e.assertRejected(t, in, ErrFundProgramScope)
	// Tagging the program INSIDE educ posts fine.
	in.Splits[0].ProgramID = &e.educ
	if _, err := e.s.PostTransaction(mutCtx(), in); err != nil {
		t.Fatalf("post with in-scope program: %v", err)
	}
}

// --- Update: replace-set diff --------------------------------------------

func TestUpdateDiffsSplits(t *testing.T) {
	e := newTxnEnv(t)
	// Post a balanced 3-split txn: two expense debits (fund nil) + one credit.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 600, Position: 0},
			{AccountID: e.salaries, Amount: 400, Position: 1},
			{AccountID: e.checking, Amount: -1_000, Position: 2},
		},
	}
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live := txnSplits(t, e.d, id)
	if len(live) != 3 {
		t.Fatalf("want 3 splits")
	}
	// Identify by account+amount: split0 (600), split1 (400), split2 (-1000).
	var s0, s1, s2 int64
	for _, sp := range live {
		switch {
		case sp.AccountID == e.salaries && sp.Amount == 600:
			s0 = sp.ID
		case sp.AccountID == e.salaries && sp.Amount == 400:
			s1 = sp.ID
		case sp.AccountID == e.checking:
			s2 = sp.ID
		}
	}
	// Each has exactly 1 version (the create).
	for _, sid := range []int64{s0, s1, s2} {
		if c := splitVersionCount(t, e.d, sid); c != 1 {
			t.Fatalf("split %d version count = %d, want 1 after create", sid, c)
		}
	}

	// Update: change s0's amount (600->500), leave s1 UNTOUCHED, remove s2 and add a
	// new credit split, and rebalance (500+400 = 900 debit; -900 credit).
	upd := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &s0, AccountID: e.salaries, Amount: 500, Position: 0}, // changed
			{ID: &s1, AccountID: e.salaries, Amount: 400, Position: 1}, // identical
			{AccountID: e.credit, Amount: -900, Position: 2},           // new (s2 removed)
		},
	}
	if err := e.s.UpdateTransaction(mutCtx(), id, upd); err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}

	// s0 changed -> 2 versions (create + update). s1 untouched -> still 1.
	if c := splitVersionCount(t, e.d, s0); c != 2 {
		t.Errorf("s0 versions = %d, want 2 (create+update)", c)
	}
	if c := splitVersionCount(t, e.d, s1); c != 1 {
		t.Errorf("s1 (untouched) versions = %d, want 1 (only create)", c)
	}
	// s2 removed -> 2 versions (create + delete), latest op=delete; live gone.
	if c := splitVersionCount(t, e.d, s2); c != 2 {
		t.Errorf("s2 versions = %d, want 2 (create+delete)", c)
	}
	testutil.AssertVersioned(t, e.d, "splits", s2, "delete")
	// The new split exists with a single create version.
	var newSplit int64
	for _, sp := range txnSplits(t, e.d, id) {
		if sp.AccountID == e.credit {
			newSplit = sp.ID
		}
	}
	if newSplit == 0 {
		t.Fatal("new credit split not found live")
	}
	if c := splitVersionCount(t, e.d, newSplit); c != 1 {
		t.Errorf("new split versions = %d, want 1 (create)", c)
	}
	// The txn header got an op=update version (anchors the edit).
	testutil.AssertVersioned(t, e.d, "transactions", id, "update")
}

// TestUpdateDuplicateExistingSplitIDRejected proves the worked audit case: an
// UpdateTransaction input carrying the SAME existing split id twice (with different
// amounts) is REJECTED with ErrDuplicateSplitID and leaves NO audit trace. Both
// copies pass the overall zero-sum check together, but without the guard the update
// loop applies UpdateSplit twice to the one live row (last-write-wins), persisting an
// UNBALANCED set that the zero-sum check could not have caught.
func TestUpdateDuplicateExistingSplitIDRejected(t *testing.T) {
	e := newTxnEnv(t)
	// Post a balanced 3-split txn: two salaries debits (100 + 300) + one -400 credit.
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 100, Position: 0},
			{AccountID: e.salaries, Amount: 300, Position: 1},
			{AccountID: e.checking, Amount: -400, Position: 2},
		},
	}
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live := txnSplits(t, e.d, id)
	if len(live) != 3 {
		t.Fatalf("want 3 splits")
	}
	// Pick the first salaries split (id "5" in the worked case) and the credit split.
	var dupID, creditID int64
	for _, sp := range live {
		switch {
		case sp.AccountID == e.salaries && sp.Amount == 100:
			dupID = sp.ID
		case sp.AccountID == e.checking:
			creditID = sp.ID
		}
	}
	if dupID == 0 || creditID == 0 {
		t.Fatalf("could not identify splits (dup=%d credit=%d)", dupID, creditID)
	}

	// The worked case: [{id:dup,amt:100},{id:dup,amt:300},{id:credit,amt:-400}].
	// The two dup copies sum to 400 so the input balances OVERALL, but they name one
	// live row twice.
	upd := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &dupID, AccountID: e.salaries, Amount: 100, Position: 0},
			{ID: &dupID, AccountID: e.salaries, Amount: 300, Position: 1},
			{ID: &creditID, AccountID: e.checking, Amount: -400, Position: 2},
		},
	}
	before := countChanges(t, e.d)
	err = e.s.UpdateTransaction(mutCtx(), id, upd)
	if !errors.Is(err, ErrDuplicateSplitID) {
		t.Fatalf("err = %v, want ErrDuplicateSplitID", err)
	}
	// No audit trace: the change count is unchanged (the rejected update rolled back).
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected update leaves no trace)", n, before)
	}
	// The live splits are untouched: still the original 100 / 300 / -400 set.
	after := txnSplits(t, e.d, id)
	if len(after) != 3 {
		t.Fatalf("live splits = %d, want 3 (unchanged)", len(after))
	}
	var sum int64
	for _, sp := range after {
		sum += sp.Amount
	}
	if sum != 0 {
		t.Errorf("live splits sum = %d, want 0 (still balanced)", sum)
	}
	// The dup row still holds its ORIGINAL amount (last-write-wins never happened).
	for _, sp := range after {
		if sp.ID == dupID && sp.Amount != 100 {
			t.Errorf("dup split amount = %d, want 100 (unchanged)", sp.Amount)
		}
	}
}

// --- Update: a pre-existing split may keep a now-inactive account (p26.13) --

// TestUpdateKeepsInactiveAccountOnUnchangedSplit posts a balanced txn, deactivates
// the account of one of its splits, then updates ONLY a non-account field (memo).
// Because that split's account is unchanged, the now-inactive account is tolerated
// and the update succeeds (and is versioned). Editing a NEW split, or a split whose
// account CHANGED, still requires an ACTIVE account.
func TestUpdateKeepsInactiveAccountOnUnchangedSplit(t *testing.T) {
	e := newTxnEnv(t)
	id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(100))
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live := txnSplits(t, e.d, id)
	if len(live) != 2 {
		t.Fatalf("want 2 splits, got %d", len(live))
	}
	var salariesSplit, checkingSplit int64
	for _, sp := range live {
		switch sp.AccountID {
		case e.salaries:
			salariesSplit = sp.ID
		case e.checking:
			checkingSplit = sp.ID
		}
	}

	// Deactivate the account referenced by BOTH splits' partner: deactivate checking.
	if err := e.s.DeactivateAccount(mutCtx(), e.checking); err != nil {
		t.Fatalf("DeactivateAccount(checking): %v", err)
	}

	// Update ONLY the memo; keep the same split accounts (checking stays inactive).
	upd := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD", Memo: "edited",
		Splits: []SplitInput{
			{ID: &salariesSplit, AccountID: e.salaries, Amount: 100, Position: 0},
			{ID: &checkingSplit, AccountID: e.checking, Amount: -100, Position: 1},
		},
	}
	if err := e.s.UpdateTransaction(mutCtx(), id, upd); err != nil {
		t.Fatalf("UpdateTransaction (memo only, inactive account unchanged) = %v, want nil", err)
	}
	testutil.AssertVersioned(t, e.d, "transactions", id, "update")

	var memo string
	if err := e.d.QueryRow(`SELECT memo FROM transactions WHERE id = ?`, id).Scan(&memo); err != nil {
		t.Fatalf("read memo: %v", err)
	}
	if memo != "edited" {
		t.Errorf("memo = %q, want %q", memo, "edited")
	}
}

// TestUpdateChangeToInactiveAccountRejected: changing an existing split's account to
// a DIFFERENT (also inactive) account still requires an active account -> rejected.
func TestUpdateChangeToInactiveAccountRejected(t *testing.T) {
	e := newTxnEnv(t)
	// A second checking-like asset leaf, mapped to the sub, then deactivated.
	other := mkAcct(t, e.s, "asset", "Savings", []int64{e.subUS}, nil, nil)
	id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(100))
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live := txnSplits(t, e.d, id)
	var salariesSplit, checkingSplit int64
	for _, sp := range live {
		switch sp.AccountID {
		case e.salaries:
			salariesSplit = sp.ID
		case e.checking:
			checkingSplit = sp.ID
		}
	}
	if err := e.s.DeactivateAccount(mutCtx(), other); err != nil {
		t.Fatalf("DeactivateAccount(other): %v", err)
	}

	before := countChanges(t, e.d)
	upd := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &salariesSplit, AccountID: e.salaries, Amount: 100, Position: 0},
			{ID: &checkingSplit, AccountID: other, Amount: -100, Position: 1}, // account CHANGED to inactive
		},
	}
	if err := e.s.UpdateTransaction(mutCtx(), id, upd); !errors.Is(err, ErrInactiveAccount) {
		t.Fatalf("UpdateTransaction (account changed to inactive) = %v, want ErrInactiveAccount", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected update leaves no trace)", n, before)
	}
}

// TestUpdateNewSplitOnInactiveAccountRejected: adding a brand-new split on an
// inactive account still requires an active account -> rejected.
func TestUpdateNewSplitOnInactiveAccountRejected(t *testing.T) {
	e := newTxnEnv(t)
	other := mkAcct(t, e.s, "asset", "Savings", []int64{e.subUS}, nil, nil)
	id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(100))
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live := txnSplits(t, e.d, id)
	var salariesSplit, checkingSplit int64
	for _, sp := range live {
		switch sp.AccountID {
		case e.salaries:
			salariesSplit = sp.ID
		case e.checking:
			checkingSplit = sp.ID
		}
	}
	if err := e.s.DeactivateAccount(mutCtx(), other); err != nil {
		t.Fatalf("DeactivateAccount(other): %v", err)
	}

	before := countChanges(t, e.d)
	// Keep both original splits (balanced 100/-100) and add a new zero-net pair on the
	// inactive account, so the ONLY reason to reject is the inactive account, not balance.
	upd := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &salariesSplit, AccountID: e.salaries, Amount: 100, Position: 0},
			{ID: &checkingSplit, AccountID: e.checking, Amount: -100, Position: 1},
			{AccountID: other, Amount: 50, Position: 2}, // NEW split on inactive account
			{AccountID: e.equity, Amount: -50, Position: 3},
		},
	}
	if err := e.s.UpdateTransaction(mutCtx(), id, upd); !errors.Is(err, ErrInactiveAccount) {
		t.Fatalf("UpdateTransaction (new split on inactive account) = %v, want ErrInactiveAccount", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected update leaves no trace)", n, before)
	}
}

// --- Delete: soft ---------------------------------------------------------

func TestDeleteIsSoft(t *testing.T) {
	e := newTxnEnv(t)
	id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(100))
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	sps := txnSplits(t, e.d, id)
	preCounts := map[int64]int{}
	for _, sp := range sps {
		preCounts[sp.ID] = splitVersionCount(t, e.d, sp.ID)
	}

	if err := e.s.DeleteTransaction(mutCtx(), id); err != nil {
		t.Fatalf("DeleteTransaction: %v", err)
	}
	// Header: deleted=1 + op=delete version.
	var deleted int64
	if err := e.d.QueryRow(`SELECT deleted FROM transactions WHERE id = ?`, id).Scan(&deleted); err != nil {
		t.Fatalf("read deleted: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (soft delete)", deleted)
	}
	testutil.AssertVersioned(t, e.d, "transactions", id, "delete")
	// Splits: untouched, no delete-version.
	for _, sp := range sps {
		if c := splitVersionCount(t, e.d, sp.ID); c != preCounts[sp.ID] {
			t.Errorf("split %d version count changed on soft-delete: %d -> %d", sp.ID, preCounts[sp.ID], c)
		}
	}
}

// --- As-of reconstruction -------------------------------------------------

func TestTransactionAsOf(t *testing.T) {
	// Deterministic increasing clock: post @T0, edit1 @T1, edit2 @T2. Setup calls
	// (subs/accounts) run at the base time; the phase timestamps are set before each
	// post/edit below. nowFn is initialized non-nil BEFORE any store call.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return base }
	d := testutil.NewDB(t)
	s := New(d, WithClock(func() time.Time { return nowFn() }))
	subUS := newSub(t, s, rootID, "US")
	educ, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Educacion"})
	if err != nil {
		t.Fatalf("CreateProgram: %v", err)
	}
	fundA := mkFund(t, s, "Fund A", []int64{subUS}, nil)
	checking := mkAcct(t, s, "asset", "Checking", []int64{subUS}, nil, nil)
	// Expense with default class management, default program root.
	salaries := mkAcctFull(t, s, "expense", "Salaries", []int64{subUS}, "management", rootProgramID)

	t0 := base
	t1 := base.Add(1 * time.Hour)
	t2 := base.Add(2 * time.Hour)

	nowFn = func() time.Time { return t0 }
	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: salaries, Amount: 1_000, Position: 0},
			{AccountID: checking, Amount: -1_000, Position: 1},
		},
	}
	id, err := s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live := txnSplits(t, d, id)
	var salSplit, chkSplit int64
	for _, sp := range live {
		if sp.AccountID == salaries {
			salSplit = sp.ID
		} else {
			chkSplit = sp.ID
		}
	}

	// edit1 @T1: change the salaries split -> program educ, fund fundA, amount 1200,
	// and rebalance the checking side to -1200 (so it also changes).
	nowFn = func() time.Time { return t1 }
	edit1 := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &salSplit, AccountID: salaries, Amount: 1_200, FundID: &fundA, ProgramID: &educ, Position: 0},
			{ID: &chkSplit, AccountID: checking, Amount: -1_200, FundID: &fundA, Position: 1},
		},
	}
	if err := s.UpdateTransaction(mutCtx(), id, edit1); err != nil {
		t.Fatalf("edit1: %v", err)
	}

	// edit2 @T2: change amounts back to 900 (different middle-state).
	nowFn = func() time.Time { return t2 }
	edit2 := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &salSplit, AccountID: salaries, Amount: 900, FundID: &fundA, ProgramID: &educ, Position: 0},
			{ID: &chkSplit, AccountID: checking, Amount: -900, FundID: &fundA, Position: 1},
		},
	}
	if err := s.UpdateTransaction(mutCtx(), id, edit2); err != nil {
		t.Fatalf("edit2: %v", err)
	}

	// As of a T strictly between edit1 and edit2: the MIDDLE state (edit1's values).
	mid := base.Add(90 * time.Minute)
	st, err := s.TransactionAsOf(context.Background(), id, mid)
	if err != nil {
		t.Fatalf("TransactionAsOf: %v", err)
	}
	if !st.Present {
		t.Fatal("txn not present at mid, want present")
	}
	if len(st.Splits) != 2 {
		t.Fatalf("mid split set size = %d, want 2", len(st.Splits))
	}
	// Assert the SET membership AND each split's values at edit1.
	byID := map[int64]SplitState{}
	for _, sp := range st.Splits {
		byID[sp.ID] = sp
	}
	sal := byID[salSplit]
	if sal.Amount != 1_200 {
		t.Errorf("mid salaries amount = %d, want 1200 (edit1)", sal.Amount)
	}
	if !sal.ProgramID.Valid || sal.ProgramID.Int64 != educ {
		t.Errorf("mid salaries program = %v, want educ %d", sal.ProgramID, educ)
	}
	if !sal.FundID.Valid || sal.FundID.Int64 != fundA {
		t.Errorf("mid salaries fund = %v, want fundA %d", sal.FundID, fundA)
	}
	chk := byID[chkSplit]
	if chk.Amount != -1_200 {
		t.Errorf("mid checking amount = %d, want -1200 (edit1)", chk.Amount)
	}

	// As of before the post: not present.
	st0, err := s.TransactionAsOf(context.Background(), id, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("TransactionAsOf(before): %v", err)
	}
	if st0.Present {
		t.Error("txn present before post, want absent")
	}
}

// --- Concurrency ----------------------------------------------------------

func TestConcurrentPostsSerialize(t *testing.T) {
	e := newTxnEnv(t)
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier: release all together for genuine overlap
			id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(int64(100+i)))
			ids[i] = id
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("poster %d failed: %v", i, errs[i])
		}
		if ids[i] == 0 {
			t.Fatalf("poster %d got id 0", i)
		}
	}
	// Distinct change ids: each post is its own change. Read the change ids of each
	// txn's create version.
	for i := 0; i < n; i++ {
		var cid int64
		if err := e.d.QueryRow(`SELECT change_id FROM transactions_versions WHERE entity_id = ? AND op = 'create'`, ids[i]).Scan(&cid); err != nil {
			t.Fatalf("read change for txn %d: %v", ids[i], err)
		}
		if seen[cid] {
			t.Errorf("duplicate change id %d across concurrent posts", cid)
		}
		seen[cid] = true
	}
}

// --- fixture helpers ------------------------------------------------------

// mkFund creates a fund scoped to subs, optionally to a program subtree.
func mkFund(t *testing.T, s *Store, name string, subs []int64, prog *int64) int64 {
	t.Helper()
	in := CreateFundInput{Name: name, Restriction: "purpose", Subsidiaries: subs}
	if prog != nil {
		p := *prog
		in.ProgramID = &p
	}
	id, err := s.CreateFund(mutCtx(), in)
	if err != nil {
		t.Fatalf("CreateFund(%s): %v", name, err)
	}
	return id
}

// mkAcctDefProg creates a leaf account with a default program (no functional class).
func mkAcctDefProg(t *testing.T, s *Store, typ, name string, subs []int64, defProg int64) int64 {
	t.Helper()
	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: typ, DefaultCurrency: "USD", Names: enName(name), Subsidiaries: subs,
		DefaultProgramID: &defProg,
	})
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", name, err)
	}
	return id
}

// TestAccountMissingRejectedEveryPath proves the double-entry invariant "a split MUST
// name an account" is enforced on EVERY server entry path (p26.10 / AGENTS rule 7): a
// split with AccountID 0 is rejected with ErrAccountMissing (GetAccount(0) -> no rows)
// on PostTransaction, UpdateTransaction, PostImportRow, and PostAndConvertExpenseReport
// -- all of which funnel through validateAndResolve/resolveSplit. The web layer maps
// this to a visible per-row error; here we prove the store never lets an accountless
// split through, so the display fix cannot become a data-integrity hole.
func TestAccountMissingRejectedEveryPath(t *testing.T) {
	// Post + Update share the txnEnv chart.
	e := newTxnEnv(t)

	postIn := e.balancedInput(10_000)
	postIn.Splits[0].AccountID = 0 // content-bearing split with no account
	if _, err := e.s.PostTransaction(mutCtx(), postIn); !errors.Is(err, ErrAccountMissing) {
		t.Fatalf("PostTransaction(account 0) = %v, want ErrAccountMissing", err)
	}

	// A valid txn to then update with an accountless split.
	id, err := e.s.PostTransaction(mutCtx(), e.balancedInput(10_000))
	if err != nil {
		t.Fatalf("seed txn for update: %v", err)
	}
	updIn := e.balancedInput(10_000)
	updIn.Splits[1].AccountID = 0
	if err := e.s.UpdateTransaction(mutCtx(), id, updIn); !errors.Is(err, ErrAccountMissing) {
		t.Fatalf("UpdateTransaction(account 0) = %v, want ErrAccountMissing", err)
	}

	// PostImportRow.
	env := newImportEnv(t)
	rowID := stageOnePending(t, env, "2025-01-15", 10000, "Acme", "Invoice")
	importIn := PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, Position: 0},
			{AccountID: 0, Amount: -10000, Position: 1}, // no account
		},
	}
	if _, err := env.s.PostImportRow(mutCtx(), rowID, importIn); !errors.Is(err, ErrAccountMissing) {
		t.Fatalf("PostImportRow(account 0) = %v, want ErrAccountMissing", err)
	}

	// PostAndConvertExpenseReport.
	s, _, ctx, submitterID, expenseAcct := seedExpenseReportEnv(t)
	reportID, err := s.CreateExpenseReport(ctx, submitterID, 1)
	if err != nil {
		t.Fatalf("CreateExpenseReport: %v", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: expenseAcct, Amount: 2000, Memo: "taxi"}); err != nil {
		t.Fatalf("AddExpenseReportLine: %v", err)
	}
	if err := s.SubmitExpenseReport(ctx, reportID); err != nil {
		t.Fatalf("SubmitExpenseReport: %v", err)
	}
	prog := int64(1)
	fc := "program"
	expIn := PostTransactionInput{
		Date: "2025-06-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: expenseAcct, Amount: 2000, Position: 0, ProgramID: &prog, FunctionalClass: &fc},
			{AccountID: 0, Amount: -2000, Position: 1}, // no account
		},
	}
	if _, err := s.PostAndConvertExpenseReport(ctx, reportID, expIn); !errors.Is(err, ErrAccountMissing) {
		t.Fatalf("PostAndConvertExpenseReport(account 0) = %v, want ErrAccountMissing", err)
	}
}

// mkAcctFull creates a leaf expense account with an explicit default functional
// class and default program.
func mkAcctFull(t *testing.T, s *Store, typ, name string, subs []int64, fClass string, defProg int64) int64 {
	t.Helper()
	id, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: typ, DefaultCurrency: "USD", Names: enName(name), Subsidiaries: subs,
		FunctionalClass: &fClass, DefaultProgramID: &defProg,
	})
	if err != nil {
		t.Fatalf("CreateAccount(%s): %v", name, err)
	}
	return id
}

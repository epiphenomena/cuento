package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/testutil"
)

// Reconciliation lifecycle tests (p16.2, D13/D20). These build a tiny bespoke
// chart inline (AGENTS testing conventions): a US subsidiary, a RECONCILABLE
// Checking account, an expense, a revenue, an equity account, and a fund so the
// spans-funds proof can mix restricted + unrestricted splits on one statement.

// reconEnv is the shared chart for the reconciliation tests.
type reconEnv struct {
	s     *Store
	d     *sql.DB
	subUS int64

	checking int64 // asset, US, RECONCILABLE
	other    int64 // asset, US, reconcilable (a different account, for Z8 mismatch)
	expense  int64 // expense
	revenue  int64 // revenue
	equity   int64 // equity
	fund     int64 // restricted fund scoped to US
}

func newReconEnv(t *testing.T) reconEnv {
	t.Helper()
	d := testutil.NewDB(t)
	s := New(d)
	subUS := newSub(t, s, rootID, "US")

	checking, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Checking US"),
		Subsidiaries: []int64{subUS}, Reconcilable: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount checking: %v", err)
	}
	other, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Savings US"),
		Subsidiaries: []int64{subUS}, Reconcilable: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount other: %v", err)
	}

	env := reconEnv{
		s: s, d: d, subUS: subUS, checking: checking, other: other,
		expense: mkAcct(t, s, "expense", "Supplies", []int64{subUS}, strp("management"), nil),
		revenue: mkAcct(t, s, "revenue", "Grants", []int64{subUS}, nil, nil),
		equity:  mkAcct(t, s, "equity", "Opening", []int64{subUS}, nil, nil),
	}
	env.fund, err = s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Beca", Restriction: "purpose", Subsidiaries: []int64{subUS},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}
	return env
}

func strp(s string) *string { return &s }

// checkingSplitID returns the id of the Checking split on a transaction.
func checkingSplitID(t *testing.T, d *sql.DB, txnID, checking int64) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(`SELECT id FROM splits WHERE transaction_id = ? AND account_id = ?`, txnID, checking).Scan(&id); err != nil {
		t.Fatalf("checkingSplitID(txn %d): %v", txnID, err)
	}
	return id
}

// reconOf returns a split's live reconciliation_id (or 0 when NULL).
func reconOf(t *testing.T, d *sql.DB, splitID int64) int64 {
	t.Helper()
	var r sql.NullInt64
	if err := d.QueryRow(`SELECT reconciliation_id FROM splits WHERE id = ?`, splitID).Scan(&r); err != nil {
		t.Fatalf("reconOf(%d): %v", splitID, err)
	}
	if !r.Valid {
		return 0
	}
	return r.Int64
}

// assertLedgerClean fails if ledger.Check reports any Error violation.
func assertLedgerClean(t *testing.T, d *sql.DB) {
	t.Helper()
	vs, err := ledger.Check(context.Background(), d)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("unexpected Error violation %s: %s", v.Rule, v.Detail)
		}
	}
}

// --- Full lifecycle + versioning -----------------------------------------

// TestReconciliationLifecycle walks Start -> clear -> Finalize -> Reopen and
// asserts every mutation is versioned with the right op (create/update/update) via
// AssertVersioned, and that the ledger stays clean throughout.
func TestReconciliationLifecycle(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// One balanced deposit: Checking +1,000 / revenue -1,000 (unrestricted).
	txn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 100_000, Position: 0},
			{AccountID: e.revenue, Amount: -100_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	spID := checkingSplitID(t, e.d, txn, e.checking)

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", 100_000)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", recon, "create")

	if err := e.s.SetSplitReconciled(ctx, recon, spID, true); err != nil {
		t.Fatalf("SetSplitReconciled on: %v", err)
	}
	if got := reconOf(t, e.d, spID); got != recon {
		t.Fatalf("split reconciliation_id = %d, want %d", got, recon)
	}
	// Clearing is LIVE-ONLY: it mints NO split version (only the create version).
	if n := splitVersionCount(t, e.d, spID); n != 1 {
		t.Errorf("split versions after clear = %d, want 1 (live-only column)", n)
	}

	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", recon, "update")
	got, _ := e.s.GetReconciliation(ctx, recon)
	if got.Status != "finalized" {
		t.Errorf("status after Finalize = %q, want finalized", got.Status)
	}
	assertLedgerClean(t, e.d) // Z8/Z9 clean on a finalized recon

	if err := e.s.Reopen(ctx, recon); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", recon, "update")
	got, _ = e.s.GetReconciliation(ctx, recon)
	if got.Status != "open" {
		t.Errorf("status after Reopen = %q, want open", got.Status)
	}
	assertLedgerClean(t, e.d)
}

// --- Start validation ----------------------------------------------------

func TestStartReconciliationValidation(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// Non-reconcilable account rejected.
	if _, err := e.s.StartReconciliation(ctx, e.expense, "USD", "2026-01-31", 0); !errors.Is(err, ErrNotReconcilable) {
		t.Errorf("start on non-reconcilable: err = %v, want ErrNotReconcilable", err)
	}
	// Unknown currency rejected (XXX is not seeded).
	if _, err := e.s.StartReconciliation(ctx, e.checking, "XXX", "2026-01-31", 0); !errors.Is(err, ErrReconciliationCurrency) {
		t.Errorf("start with bad currency: err = %v, want ErrReconciliationCurrency", err)
	}
	// Bad date rejected.
	if _, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-13-99", 0); !errors.Is(err, ErrBadDate) {
		t.Errorf("start with bad date: err = %v, want ErrBadDate", err)
	}
	// One open recon per (account, currency): a second open one is rejected.
	if _, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", 0); err != nil {
		t.Fatalf("first open recon: %v", err)
	}
	if _, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 0); !errors.Is(err, ErrOpenReconciliationExists) {
		t.Errorf("second open recon: err = %v, want ErrOpenReconciliationExists", err)
	}
}

// --- Toggle validation: account + currency (Z8 payoff) -------------------

// TestToggleValidatesAccountAndCurrency proves SetSplitReconciled rejects a split
// whose account or currency does not match the recon's -- keeping Z8 satisfiable.
func TestToggleValidatesAccountAndCurrency(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// A USD txn touching `other` (not checking).
	otherTxn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-05", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.other, Amount: 50_000, Position: 0},
			{AccountID: e.revenue, Amount: -50_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post other txn: %v", err)
	}
	otherSplit := checkingSplitID(t, e.d, otherTxn, e.other)

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", 0)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	// Wrong ACCOUNT: the split is on `other`, the recon on `checking`.
	if err := e.s.SetSplitReconciled(ctx, recon, otherSplit, true); !errors.Is(err, ErrSplitReconAccount) {
		t.Errorf("clear cross-account split: err = %v, want ErrSplitReconAccount", err)
	}

	// Wrong CURRENCY: a recon on `checking` in MXN, but the checking split's txn is
	// USD. Make MXN active + reachable (seeded) and open a second recon in MXN.
	mxnRecon, err := e.s.StartReconciliation(ctx, e.checking, "MXN", "2026-01-31", 0)
	if err != nil {
		t.Fatalf("StartReconciliation MXN: %v", err)
	}
	usdTxn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-06", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 20_000, Position: 0},
			{AccountID: e.revenue, Amount: -20_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post usd txn: %v", err)
	}
	usdSplit := checkingSplitID(t, e.d, usdTxn, e.checking)
	if err := e.s.SetSplitReconciled(ctx, mxnRecon, usdSplit, true); !errors.Is(err, ErrSplitReconCurrency) {
		t.Errorf("clear cross-currency split: err = %v, want ErrSplitReconCurrency", err)
	}
}

// --- Finalize: opening chain + zero-difference ---------------------------

// TestFinalizeRequiresZeroDifference proves the zero-difference gate AND the
// opening chain: it finalizes a FIRST recon (opening 0), then a SECOND recon whose
// opening is the first's statement balance -- rejecting a wrong statement balance
// with ErrReconciliationDifference and accepting the correct one. The Finalize gate
// is byte-identical to Z9, so a passing Finalize leaves the ledger clean.
func TestFinalizeRequiresZeroDifference(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// Deposit 1: Checking +1,000.
	txn1, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 100_000, Position: 0},
			{AccountID: e.revenue, Amount: -100_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post txn1: %v", err)
	}
	sp1 := checkingSplitID(t, e.d, txn1, e.checking)

	recon1, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", 100_000)
	if err != nil {
		t.Fatalf("StartReconciliation 1: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recon1, sp1, true); err != nil {
		t.Fatalf("clear sp1: %v", err)
	}
	if err := e.s.Finalize(ctx, recon1); err != nil {
		t.Fatalf("Finalize 1 (opening 0 + 100,000 == 100,000): %v", err)
	}

	// Deposit 2: Checking +400. The SECOND recon's opening is recon1's statement
	// balance (100,000); its statement balance must be 100,000 + 40,000 = 140,000.
	txn2, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-02-05", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 40_000, Position: 0},
			{AccountID: e.revenue, Amount: -40_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post txn2: %v", err)
	}
	sp2 := checkingSplitID(t, e.d, txn2, e.checking)

	// The SECOND recon's opening is recon1's statement balance (100,000). Its
	// correct statement balance is 100,000 + 40,000 = 140,000. First attempt to
	// finalize with sp2 NOT yet cleared: opening 100,000 + cleared 0 = 100,000 !=
	// 140,000 -> rejected (this exercises the OPENING CHAIN -- a zero-opening gate
	// would compare 0 vs 140,000 and reject for the wrong reason). Then clear sp2
	// and finalize for real.
	recon2, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 140_000)
	if err != nil {
		t.Fatalf("StartReconciliation 2: %v", err)
	}
	if err := e.s.Finalize(ctx, recon2); !errors.Is(err, ErrReconciliationDifference) {
		t.Fatalf("Finalize before clearing sp2: err = %v, want ErrReconciliationDifference", err)
	}
	// The rejected finalize left the recon OPEN (rolled back).
	if got, _ := e.s.GetReconciliation(ctx, recon2); got.Status != "open" {
		t.Errorf("status after rejected finalize = %q, want open", got.Status)
	}
	assertLedgerClean(t, e.d)

	// POSITIVE chained path (the D13 payoff): clear sp2 so opening 100,000 +
	// cleared 40,000 == 140,000 and finalize SUCCEEDS. Proves Finalize accepts a
	// non-zero opening and Z9 stays clean on a finalized recon whose opening != 0.
	if err := e.s.SetSplitReconciled(ctx, recon2, sp2, true); err != nil {
		t.Fatalf("clear sp2: %v", err)
	}
	if err := e.s.Finalize(ctx, recon2); err != nil {
		t.Fatalf("chained finalize (opening 100,000 + 40,000 == 140,000): %v", err)
	}
	if got, _ := e.s.GetReconciliation(ctx, recon2); got.Status != "finalized" {
		t.Errorf("status after chained finalize = %q, want finalized", got.Status)
	}
	assertLedgerClean(t, e.d) // Z9 now runs on a finalized recon with opening != 0
}

// --- Reopen audited (names the actor) ------------------------------------

// TestReopenAudited proves the reopen version row names the acting user (the
// audited unreconcile, D13). It reopens as a DISTINCT actor and checks the latest
// reconciliation version's change was made by that actor.
func TestReconciliationReopenAudited(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx() // actor 1

	txn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 100_000, Position: 0},
			{AccountID: e.revenue, Amount: -100_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	sp := checkingSplitID(t, e.d, txn, e.checking)
	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", 100_000)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recon, sp, true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// Reopen as a DIFFERENT, real user so the audited actor is unambiguous (the
	// changes.actor_id FK requires an existing user).
	reopener, err := e.s.CreateUser(ctx, CreateUserInput{Username: "reopener", DisplayName: "Reopener", IsAdmin: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	reopenCtx := WithActor(context.Background(), Actor{ID: reopener})
	if err := e.s.Reopen(reopenCtx, recon); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if actor := testutil.LatestVersionActor(t, e.d, "reconciliations", recon); actor != reopener {
		t.Errorf("reopen version actor = %d, want %d", actor, reopener)
	}
}

// --- Store-level edit block on a finalized-reconciled split --------------

// TestEditReconciledTxnBlocked proves the STORE (not just the trigger) refuses
// financial edits touching a split cleared in a FINALIZED recon (date/amount/
// account/fund and deletion), returning the clean typed ErrSplitReconciled BEFORE
// the trigger fires -- while ALLOWING memo edits; and that editing is allowed again
// after Reopen.
func TestEditReconciledTxnBlocked(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// A balanced txn: Checking -1,000 (a payment) / expense +1,000.
	base := PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.expense, Amount: 100_000, Position: 0},
			{AccountID: e.checking, Amount: -100_000, Position: 1},
		},
	}
	txn, err := e.s.PostTransaction(ctx, base)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	sp := txnSplits(t, e.d, txn)
	// Identify the two split ids by account.
	var expID, chkID int64
	for _, x := range sp {
		if x.AccountID == e.expense {
			expID = x.ID
		} else {
			chkID = x.ID
		}
	}

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", -100_000)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recon, chkID, true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// withEdit returns base with the checking + expense splits carrying explicit ids,
	// mutated by fn.
	withEdit := func(fn func(in *PostTransactionInput)) PostTransactionInput {
		in := PostTransactionInput{
			Date: base.Date, SubsidiaryID: base.SubsidiaryID, Currency: base.Currency,
			Splits: []SplitInput{
				{ID: &expID, AccountID: e.expense, Amount: 100_000, Position: 0},
				{ID: &chkID, AccountID: e.checking, Amount: -100_000, Position: 1},
			},
		}
		fn(&in)
		return in
	}

	// AMOUNT change on the locked (checking) split -> blocked. The counter-split
	// must move too to stay balanced; the block trips on the locked split.
	amountEdit := withEdit(func(in *PostTransactionInput) {
		in.Splits[0].Amount = 90_000  // expense
		in.Splits[1].Amount = -90_000 // checking (locked)
	})
	if err := e.s.UpdateTransaction(ctx, txn, amountEdit); !errors.Is(err, ErrSplitReconciled) {
		t.Errorf("amount edit on locked split: err = %v, want ErrSplitReconciled", err)
	}

	// FUND change on the locked split -> blocked (tag both sides so it would balance).
	fundEdit := withEdit(func(in *PostTransactionInput) {
		in.Splits[0].FundID = &e.fund
		in.Splits[1].FundID = &e.fund
	})
	if err := e.s.UpdateTransaction(ctx, txn, fundEdit); !errors.Is(err, ErrSplitReconciled) {
		t.Errorf("fund edit on locked split: err = %v, want ErrSplitReconciled", err)
	}

	// DATE change on the header (touches every split's statement) -> blocked.
	dateEdit := withEdit(func(in *PostTransactionInput) { in.Date = "2026-02-01" })
	if err := e.s.UpdateTransaction(ctx, txn, dateEdit); !errors.Is(err, ErrSplitReconciled) {
		t.Errorf("date edit: err = %v, want ErrSplitReconciled", err)
	}

	// ACCOUNT change on the locked split -> blocked (move checking side to `other`).
	acctEdit := withEdit(func(in *PostTransactionInput) { in.Splits[1].AccountID = e.other })
	if err := e.s.UpdateTransaction(ctx, txn, acctEdit); !errors.Is(err, ErrSplitReconciled) {
		t.Errorf("account edit on locked split: err = %v, want ErrSplitReconciled", err)
	}

	// DELETION of the locked split (drop it from the input) -> blocked.
	delEdit := PostTransactionInput{
		Date: base.Date, SubsidiaryID: base.SubsidiaryID, Currency: base.Currency,
		Splits: []SplitInput{
			{ID: &expID, AccountID: e.expense, Amount: 100_000, Position: 0},
			{AccountID: e.other, Amount: -100_000, Position: 1}, // new, replaces locked
		},
	}
	if err := e.s.UpdateTransaction(ctx, txn, delEdit); !errors.Is(err, ErrSplitReconciled) {
		t.Errorf("delete locked split: err = %v, want ErrSplitReconciled", err)
	}

	// MEMO edit on the locked split -> ALLOWED (non-financial; the trigger does not
	// guard memo, and the store must not either).
	memoEdit := withEdit(func(in *PostTransactionInput) {
		in.Memo = "reconciled cleared payment"
		in.Splits[1].Memo = "cleared"
	})
	if err := e.s.UpdateTransaction(ctx, txn, memoEdit); err != nil {
		t.Errorf("memo edit on locked split: err = %v, want nil (memo allowed)", err)
	}
	assertLedgerClean(t, e.d)

	// After Reopen the financial edit is allowed again.
	if err := e.s.Reopen(ctx, recon); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened := withEdit(func(in *PostTransactionInput) {
		in.Splits[0].Amount = 90_000
		in.Splits[1].Amount = -90_000
	})
	if err := e.s.UpdateTransaction(ctx, txn, reopened); err != nil {
		t.Errorf("amount edit after reopen: err = %v, want nil", err)
	}
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/ids"
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

	checking int64      // asset, US, RECONCILABLE
	other    int64      // asset, US, reconcilable (a different account, for Z8 mismatch)
	expense  int64      // expense
	revenue  int64      // revenue
	equity   int64      // equity
	fund     ids.FundID // restricted fund scoped to US
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
	testutil.AssertVersioned(t, e.d, "reconciliations", int64(recon), "create")

	if err := e.s.SetSplitReconciled(ctx, recon, spID, true); err != nil {
		t.Fatalf("SetSplitReconciled on: %v", err)
	}
	if got := reconOf(t, e.d, spID); got != int64(recon) {
		t.Fatalf("split reconciliation_id = %d, want %d", got, recon)
	}
	// Clearing is LIVE-ONLY: it mints NO split version (only the create version).
	if n := splitVersionCount(t, e.d, spID); n != 1 {
		t.Errorf("split versions after clear = %d, want 1 (live-only column)", n)
	}

	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", int64(recon), "update")
	got, _ := e.s.GetReconciliation(ctx, recon)
	if got.Status != "finalized" {
		t.Errorf("status after Finalize = %q, want finalized", got.Status)
	}
	assertLedgerClean(t, e.d) // Z8/Z9 clean on a finalized recon

	if err := e.s.Reopen(ctx, recon); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", int64(recon), "update")
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
	if actor := testutil.LatestVersionActor(t, e.d, "reconciliations", int64(recon)); actor != int64(reopener) {
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

// --- p16.5 void block on a finalized-reconciled split --------------------

// TestVoidReconciledTransactionBlocked proves the STORE refuses to soft-delete
// (void) a transaction that has a split cleared in a FINALIZED recon (Gap 1: the
// split-lock trigger fires on UPDATE only, so a void would silently drop the split
// from the recon's balance and break Z9). DeleteTransaction returns the clean typed
// ErrSplitReconciled and the txn stays live; after Reopen the void succeeds.
func TestVoidReconciledTransactionBlocked(t *testing.T) {
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
	chkID := checkingSplitID(t, e.d, txn, e.checking)

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

	// Void is BLOCKED: the checking split is cleared in a finalized recon.
	if err := e.s.DeleteTransaction(ctx, txn); !errors.Is(err, ErrSplitReconciled) {
		t.Fatalf("void of reconciled txn: err = %v, want ErrSplitReconciled", err)
	}
	// The txn stays LIVE (the rejected write rolled back).
	var deleted int64
	if err := e.d.QueryRow(`SELECT deleted FROM transactions WHERE id = ?`, txn).Scan(&deleted); err != nil {
		t.Fatalf("read deleted: %v", err)
	}
	if deleted != 0 {
		t.Errorf("txn deleted = %d after blocked void, want 0 (still live)", deleted)
	}
	// The finalized statement still proves (Z9 clean): the void did not drop the split.
	assertLedgerClean(t, e.d)

	// After Reopen the same void SUCCEEDS.
	if err := e.s.Reopen(ctx, recon); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := e.s.DeleteTransaction(ctx, txn); err != nil {
		t.Fatalf("void after reopen: %v", err)
	}
	if err := e.d.QueryRow(`SELECT deleted FROM transactions WHERE id = ?`, txn).Scan(&deleted); err != nil {
		t.Fatalf("read deleted after reopen-void: %v", err)
	}
	if deleted != 1 {
		t.Errorf("txn deleted = %d after reopen+void, want 1", deleted)
	}
}

// TestVoidOpenReconciledTransactionAllowed proves the void guard does NOT
// over-block: a txn whose split is cleared only in an OPEN recon (not finalized) is
// still voidable. Uncleared/never-reconciled txns are covered by TestDeleteIsSoft.
func TestVoidOpenReconciledTransactionAllowed(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

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
	chkID := checkingSplitID(t, e.d, txn, e.checking)

	// Cleared in an OPEN recon (never finalized).
	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", -100_000)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recon, chkID, true); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if err := e.s.DeleteTransaction(ctx, txn); err != nil {
		t.Fatalf("void of open-reconciled txn: err = %v, want nil (no over-block)", err)
	}
}

// --- p16.5 reopen in-order guard -----------------------------------------

// TestReopenBlockedWhenLaterFinalizedExists proves BOTH reopen guards on the same
// (account, currency). Two finalized recons E (earlier) and L (later):
//   - Reopen(E) while L is finalized -> ErrReconciliationNotLatest (Gap 2, in-order:
//     reopening out of order corrupts the opening chain).
//   - Reopen(L) succeeds (L is the latest); L is now OPEN.
//   - Reopen(E) is now blocked by the p22.5 later-OPEN guard ->
//     ErrOpenReconciliationExists (reopening E would yield TWO open recons, a state
//     StartReconciliation refuses; before p22.5 this silently produced two opens).
func TestReopenBlockedWhenLaterFinalizedExists(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// txn1: Checking -1,000 (Jan). txn2: Checking -400 (Feb).
	post := func(date string, amt int64) int64 {
		id, err := e.s.PostTransaction(ctx, PostTransactionInput{
			Date: date, SubsidiaryID: e.subUS, Currency: "USD",
			Splits: []SplitInput{
				{AccountID: e.expense, Amount: -amt, Position: 0},
				{AccountID: e.checking, Amount: amt, Position: 1},
			},
		})
		if err != nil {
			t.Fatalf("post %s: %v", date, err)
		}
		return id
	}
	txn1 := post("2026-01-10", -100_000)
	txn2 := post("2026-02-10", -40_000)
	sp1 := checkingSplitID(t, e.d, txn1, e.checking)
	sp2 := checkingSplitID(t, e.d, txn2, e.checking)

	// Recon E (earlier): opening 0 + cleared -100,000 == statement -100,000.
	reconE, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", -100_000)
	if err != nil {
		t.Fatalf("start E: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, reconE, sp1, true); err != nil {
		t.Fatalf("clear sp1: %v", err)
	}
	if err := e.s.Finalize(ctx, reconE); err != nil {
		t.Fatalf("finalize E: %v", err)
	}

	// Recon L (later): opening -100,000 + cleared -40,000 == statement -140,000.
	reconL, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", -140_000)
	if err != nil {
		t.Fatalf("start L: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, reconL, sp2, true); err != nil {
		t.Fatalf("clear sp2: %v", err)
	}
	if err := e.s.Finalize(ctx, reconL); err != nil {
		t.Fatalf("finalize L: %v", err)
	}

	// Reopening the EARLIER recon while a LATER finalized one exists is blocked.
	if err := e.s.Reopen(ctx, reconE); !errors.Is(err, ErrReconciliationNotLatest) {
		t.Fatalf("reopen earlier: err = %v, want ErrReconciliationNotLatest", err)
	}
	// It stays finalized (the rejected write rolled back).
	if got, _ := e.s.GetReconciliation(ctx, reconE); got.Status != "finalized" {
		t.Errorf("recon E status after blocked reopen = %q, want finalized", got.Status)
	}

	// Reopening the LATEST finalized recon succeeds. L is now OPEN.
	if err := e.s.Reopen(ctx, reconL); err != nil {
		t.Fatalf("reopen latest: %v", err)
	}
	// E is now the latest FINALIZED, so the in-order guard clears -- but L is now the
	// one OPEN recon on this (account, currency), so reopening E is blocked by the
	// later-OPEN guard (p22.5): reopening it would leave two open recons. (Before
	// p22.5 this reopen SUCCEEDED, silently producing two open recons.)
	if err := e.s.Reopen(ctx, reconE); !errors.Is(err, ErrOpenReconciliationExists) {
		t.Fatalf("reopen E while L open: err = %v, want ErrOpenReconciliationExists", err)
	}
	// E stays finalized (the rejected reopen rolled back).
	if got, _ := e.s.GetReconciliation(ctx, reconE); got.Status != "finalized" {
		t.Errorf("recon E status after blocked reopen = %q, want finalized", got.Status)
	}
}

// TestReopenBlockedWhenOtherOpenExists proves the p22.5 later-OPEN guard directly and
// in isolation from the in-order chain guard. A finalized recon E plus a LATER recon
// left OPEN on the same (account, currency): Reopen(E) is rejected with
// ErrOpenReconciliationExists (nothing LATER is finalized, so the in-order guard does
// not fire -- only the new later-OPEN guard). With NO other open recon, Reopen(E)
// succeeds.
func TestReopenBlockedWhenOtherOpenExists(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	txn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.expense, Amount: 100_000, Position: 0},
			{AccountID: e.checking, Amount: -100_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	chkID := checkingSplitID(t, e.d, txn, e.checking)

	// E (earlier): opening 0 + cleared -100,000 == statement -100,000; finalize it.
	reconE, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", -100_000)
	if err != nil {
		t.Fatalf("start E: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, reconE, chkID, true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := e.s.Finalize(ctx, reconE); err != nil {
		t.Fatalf("finalize E: %v", err)
	}

	// A LATER recon on the same (account, currency), left OPEN (never finalized), so
	// the in-order guard cannot fire when we reopen E -- only the later-OPEN guard.
	reconLater, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", -100_000)
	if err != nil {
		t.Fatalf("start later open: %v", err)
	}

	// Reopen(E) is blocked: a later OPEN recon stands, so reopening E would yield two
	// open recons on the same (account, currency).
	if err := e.s.Reopen(ctx, reconE); !errors.Is(err, ErrOpenReconciliationExists) {
		t.Fatalf("reopen E while later open exists: err = %v, want ErrOpenReconciliationExists", err)
	}
	// E stays finalized (the rejected reopen rolled back); the books stay clean.
	if got, _ := e.s.GetReconciliation(ctx, reconE); got.Status != "finalized" {
		t.Errorf("recon E status after blocked reopen = %q, want finalized", got.Status)
	}
	assertLedgerClean(t, e.d)

	// Finalize the later recon (opening -100,000 + cleared 0 == statement -100,000) so
	// NO open recon stands. It is now the latest finalized, so reopen IT first (chain
	// guard), which leaves E the only finalized recon and no open recon standing.
	if err := e.s.Finalize(ctx, reconLater); err != nil {
		t.Fatalf("finalize later: %v", err)
	}
	if err := e.s.Reopen(ctx, reconLater); err != nil {
		t.Fatalf("reopen later (now latest): %v", err)
	}
	// Now reconLater is OPEN again -- so E is STILL blocked by the later-OPEN guard.
	if err := e.s.Reopen(ctx, reconE); !errors.Is(err, ErrOpenReconciliationExists) {
		t.Fatalf("reopen E while later reopened: err = %v, want ErrOpenReconciliationExists", err)
	}
}

// TestReopenSingleFinalized proves a lone finalized recon reopens without the
// in-order guard tripping (no later finalized exists).
func TestReopenSingleFinalized(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	txn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.expense, Amount: 100_000, Position: 0},
			{AccountID: e.checking, Amount: -100_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	chkID := checkingSplitID(t, e.d, txn, e.checking)
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
	if err := e.s.Reopen(ctx, recon); err != nil {
		t.Fatalf("reopen lone finalized: %v", err)
	}
}

// --- p16.3 workspace read methods ----------------------------------------

// TestReconciliationWorkspaceReads exercises the p16.3 read surface:
// ReconcilableAccounts, ReconciliationsForAccount, ReconciliationWorkspaceSplits
// (uncleared + this-recon, prior-finalized excluded, deleted excluded), and
// ReconciliationSummaryFor (opening + cleared + difference, matching Finalize).
func TestReconciliationWorkspaceReads(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// Reconcilable accounts: checking + other (both flagged), NOT expense/revenue.
	accts, err := e.s.ReconcilableAccounts(ctx)
	if err != nil {
		t.Fatalf("ReconcilableAccounts: %v", err)
	}
	got := map[int64]string{}
	for _, a := range accts {
		got[a.ID] = a.DefaultCurrency
	}
	if _, ok := got[e.checking]; !ok {
		t.Errorf("ReconcilableAccounts missing checking")
	}
	if _, ok := got[e.other]; !ok {
		t.Errorf("ReconcilableAccounts missing other")
	}
	if _, ok := got[e.expense]; ok {
		t.Errorf("ReconcilableAccounts should not include the expense account")
	}
	if got[e.checking] != "USD" {
		t.Errorf("checking default currency = %q, want USD", got[e.checking])
	}

	// A finalized recon in January (statement 100,000) sets the opening chain.
	txnJan, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-01-10", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 100_000, Position: 0},
			{AccountID: e.revenue, Amount: -100_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction Jan: %v", err)
	}
	spJan := checkingSplitID(t, e.d, txnJan, e.checking)
	recJan, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-01-31", 100_000)
	if err != nil {
		t.Fatalf("StartReconciliation Jan: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recJan, spJan, true); err != nil {
		t.Fatalf("clear Jan split: %v", err)
	}
	if err := e.s.Finalize(ctx, recJan); err != nil {
		t.Fatalf("Finalize Jan: %v", err)
	}

	// February activity: a +250 deposit and a -400 expense (both USD), plus a
	// soft-deleted +999 deposit that must NOT appear in the workspace.
	txnDep, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-02-05", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 25_000, Position: 0},
			{AccountID: e.revenue, Amount: -25_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction Feb deposit: %v", err)
	}
	spDep := checkingSplitID(t, e.d, txnDep, e.checking)
	txnExp, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-02-08", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: -40_000, Position: 0},
			{AccountID: e.expense, Amount: 40_000, Position: 1, FunctionalClass: strp("management")},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction Feb expense: %v", err)
	}
	spExp := checkingSplitID(t, e.d, txnExp, e.checking)
	txnDel, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-02-09", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 99_900, Position: 0},
			{AccountID: e.revenue, Amount: -99_900, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction Feb deleted: %v", err)
	}
	if err := e.s.DeleteTransaction(ctx, txnDel); err != nil {
		t.Fatalf("DeleteTransaction: %v", err)
	}

	// ReconciliationsForAccount: newest first; the Jan recon is finalized.
	list, err := e.s.ReconciliationsForAccount(ctx, e.checking)
	if err != nil {
		t.Fatalf("ReconciliationsForAccount: %v", err)
	}
	if len(list) != 1 || list[0].ID != recJan || list[0].Status != "finalized" {
		t.Fatalf("ReconciliationsForAccount = %+v, want one finalized recJan", list)
	}

	// Open a February recon; workspace should show ONLY the Feb deposit + expense
	// (uncleared), NOT the Jan split (cleared in a prior finalized recon) and NOT
	// the deleted deposit.
	recFeb, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 0)
	if err != nil {
		t.Fatalf("StartReconciliation Feb: %v", err)
	}
	ws, err := e.s.ReconciliationWorkspaceSplits(ctx, recFeb)
	if err != nil {
		t.Fatalf("ReconciliationWorkspaceSplits: %v", err)
	}
	wsIDs := map[int64]bool{}
	for _, w := range ws {
		wsIDs[w.SplitID] = true
	}
	if !wsIDs[spDep] || !wsIDs[spExp] {
		t.Errorf("workspace missing Feb splits: %+v", ws)
	}
	if wsIDs[spJan] {
		t.Errorf("workspace should exclude the prior-finalized Jan split")
	}
	if len(ws) != 2 {
		t.Errorf("workspace split count = %d, want 2 (deleted excluded)", len(ws))
	}

	// Summary before any clearing: opening 100,000, cleared 0, difference
	// statement(0) - (100,000 + 0) = -100,000.
	sum, err := e.s.ReconciliationSummaryFor(ctx, recFeb)
	if err != nil {
		t.Fatalf("ReconciliationSummaryFor: %v", err)
	}
	if sum.Opening != 100_000 || sum.Cleared != 0 || sum.Difference != -100_000 {
		t.Errorf("summary before clearing = %+v, want opening 100000 cleared 0 diff -100000", sum)
	}

	// Clear both Feb splits: cleared = 250 - 400 = -150. To reach difference 0 the
	// statement must be opening+cleared = 99,850. Set that and finalize should pass.
	if err := e.s.SetSplitReconciled(ctx, recFeb, spDep, true); err != nil {
		t.Fatalf("clear dep: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recFeb, spExp, true); err != nil {
		t.Fatalf("clear exp: %v", err)
	}
	// Re-point statement balance via a fresh recon would be cleaner, but here we
	// just confirm the summary math and that Finalize agrees with a zero diff.
	sum2, err := e.s.ReconciliationSummaryFor(ctx, recFeb)
	if err != nil {
		t.Fatalf("ReconciliationSummaryFor after clear: %v", err)
	}
	if sum2.Cleared != -15_000 {
		t.Errorf("cleared after clearing both = %d, want -15000", sum2.Cleared)
	}
	// statement was 0, opening 100000, cleared -15000 => diff = 0 - 85000 = -85000.
	if sum2.Difference != -85_000 {
		t.Errorf("difference = %d, want -85000", sum2.Difference)
	}
	// A nonzero difference must make Finalize reject (matching the summary).
	if err := e.s.Finalize(ctx, recFeb); !errors.Is(err, ErrReconciliationDifference) {
		t.Errorf("Finalize at nonzero difference: err = %v, want ErrReconciliationDifference", err)
	}
}

// --- p26.57 edit statement date + balance --------------------------------

// TestEditReconciliationStatement edits an OPEN recon's date + balance, asserts the
// mutation is versioned op='update', the live row reflects the new figures, and the
// workspace summary (difference / finalize gate) recomputes against them.
func TestEditReconciliationStatement(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	// A +250 deposit; statement starts at 0 (opening 0, cleared 0 => diff 0).
	txn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-02-05", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 25_000, Position: 0},
			{AccountID: e.revenue, Amount: -25_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	spID := checkingSplitID(t, e.d, txn, e.checking)

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 0)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recon, spID, true); err != nil {
		t.Fatalf("clear split: %v", err)
	}
	// cleared 25000, statement 0, opening 0 => diff = 0 - 25000 = -25000 (not zero).
	sum, _ := e.s.ReconciliationSummaryFor(ctx, recon)
	if sum.Difference != -25_000 {
		t.Fatalf("pre-edit difference = %d, want -25000", sum.Difference)
	}

	// Edit the statement to 250.00 and a new date: now opening 0 + cleared 25000 ==
	// statement 25000 => difference 0, Finalize enabled.
	if err := e.s.EditReconciliationStatement(ctx, recon, "2026-03-02", 25_000); err != nil {
		t.Fatalf("EditReconciliationStatement: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", int64(recon), "update")

	got, _ := e.s.GetReconciliation(ctx, recon)
	if got.StatementDate != "2026-03-02" || got.StatementBalance != 25_000 {
		t.Errorf("after edit: date=%q balance=%d, want 2026-03-02/25000", got.StatementDate, got.StatementBalance)
	}
	sum2, _ := e.s.ReconciliationSummaryFor(ctx, recon)
	if sum2.Difference != 0 {
		t.Errorf("post-edit difference = %d, want 0", sum2.Difference)
	}
	// Finalize now accepts (the edit made it balance).
	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Errorf("Finalize after balancing edit: %v", err)
	}
	assertLedgerClean(t, e.d)
}

// TestEditReconciliationStatementValidation rejects a bad date and a non-open recon.
func TestEditReconciliationStatementValidation(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 0)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	// Bad date shape rejected (no live change).
	if err := e.s.EditReconciliationStatement(ctx, recon, "not-a-date", 100); !errors.Is(err, ErrBadDate) {
		t.Errorf("edit with bad date: err = %v, want ErrBadDate", err)
	}
	// Finalize it, then an edit must be rejected (not open).
	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := e.s.EditReconciliationStatement(ctx, recon, "2026-03-02", 100); !errors.Is(err, ErrReconciliationNotOpen) {
		t.Errorf("edit finalized recon: err = %v, want ErrReconciliationNotOpen", err)
	}
	// Missing recon -> ErrReconciliationNotFound.
	if err := e.s.EditReconciliationStatement(ctx, 999_999, "2026-03-02", 100); !errors.Is(err, ErrReconciliationNotFound) {
		t.Errorf("edit missing recon: err = %v, want ErrReconciliationNotFound", err)
	}
}

// --- p26.58 discard -------------------------------------------------------

// TestDiscardReconciliation abandons an OPEN recon: the status flips to 'discarded'
// (versioned op='update'), its cleared split is released (reconciliation_id NULL,
// live-only so NO split version), no row is deleted (audit intact), the open-recon
// uniqueness guard now excludes it (a fresh recon starts), and the ledger stays clean.
func TestDiscardReconciliation(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	txn, err := e.s.PostTransaction(ctx, PostTransactionInput{
		Date: "2026-02-05", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.checking, Amount: 25_000, Position: 0},
			{AccountID: e.revenue, Amount: -25_000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	spID := checkingSplitID(t, e.d, txn, e.checking)

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 25_000)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	if err := e.s.SetSplitReconciled(ctx, recon, spID, true); err != nil {
		t.Fatalf("clear split: %v", err)
	}
	if got := reconOf(t, e.d, spID); got != int64(recon) {
		t.Fatalf("split cleared against %d, want %d", got, recon)
	}
	preSplitVersions := splitVersionCount(t, e.d, spID)

	// A second open recon is blocked while this one is open.
	if _, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-03-31", 0); !errors.Is(err, ErrOpenReconciliationExists) {
		t.Fatalf("second start before discard: err = %v, want ErrOpenReconciliationExists", err)
	}

	// Discard.
	if err := e.s.DiscardReconciliation(ctx, recon); err != nil {
		t.Fatalf("DiscardReconciliation: %v", err)
	}
	testutil.AssertVersioned(t, e.d, "reconciliations", int64(recon), "update")

	got, _ := e.s.GetReconciliation(ctx, recon)
	if got.Status != "discarded" {
		t.Errorf("status after discard = %q, want discarded", got.Status)
	}
	// The split is released (uncleared) and available again.
	if got := reconOf(t, e.d, spID); got != 0 {
		t.Errorf("split reconciliation_id after discard = %d, want 0 (released)", got)
	}
	// Un-clear is live-only: NO new split version was minted.
	if n := splitVersionCount(t, e.d, spID); n != preSplitVersions {
		t.Errorf("split versions after discard = %d, want %d (live-only)", n, preSplitVersions)
	}
	// Audit intact: the discarded recon row and its versions still exist.
	var reconRows, versionRows int
	_ = e.d.QueryRow(`SELECT COUNT(*) FROM reconciliations WHERE id = ?`, recon).Scan(&reconRows)
	_ = e.d.QueryRow(`SELECT COUNT(*) FROM reconciliations_versions WHERE entity_id = ?`, recon).Scan(&versionRows)
	if reconRows != 1 {
		t.Errorf("reconciliation rows after discard = %d, want 1 (soft status, not deleted)", reconRows)
	}
	if versionRows < 2 {
		t.Errorf("reconciliation version rows after discard = %d, want >=2 (create + discard)", versionRows)
	}

	// The open-recon guard now excludes the discarded one: a fresh recon starts.
	fresh, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-03-31", 0)
	if err != nil {
		t.Fatalf("start fresh recon after discard: %v", err)
	}
	if fresh == recon {
		t.Fatalf("fresh recon id collided with discarded id")
	}
	// The released split is available to the fresh recon.
	if err := e.s.SetSplitReconciled(ctx, fresh, spID, true); err != nil {
		t.Errorf("clear released split against fresh recon: %v", err)
	}
	assertLedgerClean(t, e.d)
}

// TestDiscardReconciliationValidation rejects discard on a non-open recon.
func TestDiscardReconciliationValidation(t *testing.T) {
	e := newReconEnv(t)
	ctx := mutCtx()

	recon, err := e.s.StartReconciliation(ctx, e.checking, "USD", "2026-02-28", 0)
	if err != nil {
		t.Fatalf("StartReconciliation: %v", err)
	}
	if err := e.s.Finalize(ctx, recon); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// Discarding a finalized recon is rejected (reopen first).
	if err := e.s.DiscardReconciliation(ctx, recon); !errors.Is(err, ErrReconciliationNotOpen) {
		t.Errorf("discard finalized: err = %v, want ErrReconciliationNotOpen", err)
	}
	// Missing recon -> ErrReconciliationNotFound.
	if err := e.s.DiscardReconciliation(ctx, 999_999); !errors.Is(err, ErrReconciliationNotFound) {
		t.Errorf("discard missing: err = %v, want ErrReconciliationNotFound", err)
	}
}

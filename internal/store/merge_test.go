package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"cuento/internal/ledger"
	"cuento/internal/testutil"
)

// Account merge tests (p08.5). These build tiny bespoke charts inline through the
// store (AGENTS testing conventions), reusing the transaction-test helpers
// (newTxnEnv, mkAcct, mutCtx, countChanges, txnSplits, splitVersionCount).

// liveSplitAccount reads one split's current live account_id.
func liveSplitAccount(t *testing.T, d *sql.DB, splitID int64) int64 {
	t.Helper()
	var acct int64
	if err := d.QueryRow(`SELECT account_id FROM splits WHERE id = ?`, splitID).Scan(&acct); err != nil {
		t.Fatalf("liveSplitAccount(%d): %v", splitID, err)
	}
	return acct
}

// countSplitsOnAccount counts live splits currently pointing at an account.
func countSplitsOnAccount(t *testing.T, d *sql.DB, accountID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM splits WHERE account_id = ?`, accountID).Scan(&n); err != nil {
		t.Fatalf("countSplitsOnAccount(%d): %v", accountID, err)
	}
	return n
}

// accountActive reads an account's live active flag.
func accountActive(t *testing.T, d *sql.DB, accountID int64) int64 {
	t.Helper()
	var a int64
	if err := d.QueryRow(`SELECT active FROM accounts WHERE id = ?`, accountID).Scan(&a); err != nil {
		t.Fatalf("accountActive(%d): %v", accountID, err)
	}
	return a
}

// postExpense posts a balanced expense txn (debit `expense`, credit `checking`)
// and returns the txn id plus its two live splits.
func (e txnEnv) postExpense(t *testing.T, expense int64, amount int64) (int64, []SplitState) {
	t.Helper()
	mgmt := "management"
	id, err := e.s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: expense, Amount: amount, FunctionalClass: &mgmt, Position: 0},
			{AccountID: e.checking, Amount: -amount, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	return id, txnSplits(t, e.d, id)
}

// splitOnAccount returns the one split (from a list) that sits on account acct.
func splitOnAccount(t *testing.T, sps []SplitState, acct int64) SplitState {
	t.Helper()
	for _, sp := range sps {
		if sp.AccountID == acct {
			return sp
		}
	}
	t.Fatalf("no split on account %d", acct)
	return SplitState{}
}

// --- happy path: repointing + versioning ---------------------------------

func TestMergeRepointsSplitsAndRecons(t *testing.T) {
	e := newTxnEnv(t)
	// Two expense siblings, same type, both mapped to the US sub. salaries has a
	// default class+program (mkAcct in newTxnEnv), supplies has neither -- both are
	// valid expense targets; merge supplies -> salaries.
	src := e.supplies
	dst := e.salaries

	// Two txns hit src, so there are two src splits to move.
	_, sps1 := e.postExpense(t, src, 10_000)
	_, sps2 := e.postExpense(t, src, 25_000)
	srcSplitA := splitOnAccount(t, sps1, src)
	srcSplitB := splitOnAccount(t, sps2, src)

	before := countChanges(t, e.d)
	if err := e.s.MergeAccount(mutCtx(), src, dst); err != nil {
		t.Fatalf("MergeAccount: %v", err)
	}

	// Exactly ONE new change anchors the whole merge.
	if n := countChanges(t, e.d); n != before+1 {
		t.Fatalf("changes = %d, want %d (one change for the merge)", n, before+1)
	}

	// Every src split now lives (live) on dst.
	for _, id := range []int64{srcSplitA.ID, srcSplitB.ID} {
		if got := liveSplitAccount(t, e.d, id); got != dst {
			t.Errorf("split %d account = %d, want dst %d", id, got, dst)
		}
	}
	// src has NO live splits left.
	if n := countSplitsOnAccount(t, e.d, src); n != 0 {
		t.Errorf("src still has %d live splits, want 0", n)
	}

	// Each moved split got an op='update' version row (2 rows total: create + update).
	for _, id := range []int64{srcSplitA.ID, srcSplitB.ID} {
		testutil.AssertVersioned(t, e.d, "splits", id, "update")
		if n := splitVersionCount(t, e.d, id); n != 2 {
			t.Errorf("split %d version count = %d, want 2 (create + merge update)", id, n)
		}
	}

	// The post-merge books stay integrity-clean: no split is stranded on the
	// deactivated source (Z2), and every moved split still maps its txn's
	// subsidiary (Z11). This proves the "move ALL splits, incl. soft-deleted-txn
	// splits" decision keeps `cuento check` clean.
	vs, err := ledger.Check(context.Background(), e.d)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("post-merge integrity error: %s: %s", v.Rule, v.Detail)
		}
	}

	// TODO(p16.1): once reconciliations exist, assert every reconciliation on src
	// is repointed to dst here. The recon table / splits.reconciliation_id do not
	// exist yet (p16.1); the split repointing is what this test asserts now.
}

// --- validations (each Blocked test satisfies EVERY other constraint so only
// the target error can fire) ---------------------------------------------

func TestMergeBlockedCrossTypeClass(t *testing.T) {
	e := newTxnEnv(t)
	// contrib is revenue, salaries is expense: both leaves, dst subs cover src.
	// Only the type check should trip.
	before := countChanges(t, e.d)
	err := e.s.MergeAccount(mutCtx(), e.contrib, e.salaries)
	if !errors.Is(err, ErrMergeCrossTypeClass) {
		t.Fatalf("err = %v, want ErrMergeCrossTypeClass", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

func TestMergeBlockedIntoPlaceholder(t *testing.T) {
	e := newTxnEnv(t)
	// Make dst a placeholder: create an expense child under a fresh expense parent.
	parent := mkAcct(t, e.s, "expense", "Occupancy", []int64{e.subUS}, nil, nil)
	pptr := parent
	child := mkAcct(t, e.s, "expense", "Rent", []int64{e.subUS}, nil, nil)
	if err := e.s.UpdateAccount(mutCtx(), child, UpdateAccountInput{ParentID: &pptr}); err != nil {
		t.Fatalf("reparent: %v", err)
	}
	// src = supplies (leaf), dst = parent (now has a child). Same type; subs cover.
	before := countChanges(t, e.d)
	err := e.s.MergeAccount(mutCtx(), e.supplies, parent)
	if !errors.Is(err, ErrMergeIntoPlaceholder) {
		t.Fatalf("err = %v, want ErrMergeIntoPlaceholder", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

func TestMergeBlockedSubsetSubs(t *testing.T) {
	e := newTxnEnv(t)
	// A second subsidiary; src maps to BOTH US and MX, dst maps only to US, so
	// dst's set does NOT cover src's. Both leaves, same type.
	subMX := newSub(t, e.s, rootID, "MX")
	src := mkAcct(t, e.s, "expense", "Travel", []int64{e.subUS, subMX}, nil, nil)
	dst := mkAcct(t, e.s, "expense", "Office", []int64{e.subUS}, nil, nil)

	before := countChanges(t, e.d)
	err := e.s.MergeAccount(mutCtx(), src, dst)
	if !errors.Is(err, ErrMergeSubsetSubs) {
		t.Fatalf("err = %v, want ErrMergeSubsetSubs", err)
	}
	if n := countChanges(t, e.d); n != before {
		t.Errorf("changes = %d, want %d (rejected merge leaves no trace)", n, before)
	}
}

// --- dst untouched -------------------------------------------------------

func TestMergeFunctionDefaultKept(t *testing.T) {
	e := newTxnEnv(t)
	// dst (salaries) has a default functional class 'management' (set in newTxnEnv).
	// Merging src (supplies) into it must NOT change dst's default.
	dst := e.salaries
	src := e.supplies
	// Give src at least one split so the repoint actually runs.
	e.postExpense(t, src, 5_000)

	before, err := e.s.GetAccount(mutCtx(), dst)
	if err != nil {
		t.Fatalf("GetAccount(dst) before: %v", err)
	}
	if err := e.s.MergeAccount(mutCtx(), src, dst); err != nil {
		t.Fatalf("MergeAccount: %v", err)
	}
	after, err := e.s.GetAccount(mutCtx(), dst)
	if err != nil {
		t.Fatalf("GetAccount(dst) after: %v", err)
	}

	if !after.FunctionalClass.Valid || after.FunctionalClass.String != "management" {
		t.Errorf("dst functional class = %v, want management (kept)", after.FunctionalClass)
	}
	if before.FunctionalClass != after.FunctionalClass ||
		before.DefaultProgramID != after.DefaultProgramID ||
		before.Active != after.Active ||
		before.Type != after.Type {
		t.Errorf("dst attributes changed: before %+v after %+v", before, after)
	}
	if after.Active != 1 {
		t.Errorf("dst active = %d, want 1 (dst untouched)", after.Active)
	}
	// src is deactivated.
	if a := accountActive(t, e.d, src); a != 0 {
		t.Errorf("src active = %d, want 0 (source deactivated)", a)
	}
	testutil.AssertVersioned(t, e.d, "accounts", src, "update")
}

// --- history intact ------------------------------------------------------

func TestMergeHistoryIntact(t *testing.T) {
	// Order create-before-merge with an injected clock so as-of times are distinct.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var tick int
	clock := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Minute)
	}
	e := newTxnEnvClock(t, clock)

	src := e.supplies
	dst := e.salaries

	// Post the txn (@ some T0). Capture the split id on src and the moment AFTER
	// the post but BEFORE the merge.
	txnID, sps := e.postExpense(t, src, 12_000)
	srcSplit := splitOnAccount(t, sps, src)
	beforeMerge := clock() // a timestamp strictly after the post, before the merge

	if err := e.s.MergeAccount(mutCtx(), src, dst); err != nil {
		t.Fatalf("MergeAccount: %v", err)
	}

	// As of a time BEFORE the merge: the split still sits on the SOURCE account.
	stBefore, err := e.s.TransactionAsOf(mutCtx(), txnID, beforeMerge)
	if err != nil {
		t.Fatalf("TransactionAsOf(before): %v", err)
	}
	asOfBefore := splitStateByID(t, stBefore, srcSplit.ID)
	if asOfBefore.AccountID != src {
		t.Errorf("as-of before merge: split account = %d, want src %d", asOfBefore.AccountID, src)
	}

	// As of NOW (after the merge): the same split sits on dst.
	stAfter, err := e.s.TransactionAsOf(mutCtx(), txnID, clock())
	if err != nil {
		t.Fatalf("TransactionAsOf(after): %v", err)
	}
	asOfAfter := splitStateByID(t, stAfter, srcSplit.ID)
	if asOfAfter.AccountID != dst {
		t.Errorf("as-of after merge: split account = %d, want dst %d", asOfAfter.AccountID, dst)
	}
}

// splitStateByID finds one split in a reconstructed transaction state.
func splitStateByID(t *testing.T, st TransactionState, id int64) SplitState {
	t.Helper()
	if !st.Present {
		t.Fatalf("transaction not present as of that time")
	}
	for _, sp := range st.Splits {
		if sp.ID == id {
			return sp
		}
	}
	t.Fatalf("split %d not in reconstructed state", id)
	return SplitState{}
}

// newTxnEnvClock builds the standard txnEnv but with an injected clock so the
// history test can order create-before-merge deterministically.
func newTxnEnvClock(t *testing.T, clock func() time.Time) txnEnv {
	t.Helper()
	d := testutil.NewDB(t)
	s := New(d, WithClock(clock))
	subUS := newSub(t, s, rootID, "US")

	educ, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Educacion"})
	if err != nil {
		t.Fatalf("CreateProgram: %v", err)
	}
	env := txnEnv{s: s, d: d, subUS: subUS, educ: educ}
	root := rootProgramID
	mgmt := "management"
	env.checking = mkAcct(t, s, "asset", "Checking", []int64{subUS}, nil, nil)
	env.salaries = mkAcct(t, s, "expense", "Salaries", []int64{subUS}, &mgmt, &root)
	env.supplies = mkAcct(t, s, "expense", "Supplies", []int64{subUS}, nil, nil)
	env.contrib = mkAcct(t, s, "revenue", "Contributions", []int64{subUS}, nil, nil)
	env.equity = mkAcct(t, s, "equity", "Opening Balances", []int64{subUS}, nil, nil)
	return env
}

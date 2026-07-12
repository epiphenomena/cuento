package store

import (
	"errors"
	"testing"
)

// p12.4 transaction history reconstruction. The timeline is rebuilt from the
// append-only version twins; these tests assert the STRUCTURED diffs (typed old/new
// values -- the web layer renders them), covering create / update / delete and the
// fund + functional-class split deltas the step names. Diffs are computed in Go, so
// they are asserted directly here (no HTML).

// findEntryOp returns the first entry whose header Op matches (create/update/delete).
func findEntryOp(t *testing.T, entries []HistoryEntry, op string) HistoryEntry {
	t.Helper()
	for _, e := range entries {
		if e.Op == op {
			return e
		}
	}
	t.Fatalf("no history entry with op=%s (got %d entries)", op, len(entries))
	return HistoryEntry{}
}

// hasHeaderDiff reports whether the entry carries a header field diff with new text.
func headerNew(e HistoryEntry, f HistoryField) (DiffValue, bool) {
	for _, d := range e.HeaderDiffs {
		if d.Field == f {
			return d.New, true
		}
	}
	return DiffValue{}, false
}

// splitFieldDiff finds a per-field diff within an entry's split diffs.
func splitFieldDiff(e HistoryEntry, f HistoryField) (FieldDiff, bool) {
	for _, sd := range e.SplitDiffs {
		for _, d := range sd.Fields {
			if d.Field == f {
				return d, true
			}
		}
	}
	return FieldDiff{}, false
}

// TestHistoryCreateRecordsActorAndInitialValues: a create yields one entry with the
// actor, timestamp, op=create, header diffs showing the initial values, and one
// split diff per split (op=create) carrying account/amount.
func TestHistoryCreateRecordsActorAndInitialValues(t *testing.T) {
	e := newTxnEnv(t)
	id := post2(t, e, e.subUS, e.salaries, 10000, e.checking, -10000)

	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	c := entries[0]
	if c.Op != "create" {
		t.Fatalf("op=%q, want create", c.Op)
	}
	if c.ActorID != 1 {
		t.Fatalf("actor id=%d, want 1", c.ActorID)
	}
	if c.ActorName == "" {
		t.Fatalf("actor name empty (must resolve from users.display_name)")
	}
	if c.At == "" {
		t.Fatalf("timestamp empty (must come from changes.at)")
	}
	if dv, ok := headerNew(c, FieldCurrency); !ok || dv.Text != "USD" {
		t.Fatalf("header currency diff = %+v ok=%v, want USD", dv, ok)
	}
	if len(c.SplitDiffs) != 2 {
		t.Fatalf("want 2 split diffs on create, got %d", len(c.SplitDiffs))
	}
	for _, sd := range c.SplitDiffs {
		if sd.Op != "create" {
			t.Fatalf("split op=%q, want create", sd.Op)
		}
	}
}

// TestHistoryUpdateShowsPerFieldDiff: editing the header memo yields an update entry
// whose header diff carries old="" -> new="revised".
func TestHistoryUpdateShowsPerFieldDiff(t *testing.T) {
	e := newTxnEnv(t)
	id := post2(t, e, e.subUS, e.salaries, 10000, e.checking, -10000)

	in := PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Memo: "revised", Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 10000, Position: 0},
			{AccountID: e.checking, Amount: -10000, Position: 1},
		},
	}
	if err := e.s.UpdateTransaction(mutCtx(), id, in); err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}

	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory: %v", err)
	}
	u := findEntryOp(t, entries, "update")
	dv, ok := headerNew(u, FieldMemo)
	if !ok || dv.Text != "revised" {
		t.Fatalf("memo diff new = %+v ok=%v, want 'revised'", dv, ok)
	}
}

// TestHistoryUpdateShowsFundAndClassSplitDiffs: changing a split's FUND and another
// split's FUNCTIONAL CLASS produces split diffs naming those exact fields with the
// right old/new ids/text (the step's headline assertion).
func TestHistoryUpdateShowsFundAndClassSplitDiffs(t *testing.T) {
	e := newTxnEnv(t)
	// Fund scoped to subUS so the salaries split can carry it (balanced within fund
	// requires both splits share the fund; keep it simple: retag BOTH splits' fund).
	fund := newFund(t, e.s, "Beca", []int64{e.subUS}, nil)

	// Create with class=program on the expense, no fund.
	prog := "program"
	id, err := e.s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.supplies, Amount: 10000, FunctionalClass: &prog, Position: 0},
			{AccountID: e.checking, Amount: -10000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// Read the created split ids so the update is a per-split UPDATE (diff by id),
	// not a delete-all + recreate -- otherwise the class change reads as a create.
	live, err := e.s.TransactionSplits(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionSplits: %v", err)
	}
	var supID, chkID int64
	for _, sp := range live {
		if sp.AccountID == e.supplies {
			supID = sp.ID
		} else {
			chkID = sp.ID
		}
	}

	// Update: tag BOTH splits with the fund (per-fund zero) and change the expense
	// class to management.
	mgmt := "management"
	if err := e.s.UpdateTransaction(mutCtx(), id, PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &supID, AccountID: e.supplies, Amount: 10000, FundID: &fund, FunctionalClass: &mgmt, Position: 0},
			{ID: &chkID, AccountID: e.checking, Amount: -10000, FundID: &fund, Position: 1},
		},
	}); err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}

	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory: %v", err)
	}
	u := findEntryOp(t, entries, "update")

	fd, ok := splitFieldDiff(u, FieldFund)
	if !ok {
		t.Fatalf("expected a fund split diff, got splits=%+v", u.SplitDiffs)
	}
	if fd.Old.ID.Valid {
		t.Fatalf("fund old should be unrestricted (invalid), got %+v", fd.Old.ID)
	}
	if !fd.New.ID.Valid || fd.New.ID.Int64 != fund {
		t.Fatalf("fund new id = %+v, want %d", fd.New.ID, fund)
	}

	cd, ok := splitFieldDiff(u, FieldFunctional)
	if !ok {
		t.Fatalf("expected a functional-class split diff, got splits=%+v", u.SplitDiffs)
	}
	if cd.Old.Text != "program" || cd.New.Text != "management" {
		t.Fatalf("class diff old=%q new=%q, want program->management", cd.Old.Text, cd.New.Text)
	}
}

// TestHistoryUpdateShowsRemovedSplit: dropping a split on an edit yields a split diff
// with op=delete carrying the removed line's fields (the removed-split render path).
func TestHistoryUpdateShowsRemovedSplit(t *testing.T) {
	e := newTxnEnv(t)
	// A 3-split txn: two debits (salaries, supplies-with-class) and one credit.
	prog := "program"
	id, err := e.s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: e.salaries, Amount: 6000, Position: 0},
			{AccountID: e.supplies, Amount: 4000, FunctionalClass: &prog, Position: 1},
			{AccountID: e.checking, Amount: -10000, Position: 2},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	live, err := e.s.TransactionSplits(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionSplits: %v", err)
	}
	var salID, chkID int64
	for _, sp := range live {
		switch sp.AccountID {
		case e.salaries:
			salID = sp.ID
		case e.checking:
			chkID = sp.ID
		}
	}
	// Drop the supplies split (keep salaries + checking, still balanced at 6000).
	if err := e.s.UpdateTransaction(mutCtx(), id, PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.subUS, Currency: "USD",
		Splits: []SplitInput{
			{ID: &salID, AccountID: e.salaries, Amount: 6000, Position: 0},
			{ID: &chkID, AccountID: e.checking, Amount: -6000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}

	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory: %v", err)
	}
	u := findEntryOp(t, entries, "update")
	var removed *SplitDiff
	for i := range u.SplitDiffs {
		if u.SplitDiffs[i].Op == "delete" {
			removed = &u.SplitDiffs[i]
		}
	}
	if removed == nil {
		t.Fatalf("expected a removed split diff (op=delete), got %+v", u.SplitDiffs)
	}
	// The removed line carries its old-side account (supplies) -- one-sided.
	found := false
	for _, d := range removed.Fields {
		if d.Field == FieldAccount && d.Old.ID.Valid && d.Old.ID.Int64 == e.supplies {
			found = true
		}
	}
	if !found {
		t.Fatalf("removed split diff missing the old-side supplies account: %+v", removed.Fields)
	}
}

// TestHistorySplitOnlyChangeOrdered: a change that touches ONLY a split (no header
// version -- an account merge repoints and versions a split) is placed by its own
// change_id, in chronological order, NOT appended after all header changes.
func TestHistorySplitOnlyChangeOrdered(t *testing.T) {
	e := newTxnEnv(t)
	id := post2(t, e, e.subUS, e.salaries, 10000, e.checking, -10000)

	// A second leaf checking-like account to merge INTO checking (same asset type,
	// same sub). MergeAccount repoints splits and versions each -- a SPLIT-ONLY change.
	other := mkAcct(t, e.s, "asset", "Checking Two", []int64{e.subUS}, nil, nil)
	// Post a txn using `other` so the merge has a split to repoint on THIS txn's account
	// set is not required; merge repoints ALL splits on `other`. To make the merge touch
	// OUR txn's history, merge `checking` INTO `other` (our split moves to `other`).
	if err := e.s.MergeAccount(mutCtx(), e.checking, other); err != nil {
		t.Fatalf("MergeAccount: %v", err)
	}

	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory: %v", err)
	}
	// Entries must be sorted by change_id ascending (chronological): the create is
	// first, the merge (split-only) is last.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].ChangeID > entries[i].ChangeID {
			t.Fatalf("entries out of order: %d before %d", entries[i-1].ChangeID, entries[i].ChangeID)
		}
	}
	last := entries[len(entries)-1]
	if len(last.SplitDiffs) == 0 {
		t.Fatalf("the split-only merge change should carry a split diff, got %+v", last)
	}
}

// TestHistoryVoidVisibleAfterDelete: a VOIDED transaction still has history (the
// delete case), and the delete entry carries op=delete. This is the trap the guard
// must avoid -- GetTransaction 404s a soft-deleted txn, but history must not.
func TestHistoryVoidVisibleAfterDelete(t *testing.T) {
	e := newTxnEnv(t)
	id := post2(t, e, e.subUS, e.salaries, 10000, e.checking, -10000)
	if err := e.s.DeleteTransaction(mutCtx(), id); err != nil {
		t.Fatalf("DeleteTransaction: %v", err)
	}

	entries, err := e.s.TransactionHistory(mutCtx(), id)
	if err != nil {
		t.Fatalf("TransactionHistory after void: %v", err)
	}
	// create + delete.
	findEntryOp(t, entries, "create")
	findEntryOp(t, entries, "delete")
}

// TestHistoryMissingTransaction: a never-created id has no version rows ->
// ErrTransactionNotFound.
func TestHistoryMissingTransaction(t *testing.T) {
	e := newTxnEnv(t)
	if _, err := e.s.TransactionHistory(mutCtx(), 99999); !errors.Is(err, ErrTransactionNotFound) {
		t.Fatalf("missing txn err = %v, want ErrTransactionNotFound", err)
	}
}

// --- helpers --------------------------------------------------------------

// post2 posts a balanced 2-split transaction and returns its id. debitAcct must
// carry (or default) any required program/class -- newTxnEnv's salaries has both.
func post2(t *testing.T, e txnEnv, sub, debitAcct int64, debitAmt int64, creditAcct, creditAmt int64) int64 {
	t.Helper()
	id, err := e.s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: sub, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: debitAcct, Amount: debitAmt, Position: 0},
			{AccountID: creditAcct, Amount: creditAmt, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("post2: %v", err)
	}
	return id
}

// newFund creates a fund scoped to subs (optionally a program scope) and returns id.
func newFund(t *testing.T, s *Store, name string, subs []int64, programID *int64) int64 {
	t.Helper()
	id, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: name, Restriction: "purpose", Subsidiaries: subs, ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("CreateFund(%s): %v", name, err)
	}
	return id
}

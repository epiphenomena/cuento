package store

import (
	"testing"
)

// p12.3 payee autocomplete + template autofill. These tests build a tiny chart
// inline (AGENTS testing conventions) reusing the txnEnv helper: one subsidiary, a
// few leaf accounts, and payees with transactions on different dates so the ranking
// (most-recent-first) and the last-transaction template are observable.

// postForPayee posts a balanced 2-split txn (salaries debit / checking credit) on
// `date` tagged with `payeeID`, returning the new transaction id.
func (e txnEnv) postForPayee(t *testing.T, payeeID int64, date string, amount int64) int64 {
	t.Helper()
	in := e.balancedInput(amount)
	in.Date = date
	in.PayeeID = &payeeID
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("post for payee %d: %v", payeeID, err)
	}
	return id
}

// TestPayeeSuggestRanking: prefix match (case-insensitive), most-recent-first by the
// payee's latest non-deleted transaction; never-used / only-deleted payees rank last
// (then by name).
func TestPayeeSuggestRanking(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	// Four payees whose names share the "Ac" prefix, plus one that does not.
	acme, err := s.CreatePayee(mutCtx(), "Acme Supplies")
	if err != nil {
		t.Fatalf("payee acme: %v", err)
	}
	acorn, err := s.CreatePayee(mutCtx(), "Acorn Press")
	if err != nil {
		t.Fatalf("payee acorn: %v", err)
	}
	across, err := s.CreatePayee(mutCtx(), "Across Town") // never used
	if err != nil {
		t.Fatalf("payee across: %v", err)
	}
	ace, err := s.CreatePayee(mutCtx(), "Ace Repair") // only a deleted txn, sorts before Across by name
	if err != nil {
		t.Fatalf("payee ace: %v", err)
	}
	other, err := s.CreatePayee(mutCtx(), "Zenith Ltd") // does NOT match "Ac"
	if err != nil {
		t.Fatalf("payee zenith: %v", err)
	}

	// acorn's latest live txn is MORE RECENT than acme's -> acorn ranks first.
	e.postForPayee(t, acme, "2025-01-10", 5000)
	e.postForPayee(t, acorn, "2025-01-05", 5000)
	e.postForPayee(t, acorn, "2025-03-20", 7000) // acorn's most-recent
	// A deleted txn must NOT lift a payee: give "ace" a txn then delete it, so it
	// still ranks with the never-used tail (by name).
	delID := e.postForPayee(t, ace, "2025-06-01", 3000)
	if err := s.DeleteTransaction(mutCtx(), delID); err != nil {
		t.Fatalf("delete ace txn: %v", err)
	}

	got, err := s.SuggestPayees(mutCtx(), "ac") // case-insensitive prefix
	if err != nil {
		t.Fatalf("SuggestPayees: %v", err)
	}

	var ids []int64
	for _, sug := range got {
		ids = append(ids, sug.ID)
	}
	// Expected order: acorn (2025-03-20), acme (2025-01-10), then the used-less tail
	// by name: Ace Repair (only a deleted txn), Across Town (never used). Zenith is
	// excluded (no prefix match).
	want := []int64{acorn, acme, ace, across}
	if len(ids) != len(want) {
		t.Fatalf("suggest ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("suggest order = %v, want %v (pos %d)", ids, want, i)
		}
	}
	if containsID(ids, other) {
		t.Fatalf("Zenith (%d) should not match prefix 'ac': %v", other, ids)
	}

	// Empty query -> no suggestions (autocomplete shows nothing until the user types).
	if empty, err := s.SuggestPayees(mutCtx(), "   "); err != nil || len(empty) != 0 {
		t.Fatalf("empty query: got %v err %v, want empty", empty, err)
	}
}

// TestPayeeLastTransactionTemplate: the payee's last non-deleted transaction's
// splits are returned (the store half of the template prefill); a payee with no live
// transaction yields Found=false.
func TestPayeeLastTransactionTemplate(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	vendor, err := s.CreatePayee(mutCtx(), "Vendor Inc")
	if err != nil {
		t.Fatalf("payee: %v", err)
	}
	// An older txn, then the LATEST one -> the template must reflect the latest.
	e.postForPayee(t, vendor, "2025-01-01", 4000)
	lastID := e.postForPayee(t, vendor, "2025-05-15", 9000)

	tpl, err := s.PayeeLastTransactionTemplate(mutCtx(), vendor)
	if err != nil {
		t.Fatalf("PayeeLastTransactionTemplate: %v", err)
	}
	if !tpl.Found {
		t.Fatalf("want Found for a used payee")
	}
	if tpl.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", tpl.Currency)
	}
	if len(tpl.Splits) != 2 {
		t.Fatalf("want 2 splits, got %d", len(tpl.Splits))
	}
	// The template is the LATEST txn (amount 9000), not the older 4000.
	for _, sp := range tpl.Splits {
		if sp.TransactionID != lastID {
			t.Fatalf("split from txn %d, want latest %d", sp.TransactionID, lastID)
		}
		if sp.AccountID == e.salaries && sp.Amount != 9000 {
			t.Fatalf("salaries amount = %d, want 9000 (latest txn)", sp.Amount)
		}
	}

	// A never-used payee -> Found=false.
	ghost, err := s.CreatePayee(mutCtx(), "Ghost Co")
	if err != nil {
		t.Fatalf("ghost: %v", err)
	}
	empty, err := s.PayeeLastTransactionTemplate(mutCtx(), ghost)
	if err != nil {
		t.Fatalf("template ghost: %v", err)
	}
	if empty.Found {
		t.Fatalf("want Found=false for an unused payee")
	}
}

// TestEnsurePayee: find-or-create by name is case-insensitive and idempotent (a
// repeat by a differently-cased name returns the SAME id, never a duplicate).
func TestEnsurePayee(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	id1, err := s.EnsurePayee(mutCtx(), "Acme Co")
	if err != nil {
		t.Fatalf("EnsurePayee create: %v", err)
	}
	if id1 == 0 {
		t.Fatalf("want a new payee id")
	}
	// A repeat (different case) must find the existing one, not create a second.
	id2, err := s.EnsurePayee(mutCtx(), "acme co")
	if err != nil {
		t.Fatalf("EnsurePayee find: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("EnsurePayee returned %d, want existing %d", id2, id1)
	}
	// A blank name is a no-op (no payee, no error).
	if id, err := s.EnsurePayee(mutCtx(), "   "); err != nil || id != 0 {
		t.Fatalf("blank name: id=%d err=%v, want 0/nil", id, err)
	}
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

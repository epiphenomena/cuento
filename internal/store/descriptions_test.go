package store

import (
	"testing"

	"cuento/internal/ids"
)

// p26.18 per-split description autocomplete + per-row prefill (step 4a of the
// payee->description migration). These tests build on the shared txnEnv (single
// subsidiary) reusing balancedInput, seeding splits with descriptions across dates
// so the ranking (most-recent-first), distinctness, exact-match prefill, and
// no-match paths are observable at the store level. Sub-PREFERENCE (which needs two
// subsidiaries + a cross-sub account) is asserted in the web handler test, where the
// multi-sub env exists.

// postWithDesc posts a balanced 2-split txn on `date` whose salaries (debit) split
// carries `desc` as its description, returning the new transaction id.
func (e txnEnv) postWithDesc(t *testing.T, date, desc string, amount int64) ids.TransactionID {
	t.Helper()
	in := e.balancedInput(amount)
	in.Date = date
	in.Splits[0].Description = desc
	id, err := e.s.PostTransaction(mutCtx(), in)
	if err != nil {
		t.Fatalf("post with desc %q: %v", desc, err)
	}
	return id
}

// TestSuggestDescriptionsRanking: distinct non-empty descriptions, substring +
// case-insensitive match, most-recently-used first; empty query -> nothing.
func TestSuggestDescriptionsRanking(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	// "Office rent" used twice; the LATER use lifts it. "Office supplies" once,
	// earlier. "Utilities" does not match "office". A blank-description split must not
	// appear. Case: query "office" matches "Office ..." (case-insensitive).
	e.postWithDesc(t, "2025-01-05", "Office rent", 5000) // early
	e.postWithDesc(t, "2025-02-10", "Office supplies", 4000)
	e.postWithDesc(t, "2025-03-20", "Office rent", 6000) // Office rent's most-recent -> ranks first
	e.postWithDesc(t, "2025-04-01", "Utilities", 3000)   // no "office" match
	e.postWithDesc(t, "2025-05-01", "", 2000)            // blank -> excluded

	got, err := s.SuggestDescriptions(mutCtx(), "office", 0)
	if err != nil {
		t.Fatalf("SuggestDescriptions: %v", err)
	}
	want := []string{"Office rent", "Office supplies"}
	if len(got) != len(want) {
		t.Fatalf("suggestions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suggestion order = %v, want %v (pos %d)", got, want, i)
		}
	}

	// A mid-string substring match ("ent" is inside "rent") -> substring, not prefix.
	if sub, err := s.SuggestDescriptions(mutCtx(), "ent", 0); err != nil || len(sub) != 1 || sub[0] != "Office rent" {
		t.Fatalf("substring 'ent' = %v err %v, want [Office rent]", sub, err)
	}

	// Empty / whitespace query -> nothing.
	if empty, err := s.SuggestDescriptions(mutCtx(), "   ", 0); err != nil || len(empty) != 0 {
		t.Fatalf("empty query: got %v err %v, want empty", empty, err)
	}
}

// TestSuggestDescriptionsExcludesDeleted: a description used ONLY on a deleted txn
// must not appear.
func TestSuggestDescriptionsExcludesDeleted(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	del := e.postWithDesc(t, "2025-06-01", "Ghost payment", 3000)
	if err := s.DeleteTransaction(mutCtx(), del); err != nil {
		t.Fatalf("delete: %v", err)
	}
	e.postWithDesc(t, "2025-06-02", "Live payment", 3000)

	got, err := s.SuggestDescriptions(mutCtx(), "payment", 0)
	if err != nil {
		t.Fatalf("SuggestDescriptions: %v", err)
	}
	if len(got) != 1 || got[0] != "Live payment" {
		t.Fatalf("suggestions = %v, want [Live payment] (deleted excluded)", got)
	}
}

// TestSuggestDescriptionsLimit: at most 10 distinct suggestions are returned even
// when more descriptions match, and the cap keeps the most-recent ones.
func TestSuggestDescriptionsLimit(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	// 12 distinct descriptions all matching "svc", each on its own date so recency is
	// a total order. The dates ascend with i, so "svc-11" is the most recent.
	for i := 0; i < 12; i++ {
		date := "2025-01-" + twoDigit(i+1)
		e.postWithDesc(t, date, "svc-"+twoDigit(i), int64(1000+i))
	}

	got, err := s.SuggestDescriptions(mutCtx(), "svc", 0)
	if err != nil {
		t.Fatalf("SuggestDescriptions: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("suggestions = %d, want 10 (capped)", len(got))
	}
	// Capped to the 10 MOST-RECENT: svc-11 (newest) first, svc-2 (10th) last; the two
	// oldest (svc-0, svc-1) are dropped.
	if got[0] != "svc-11" {
		t.Fatalf("first = %q, want svc-11 (newest)", got[0])
	}
	for _, dropped := range []string{"svc-00", "svc-01"} {
		for _, g := range got {
			if g == dropped {
				t.Fatalf("%q should have been dropped by the LIMIT\n%v", dropped, got)
			}
		}
	}
}

// twoDigit zero-pads a small non-negative int to two digits (for lexicographic dates
// / stable distinct labels in the limit test).
func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// TestPrefillDescriptionExact: the most-recent split with an EXACT description is
// returned with its account/amount/memo; a description used only as a SUBSTRING of
// another does NOT match; no exact match -> Found=false.
func TestPrefillDescriptionExact(t *testing.T) {
	e := newTxnEnv(t)
	s := e.s

	// Two txns with the SAME exact description; the later one wins. Its salaries debit
	// carries the description, amount +6000; the checking credit carries "".
	e.postWithDesc(t, "2025-01-01", "Monthly rent", 5000)
	e.postWithDesc(t, "2025-07-01", "Monthly rent", 6000) // most-recent -> chosen

	pf, err := s.PrefillDescription(mutCtx(), "Monthly rent", 0)
	if err != nil {
		t.Fatalf("PrefillDescription: %v", err)
	}
	if !pf.Found {
		t.Fatalf("expected a match for 'Monthly rent'")
	}
	if pf.AccountID != e.salaries {
		t.Fatalf("account = %d, want salaries %d", pf.AccountID, e.salaries)
	}
	if pf.Amount != 6000 {
		t.Fatalf("amount = %d, want 6000 (the most-recent split)", pf.Amount)
	}
	if pf.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", pf.Currency)
	}
	// salaries has a default program (root) + default class (management).
	if pf.ProgramID != rootProgramID {
		t.Fatalf("program = %d, want root %d", pf.ProgramID, rootProgramID)
	}
	if pf.Class != "management" {
		t.Fatalf("class = %q, want management", pf.Class)
	}

	// Exact match only: "Monthly" (a prefix, not the full string) does NOT match.
	if partial, err := s.PrefillDescription(mutCtx(), "Monthly", 0); err != nil || partial.Found {
		t.Fatalf("partial 'Monthly': found=%v err=%v, want no match", partial.Found, err)
	}
	// No match at all -> Found=false, no error.
	if none, err := s.PrefillDescription(mutCtx(), "Nonexistent", 0); err != nil || none.Found {
		t.Fatalf("nonexistent: found=%v err=%v, want no match", none.Found, err)
	}
	// Blank query -> Found=false.
	if blank, err := s.PrefillDescription(mutCtx(), "  ", 0); err != nil || blank.Found {
		t.Fatalf("blank: found=%v err=%v, want no match", blank.Found, err)
	}
}

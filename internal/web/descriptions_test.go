package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p26.18 per-split description autocomplete + per-row prefill handler tests (step 4a
// of the payee->description migration). Driven through the REAL mounted router
// (httptest) against a real migrated db (AGENTS conventions); no store mocks. The
// multi-sub txnWebEnv (sub1/sub2, salaries mapped to both) exercises the p26.38 sub
// SCOPING (filter, not prefer), which the single-sub store test cannot.

// seedDescTxn posts a balanced 2-split txn in `sub` on `date`: salaries (debit,
// program+class defaulted) carrying `desc` as its description + memo `memo`, and
// checking-or-cashB (credit) with no description. sub picks the matching asset
// (sub1 -> checking, sub2 -> cashB) so the txn is single-subsidiary-valid.
func (e *txnWebEnv) seedDescTxn(t *testing.T, sub ids.SubsidiaryID, date, desc, memo string, amount int64) {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	asset := e.checking
	if sub == e.sub2 {
		asset = e.cashB
	}
	in := store.PostTransactionInput{
		Date: date, SubsidiaryID: sub, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: amount, FunctionalClass: strptr("program"), Description: desc, Memo: memo, Position: 0},
			{AccountID: asset, Amount: -amount, Position: 1},
		},
	}
	if _, err := e.st.PostTransaction(ctx, in); err != nil {
		t.Fatalf("seed desc txn %q: %v", desc, err)
	}
}

// TestDescriptionsSuggest: GET /descriptions/suggest returns the ranked distinct
// descriptions as desc-suggestion <li>s; sub preference floats a sub-scoped match
// first; empty q -> no items.
func TestDescriptionsSuggest(t *testing.T) {
	e := newTxnWebEnv(t)

	// "Rent office" in sub1 (older) and sub2 (newer); "Rent parking" in sub1.
	e.seedDescTxn(t, e.sub1, "2025-01-05", "Rent office", "", 5000)
	e.seedDescTxn(t, e.sub2, "2025-03-20", "Rent office", "", 6000) // newer, in sub2
	e.seedDescTxn(t, e.sub1, "2025-02-01", "Rent parking", "", 4000)

	// No sub preference: pure recency -> "Rent office" (2025-03-20) then "Rent parking".
	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/suggest?q=rent&sub=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("suggest: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if n := strings.Count(body, `class="desc-suggestion"`); n != 2 {
		t.Fatalf("want 2 distinct suggestions, got %d\n%s", n, body)
	}
	off := strings.Index(body, "Rent office")
	park := strings.Index(body, "Rent parking")
	if off < 0 || park < 0 || off > park {
		t.Fatalf("expected 'Rent office' before 'Rent parking' (recency)\n%s", body)
	}
	// The data-description attribute carries the full text.
	if !strings.Contains(body, `data-description="Rent office"`) {
		t.Fatalf("missing data-description for Rent office\n%s", body)
	}

	// p26.38 sub SCOPING (filter, not prefer): a sub2-ONLY description must NOT appear in
	// sub1's suggestions at all (subsidiary A never surfaces B's descriptions). "Rent tower"
	// is the newest overall but lives only in sub2.
	e.seedDescTxn(t, e.sub2, "2025-09-09", "Rent tower", "", 7000) // sub2 only, newest
	rec2 := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/suggest?q=rent&sub="+itoa(int64(e.sub1)), nil)
	body2 := rec2.Body.String()
	if strings.Contains(body2, "Rent tower") {
		t.Fatalf("sub1 scope: sub2-only 'Rent tower' must NOT appear\n%s", body2)
	}
	// sub1's own descriptions ("Rent office" used in sub1, "Rent parking") are still there.
	if !strings.Contains(body2, "Rent parking") || !strings.Contains(body2, "Rent office") {
		t.Fatalf("sub1 scope: expected sub1's own 'Rent office' + 'Rent parking'\n%s", body2)
	}
	// The converse: sub2's suggestions include "Rent tower" but NOT sub1-only "Rent parking".
	rec2b := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/suggest?q=rent&sub="+itoa(int64(e.sub2)), nil)
	body2b := rec2b.Body.String()
	if !strings.Contains(body2b, "Rent tower") || strings.Contains(body2b, "Rent parking") {
		t.Fatalf("sub2 scope: expected 'Rent tower' and NOT sub1-only 'Rent parking'\n%s", body2b)
	}

	// Empty q -> an empty listbox (no <li> items), 200.
	rec3 := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/suggest?q=&sub="+itoa(int64(e.sub1)), nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("empty-q suggest: status=%d", rec3.Code)
	}
	if strings.Contains(rec3.Body.String(), `class="desc-suggestion"`) {
		t.Fatalf("empty q should render no suggestions\n%s", rec3.Body.String())
	}
}

// TestDescriptionsPrefill: GET /descriptions/prefill returns the most-recent EXACT
// match's fields as data-* on #desc-prefill; sub preference chooses a sub-scoped
// split over a more-recent cross-sub one; no match -> data-found="0".
func TestDescriptionsPrefill(t *testing.T) {
	e := newTxnWebEnv(t)

	// The SAME exact description in sub1 (amount 8000) and sub2 (amount 9000, newer).
	// With NO sub preference the newer sub2 split (9000) wins; preferring sub1 chooses
	// the sub1 split (8000) even though it is older.
	e.seedDescTxn(t, e.sub1, "2025-01-01", "Consulting fee", "sub1 memo", 8000)
	e.seedDescTxn(t, e.sub2, "2025-05-01", "Consulting fee", "sub2 memo", 9000)

	// No preference -> the most-recent (sub2, 9000).
	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/prefill?q=Consulting+fee&sub=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prefill: status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-found="1"`) {
		t.Fatalf("expected a match\n%s", body)
	}
	if !strings.Contains(body, `data-account="`+itoa(e.salaries)+`"`) {
		t.Fatalf("account not carried\n%s", body)
	}
	if !strings.Contains(body, `data-amount="90.00"`) {
		t.Fatalf("amount not the most-recent (90.00)\n%s", body)
	}
	if !strings.Contains(body, `data-memo="sub2 memo"`) {
		t.Fatalf("memo not the most-recent split's\n%s", body)
	}
	if !strings.Contains(body, `data-class="program"`) {
		t.Fatalf("class not carried\n%s", body)
	}

	// p26.38 sub SCOPING: sub1 -> the sub1 split (80.00, sub1 memo) even though sub2 is
	// newer (the sub2 split is FILTERED OUT, so it never leaks its account/memo into sub1).
	rec2 := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/prefill?q=Consulting+fee&sub="+itoa(int64(e.sub1)), nil)
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `data-amount="80.00"`) || !strings.Contains(body2, `data-memo="sub1 memo"`) {
		t.Fatalf("sub1 scope: expected the sub1 split (80.00 / sub1 memo)\n%s", body2)
	}
	// A description that exists ONLY in sub2, queried under sub1 -> no match (data-found=0):
	// subsidiary A never pulls B's split.
	e.seedDescTxn(t, e.sub2, "2025-06-01", "Sub2 vendor", "sub2 only", 3000)
	recX := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/prefill?q=Sub2+vendor&sub="+itoa(int64(e.sub1)), nil)
	if recX.Code != http.StatusOK || !strings.Contains(recX.Body.String(), `data-found="0"`) {
		t.Fatalf("sub1 scope: a sub2-only description must not prefill under sub1\n%s", recX.Body.String())
	}

	// Exact-only: a substring ("Consulting") is NOT an exact match -> data-found="0".
	recP := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/prefill?q=Consulting&sub=0", nil)
	if recP.Code != http.StatusOK || !strings.Contains(recP.Body.String(), `data-found="0"`) {
		t.Fatalf("partial should not match: status=%d body=%s", recP.Code, recP.Body.String())
	}

	// No match / empty q -> data-found="0", 200 (the reachability sweep sends a bare
	// request; it must not error).
	recN := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/descriptions/prefill", nil)
	if recN.Code != http.StatusOK || !strings.Contains(recN.Body.String(), `data-found="0"`) {
		t.Fatalf("no-match/empty should be 200 empty: status=%d body=%s", recN.Code, recN.Body.String())
	}
}

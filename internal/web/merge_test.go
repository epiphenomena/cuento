package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/store"
)

// p11.2 merge-UI handler tests. They drive the REAL mounted router (httptest)
// against a real migrated db (AGENTS testing conventions) -- the store is never
// mocked. Accounts + transactions are seeded through the store directly (as the
// p11.1 accounts_test.go does); the merge itself goes through the HTTP handlers.
//
// The flow under test (see merge.go):
//   - GET  /accounts/merge          -> the merge form partial (source/destination selects)
//   - POST /accounts/merge          -> WITHOUT confirm: a consequences PREVIEW that
//                                       does NOT execute; WITH confirm=1: the merge.
// Typed store errors surface as localized validation messages (422 + partial),
// reusing the p10.3 form-error convention.

// mergeEnv seeds two same-type leaf accounts (both mapped to the root sub) plus a
// balanced transaction whose expense split sits on src, so a merge has exactly one
// split to repoint. It returns the app, store, sessions, a bookkeeper id, and the
// src/dst account ids and the src split id.
type mergeEnv struct {
	h    http.Handler
	st   *store.Store
	sm   *scs.SessionManager
	book int64
	src  int64
	dst  int64
}

func seedMergeEnv(t *testing.T) mergeEnv {
	t.Helper()
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Two expense siblings, same type, both mapped to root sub 1.
	mgmt := "management"
	src, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Supplies"},
		Subsidiaries: []int64{1}, FunctionalClass: &mgmt,
	})
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	dst, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Office"},
		Subsidiaries: []int64{1}, FunctionalClass: &mgmt,
	})
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	cash, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}
	if _, err := st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-02-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []store.SplitInput{
			{AccountID: src, Amount: 5000, FunctionalClass: &mgmt, Position: 0},
			{AccountID: cash, Amount: -5000, Position: 1},
		},
	}); err != nil {
		t.Fatalf("post txn: %v", err)
	}

	// Sanity: exactly one split sits on src (the single merge target).
	ids, err := st.SplitIDsForAccount(context.Background(), src)
	if err != nil {
		t.Fatalf("SplitIDsForAccount: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 split on src, got %d", len(ids))
	}
	return mergeEnv{h: h, st: st, sm: sm, book: book, src: src, dst: dst}
}

// liveSplitAccountWeb reads a split's live account_id via the store's read helper.
func liveSplitAccountWeb(t *testing.T, st *store.Store, srcAccount int64) int {
	t.Helper()
	ids, err := st.SplitIDsForAccount(context.Background(), srcAccount)
	if err != nil {
		t.Fatalf("SplitIDsForAccount: %v", err)
	}
	return len(ids)
}

// TestMergeHappyPath: a valid confirmed merge repoints the src split onto dst and
// deactivates src. Asserted via the store/db, not scraped HTML.
func TestMergeHappyPath(t *testing.T) {
	e := seedMergeEnv(t)

	form := url.Values{}
	form.Set("src", itoa(e.src))
	form.Set("dst", itoa(e.dst))
	form.Set("confirm", "1")

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/accounts/merge", form)
	if rec.Code >= 400 {
		t.Fatalf("confirmed merge returned %d, body: %s", rec.Code, rec.Body.String())
	}
	// src has no live splits left; dst now carries it.
	if n := liveSplitAccountWeb(t, e.st, e.src); n != 0 {
		t.Errorf("src still has %d live splits after merge, want 0", n)
	}
	if n := liveSplitAccountWeb(t, e.st, e.dst); n != 1 {
		t.Errorf("dst has %d live splits after merge, want 1", n)
	}
	// src deactivated.
	acct, _ := e.st.GetAccount(context.Background(), e.src)
	if acct.Active != 0 {
		t.Errorf("src still active after merge")
	}
}

// TestMergeConfirmRequired: a POST WITHOUT the confirm flag shows the
// consequences/confirmation and does NOT execute (the split stays on src).
func TestMergeConfirmRequired(t *testing.T) {
	e := seedMergeEnv(t)

	form := url.Values{}
	form.Set("src", itoa(e.src))
	form.Set("dst", itoa(e.dst))
	// no confirm flag

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/accounts/merge", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// NOTHING executed: the split is still on src, src still active.
	if n := liveSplitAccountWeb(t, e.st, e.src); n != 1 {
		t.Errorf("preview executed the merge: src has %d live splits, want 1", n)
	}
	acct, _ := e.st.GetAccount(context.Background(), e.src)
	if acct.Active == 0 {
		t.Errorf("preview deactivated src, want still active")
	}
	// The confirmation body must offer a confirm control.
	if !strings.Contains(rec.Body.String(), `name="confirm"`) {
		t.Errorf("preview body missing a confirm control; body: %s", rec.Body.String())
	}
}

// TestMergeConsequencesSummarized: the preview reports the split count that will
// repoint and the 0-reconciliations note.
func TestMergeConsequencesSummarized(t *testing.T) {
	e := seedMergeEnv(t)

	form := url.Values{}
	form.Set("src", itoa(e.src))
	form.Set("dst", itoa(e.dst))

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/accounts/merge", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The split count (1) and the 0-reconciliations note must appear, anchored to
	// the rendered en phrases (the book user resolves to en) so the assertion is
	// about the CONSEQUENCES text, not an incidental id in a hidden input. These
	// are now CLDR-pluralized (D15): count 1 selects the singular "1 transaction
	// line", count 0 selects the plural "0 reconciliations".
	if !strings.Contains(body, "1 transaction line will move") {
		t.Errorf("preview does not surface the split count; body: %s", body)
	}
	if !strings.Contains(body, "0 reconciliations will move") {
		t.Errorf("preview does not surface the 0-reconciliations note; body: %s", body)
	}
}

// TestMergeSubCoverageSurfaced: a confirmed merge where dst's subs don't cover
// src's returns the localized ErrMergeSubsetSubs message (422/partial), and does
// NOT execute.
func TestMergeSubCoverageSurfaced(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// A second subsidiary; src maps to BOTH, dst only to root -> dst does not cover.
	subMX, err := st.CreateSubsidiary(ctx, store.CreateSubsidiaryInput{ParentID: 1, Name: "MX", BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("create sub MX: %v", err)
	}
	src, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Travel"}, Subsidiaries: []int64{1, subMX},
	})
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	dst, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Office"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}

	form := url.Values{}
	form.Set("src", itoa(src))
	form.Set("dst", itoa(dst))
	form.Set("confirm", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/merge", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("sub-coverage merge status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// The localized ErrMergeSubsetSubs message (en) must be present.
	if !strings.Contains(rec.Body.String(), "cover") && !strings.Contains(rec.Body.String(), "subsidiar") {
		t.Errorf("422 body missing the localized sub-coverage message; body: %s", rec.Body.String())
	}
	// No execution: src still active, src split not moved.
	acct, _ := st.GetAccount(context.Background(), src)
	if acct.Active == 0 {
		t.Errorf("sub-coverage merge executed (src deactivated), want no execution")
	}
}

// TestMergeCrossTypeSurfaced: a confirmed cross-type merge returns the localized
// ErrMergeCrossTypeClass message (422/partial), no execution.
func TestMergeCrossTypeSurfaced(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	rev, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD", Names: map[string]string{"en": "Contributions"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	exp, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: map[string]string{"en": "Salaries"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create expense: %v", err)
	}

	form := url.Values{}
	form.Set("src", itoa(rev))
	form.Set("dst", itoa(exp))
	form.Set("confirm", "1")

	rec := asUser(t, h, sm, book, http.MethodPost, "/accounts/merge", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-type merge status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "type") && !strings.Contains(rec.Body.String(), "tipo") {
		t.Errorf("422 body missing the localized cross-type message; body: %s", rec.Body.String())
	}
	acct, _ := st.GetAccount(context.Background(), rev)
	if acct.Active == 0 {
		t.Errorf("cross-type merge executed (src deactivated), want no execution")
	}
}

// TestMergePermissions: POST /accounts/merge is TxnWrite; a ReadOnly user is
// forbidden and anon is bounced to login.
func TestMergePermissions(t *testing.T) {
	h, st, sm := accountsApp(t)
	readOnly := mkUser(t, st, "ro", "read", false)

	form := url.Values{}
	form.Set("src", "1")
	form.Set("dst", "2")

	rec := asUser(t, h, sm, readOnly, http.MethodPost, "/accounts/merge", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("ReadOnly POST /accounts/merge: status=%d, want 403", rec.Code)
	}
	// anon on the GET form -> login.
	rec = asUser(t, h, sm, 0, http.MethodGet, "/accounts/merge", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anon GET /accounts/merge: status=%d loc=%q, want 302 -> /login", rec.Code, rec.Header().Get("Location"))
	}
}

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cuento/internal/store"
)

// p12.4 web tests: history timeline render (actor / date / per-field + split diffs
// including fund & functional-class), void two-step confirm (TxnWrite; TxnRead is
// forbidden), and duplicate (editor prefilled as a NEW unsaved entry). Driven
// through the REAL mounted router against a real migrated db (no store mocks).

// seedTxn posts a balanced 2-split expense txn (salaries debit / checking credit) in
// sub1 and returns its id, using the store directly (the editor's create path is
// p12.2's own tests). actor id 1.
func seedTxn(t *testing.T, e *txnWebEnv) int64 {
	t.Helper()
	// Attribute the write to the "txnbook" user so the timeline shows a real actor
	// name (display_name == username).
	ctx := store.WithActor(context.Background(), store.Actor{ID: e.book})
	id, err := e.st.PostTransaction(ctx, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD", Memo: "initial",
		Splits: []store.SplitInput{
			{AccountID: e.salaries, Amount: 10000, Position: 0},
			{AccountID: e.checking, Amount: -10000, Position: 1},
		},
	})
	must(t, err, "seed txn")
	return id
}

// TestHistoryTimelineRendersDiffs: the history page shows the actor, an op label, and
// after an edit the changed header field AND a split fund/class delta.
func TestHistoryTimelineRendersDiffs(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: e.book})
	id := seedTxn(t, e)

	// Read the created split ids so the edit is a per-split UPDATE (diff by id).
	live, err := e.st.TransactionSplits(ctx, id)
	must(t, err, "splits")
	var salID, chkID int64
	for _, sp := range live {
		if sp.AccountID == e.salaries {
			salID = sp.ID
		} else {
			chkID = sp.ID
		}
	}

	// Edit: change the header memo, and tag both splits with the scoped fund (so the
	// timeline shows a header memo diff AND a split fund delta). The fund is scoped to
	// sub1 with program scope progEdu; retag salaries' program to progEdu to stay in
	// scope.
	edu := e.progEdu
	if err := e.st.UpdateTransaction(ctx, id, store.PostTransactionInput{
		Date: "2025-03-01", SubsidiaryID: e.sub1, Currency: "USD", Memo: "revised memo",
		Splits: []store.SplitInput{
			{ID: &salID, AccountID: e.salaries, Amount: 10000, FundID: &e.fund, ProgramID: &edu, Position: 0},
			{ID: &chkID, AccountID: e.checking, Amount: -10000, FundID: &e.fund, Position: 1},
		},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(id)+"/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history GET status=%d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Actor display name (the write user's display_name) appears.
	if !strings.Contains(body, "txnbook") {
		t.Errorf("history missing actor name; body=%s", body)
	}
	// Op labels (created + updated) appear.
	if !strings.Contains(body, "Created") || !strings.Contains(body, "Updated") {
		t.Errorf("history missing op labels")
	}
	// The changed header memo shows the new value.
	if !strings.Contains(body, "revised memo") {
		t.Errorf("history missing header memo diff")
	}
	// The split fund delta names the fund (Beca) -- the fund label rendered from the id.
	if !strings.Contains(body, "Beca") {
		t.Errorf("history missing fund split diff (Beca)")
	}

	// p29.16: the page is now a set of SAVED-STATE cards. Assert the new structure --
	// each version is a state card (hist-version) with a full split table (hist-splits),
	// and there are TWO cards (initial create + the edit) so a reviewer sees each state.
	if got := strings.Count(body, "hist-version"); got < 2 {
		t.Errorf("want >=2 state cards (hist-version), got %d", got)
	}
	if !strings.Contains(body, "hist-splits") {
		t.Errorf("history missing the full split table (hist-splits)")
	}
	// The initial + current state sequence labels are present.
	if !strings.Contains(body, "Initial state") || !strings.Contains(body, "Current state") {
		t.Errorf("history missing initial/current state labels")
	}
	// The changed header memo is VISIBLY MARKED as changed (the field carries is-changed
	// AND the prior value is shown struck via hist-old), not just present as text.
	if !strings.Contains(body, "is-changed") {
		t.Errorf("changed field not marked (is-changed)")
	}
	// A split fund was added on the edit -> the row is marked changed with a status word.
	if !strings.Contains(body, "is-update") {
		t.Errorf("changed split row not marked (is-update)")
	}
	// The initial state card shows the FULL split set (checking split's account name).
	if !strings.Contains(body, "Checking") {
		t.Errorf("state card missing the full split set (checking account)")
	}
}

// TestHistoryVisibleAfterVoid: viewing history of a VOIDED txn does NOT 404 (the
// handler must load from version rows, not GetTransaction which 404s a soft-delete).
func TestHistoryVisibleAfterVoid(t *testing.T) {
	e := newTxnWebEnv(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id := seedTxn(t, e)
	if err := e.st.DeleteTransaction(ctx, id); err != nil {
		t.Fatalf("void: %v", err)
	}

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(id)+"/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history of voided txn status=%d, want 200 (must not 404)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Voided") {
		t.Errorf("voided txn history missing the Voided op label")
	}
}

// TestHistoryTxnReadMayView: a read-only user CAN view history (Perm TxnRead).
func TestHistoryTxnReadMayView(t *testing.T) {
	e := newTxnWebEnv(t)
	ro := mkUser(t, e.st, "txnro_hist", "read", false)
	id := seedTxn(t, e)

	rec := asUser(t, e.h, e.sm, ro, http.MethodGet, "/transactions/"+itoa(id)+"/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("TxnRead history GET status=%d, want 200", rec.Code)
	}
}

// TestVoidRequiresConfirmAndTxnWrite: a read-only user is FORBIDDEN from the void
// route (void is TxnWrite), and a write user's confirm POST voids the txn (it leaves
// the register). This is the perm-enforcement assertion the step names.
func TestVoidRequiresConfirmAndTxnWrite(t *testing.T) {
	e := newTxnWebEnv(t)
	ro := mkUser(t, e.st, "txnro_void", "read", false)
	id := seedTxn(t, e)

	// TxnRead cannot reach the void review (GET) nor execute (POST) -- 403.
	rec := asUser(t, e.h, e.sm, ro, http.MethodGet, "/transactions/"+itoa(id)+"/void", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("TxnRead GET void: status=%d, want 403", rec.Code)
	}
	f := url.Values{}
	f.Set("confirm", "1")
	rec = asUser(t, e.h, e.sm, ro, http.MethodPost, "/transactions/"+itoa(id)+"/void", f)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("TxnRead POST void: status=%d, want 403", rec.Code)
	}

	// The txn is still live (nothing voided).
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if _, err := e.st.GetTransaction(ctx, id); err != nil {
		t.Fatalf("txn should still be live after forbidden void: %v", err)
	}

	// A write user's confirm POST voids it (303 redirect).
	rec = asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(id)+"/void", f)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("write confirm void: status=%d, want 303, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := e.st.GetTransaction(ctx, id); err == nil {
		t.Fatalf("txn should be voided (GetTransaction should 404 a soft-delete)")
	}
}

// TestVoidReviewIsReadOnly: the GET review renders the summary and performs NO write
// (the txn stays live until the confirm POST).
func TestVoidReviewIsReadOnly(t *testing.T) {
	e := newTxnWebEnv(t)
	id := seedTxn(t, e)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(id)+"/void", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("void review GET: status=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Void this transaction") {
		t.Errorf("void review missing the confirm control")
	}
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if _, err := e.st.GetTransaction(ctx, id); err != nil {
		t.Fatalf("review must not void: %v", err)
	}
}

// TestVoidRequiresConfirm: a POST WITHOUT the confirm flag does NOT void (the "void
// requires confirm" safety property) -- it re-renders the review and the txn stays
// live.
func TestVoidRequiresConfirm(t *testing.T) {
	e := newTxnWebEnv(t)
	id := seedTxn(t, e)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodPost, "/transactions/"+itoa(id)+"/void", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("void POST without confirm: status=%d, want 200 (review re-render)", rec.Code)
	}
	// The txn is untouched (still live).
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	if _, err := e.st.GetTransaction(ctx, id); err != nil {
		t.Fatalf("void without confirm must not delete: %v", err)
	}
}

// TestDuplicatePrefillsNewEntry: duplicate opens the editor prefilled from the source
// splits but as a NEW entry -- the form POSTs to /transactions (create), carries no
// split ids, and echoes the source accounts.
func TestDuplicatePrefillsNewEntry(t *testing.T) {
	e := newTxnWebEnv(t)
	id := seedTxn(t, e)

	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/"+itoa(id)+"/duplicate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate GET: status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The form is a CREATE: it posts to /transactions (no id in the action).
	if !strings.Contains(body, `action="/transactions"`) && !strings.Contains(body, `hx-post="/transactions"`) {
		t.Errorf("duplicate form should POST to /transactions (create); body=%s", body)
	}
	// The source memo is copied.
	if !strings.Contains(body, "initial") {
		t.Errorf("duplicate should copy the source memo")
	}
	// The source accounts are prefilled (salaries selected in a row).
	if !strings.Contains(body, `value="`+itoa(e.salaries)+`"`) {
		t.Errorf("duplicate should prefill the source account")
	}
}

// TestDuplicateTxnWrite: a read-only user is forbidden from duplicate (TxnWrite).
func TestDuplicateTxnWrite(t *testing.T) {
	e := newTxnWebEnv(t)
	ro := mkUser(t, e.st, "txnro_dup", "read", false)
	id := seedTxn(t, e)
	rec := asUser(t, e.h, e.sm, ro, http.MethodGet, "/transactions/"+itoa(id)+"/duplicate", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("TxnRead duplicate GET: status=%d, want 403", rec.Code)
	}
}

// TestHistoryMissing404: a nonexistent txn id has no version rows -> 404.
func TestHistoryMissing404(t *testing.T) {
	e := newTxnWebEnv(t)
	rec := asUser(t, e.h, e.sm, e.book, http.MethodGet, "/transactions/99999/history", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing txn history: status=%d, want 404", rec.Code)
	}
}

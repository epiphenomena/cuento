package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// p20.3 reviewer-queue handler tests. Driven through the REAL mounted router
// (httptest) against a real migrated temp db (AGENTS testing conventions) -- no store
// mocks. They prove: a TxnWrite reviewer opens a submitted report in the p12 editor
// (subsidiary LOCKED), posts a BALANCED txn that CONVERTS the report (linked via
// posted_transaction_id) AND an UNBALANCED post -> 422 leaving the report submitted;
// reject-with-reason routes it back (review_notes=reason) and a missing reason -> 422;
// a converted report is immutable and shows its txn link; and a PURE submitter is 403
// on the whole review surface while a TxnWrite user is 200.

// seedSubmittedReport creates an expense + cash account, a submitter, and a SUBMITTED
// report with one expense line, returning the report id and account ids. The reviewer
// balances it against cash.
type reviewReportEnv struct {
	reportID  int64
	expense   int64
	cash      int64
	submitter ids.UserID
}

func seedSubmittedReport(t *testing.T, st *store.Store, amount int64) reviewReportEnv {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// An expense leaf account with a default program + functional class, so the prefilled
	// R/E row satisfies Z15/Z16 without the reviewer filling hidden gating fields.
	fc := "program"
	rootProg := ids.ProgramID(1)
	expense, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Travel", "es": "Viajes"}, Subsidiaries: []ids.SubsidiaryID{1},
		FunctionalClass: &fc, DefaultProgramID: &rootProg,
	})
	if err != nil {
		t.Fatalf("create expense account: %v", err)
	}
	cash, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Cash", "es": "Caja"}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("create cash account: %v", err)
	}

	submitter := mkSubmitter(t, st, "reviewsub")
	subCtx := store.WithActor(context.Background(), store.Actor{ID: submitter})
	reportID, err := st.CreateExpenseReport(subCtx, submitter, 1)
	if err != nil {
		t.Fatalf("create report: %v", err)
	}
	if _, err := st.AddExpenseReportLine(subCtx, ids.ExpenseReportID(reportID), store.ExpenseReportLineInput{AccountID: expense, Amount: amount, Memo: "taxi"}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	if err := st.SubmitExpenseReport(subCtx, ids.ExpenseReportID(reportID)); err != nil {
		t.Fatalf("submit report: %v", err)
	}
	return reviewReportEnv{reportID: int64(reportID), expense: expense, cash: cash, submitter: submitter}
}

// TestReviewPostCreatesBalancedTxnAndConverts: a TxnWrite reviewer opens a submitted
// report, an UNBALANCED post -> 422 (report stays submitted), then a BALANCED post
// creates a real versioned ledger txn AND converts the report (status=converted,
// posted_transaction_id set). Atomicity: a converted report always points at a valid
// posted txn.
func TestReviewPostCreatesBalancedTxnAndConverts(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "reviewer", "write", false)
	env := seedSubmittedReport(t, st, 2000) // expense +20.00

	// The editor opens prefilled with the subsidiary LOCKED.
	rec := asUser(t, h, sm, book, http.MethodGet, "/expenses/review/"+itoa(env.reportID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET review form = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/expenses/review/post/`+itoa(env.reportID)+`"`) {
		t.Error("form action is not the review-post route")
	}
	if !strings.Contains(body, `id="txn-subsidiary"`) || !strings.Contains(body, "disabled") {
		t.Error("subsidiary select is not disabled (locked)")
	}
	if !strings.Contains(body, `name="subsidiary" value="1"`) {
		t.Error("locked subsidiary hidden carrier missing")
	}
	if !strings.Contains(body, `selected>Travel</option>`) {
		t.Error("expense line not prefilled in the editor")
	}

	// UNBALANCED post -> 422; the report stays submitted.
	unbalanced := url.Values{}
	unbalanced.Set("currency", "USD")
	unbalanced.Set("date", "2025-06-01")
	unbalanced.Set("rows", "2")
	unbalanced.Set("account_0", itoa(env.expense))
	unbalanced.Set("amount_0", "20.00")
	unbalanced.Set("program_0", "1")
	unbalanced.Set("progclass_0", "p:1") // p26.41 combined control encoding
	unbalanced.Set("account_1", itoa(env.cash))
	unbalanced.Set("amount_1", "-15.00") // does not balance
	rec = asUser(t, h, sm, book, http.MethodPost, "/expenses/review/post/"+itoa(env.reportID), unbalanced)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unbalanced post = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	rep, _ := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(env.reportID))
	if rep.Status != "submitted" {
		t.Fatalf("status after unbalanced post = %q, want submitted", rep.Status)
	}
	if rep.PostedTransactionID.Valid {
		t.Fatal("posted_transaction_id set after a rejected post")
	}

	// BALANCED post -> converted + linked; the reviewer is sent to the txn history.
	balanced := url.Values{}
	balanced.Set("currency", "USD")
	balanced.Set("date", "2025-06-01")
	balanced.Set("rows", "2")
	balanced.Set("account_0", itoa(env.expense))
	balanced.Set("amount_0", "20.00")
	balanced.Set("program_0", "1")
	balanced.Set("progclass_0", "p:1") // p26.41 combined control encoding
	balanced.Set("account_1", itoa(env.cash))
	balanced.Set("amount_1", "-20.00")
	rec = asUser(t, h, sm, book, http.MethodPost, "/expenses/review/post/"+itoa(env.reportID), balanced)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("balanced post = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	rep, _ = st.GetExpenseReport(context.Background(), ids.ExpenseReportID(env.reportID))
	if rep.Status != "converted" {
		t.Fatalf("status after balanced post = %q, want converted", rep.Status)
	}
	if !rep.PostedTransactionID.Valid {
		t.Fatal("posted_transaction_id not set after convert")
	}
	txnID := rep.PostedTransactionID.Int64
	// The redirect points at the created txn's history.
	if loc := rec.Header().Get("Location"); loc != "/transactions/"+itoa(txnID)+"/history" {
		t.Errorf("redirect = %q, want the txn history", loc)
	}
	// The created txn is a real ledger entry (appears in the cash register).
	regRows, _, _, err := st.RegisterPage(context.Background(), env.cash, store.RegisterCursor{}, store.RegisterFilters{}, 50)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	seen := false
	for _, e := range regRows {
		if e.TxnID == txnID {
			seen = true
		}
	}
	if !seen {
		t.Error("converted txn not in the cash register")
	}

	// The queue shows the converted report with a txn link (immutable).
	q := asUser(t, h, sm, book, http.MethodGet, "/expenses/review", nil)
	if !strings.Contains(q.Body.String(), "/transactions/"+itoa(txnID)+"/history") {
		t.Error("queue does not link the converted report's txn")
	}
}

// TestReviewPostImmutableConverted: re-posting a CONVERTED report is rejected (the store
// re-read gate) -> redirect to the queue, no second txn.
func TestReviewPostImmutableConverted(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "reviewer2", "write", false)
	env := seedSubmittedReport(t, st, 2000)

	post := func() { // a balanced post
		form := url.Values{}
		form.Set("currency", "USD")
		form.Set("date", "2025-06-01")
		form.Set("rows", "2")
		form.Set("account_0", itoa(env.expense))
		form.Set("amount_0", "20.00")
		form.Set("program_0", "1")
		form.Set("progclass_0", "p:1") // p26.41 combined control encoding
		form.Set("account_1", itoa(env.cash))
		form.Set("amount_1", "-20.00")
		asUser(t, h, sm, book, http.MethodPost, "/expenses/review/post/"+itoa(env.reportID), form)
	}
	post() // converts
	rep, _ := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(env.reportID))
	firstTxn := rep.PostedTransactionID.Int64

	// A GET of the review form for a converted report redirects to the queue.
	rec := asUser(t, h, sm, book, http.MethodGet, "/expenses/review/"+itoa(env.reportID), nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/expenses/review" {
		t.Errorf("GET review form on converted report = %d (loc %q), want 303 to queue", rec.Code, rec.Header().Get("Location"))
	}

	// A re-post leaves the SAME txn linked (no double post).
	post()
	rep, _ = st.GetExpenseReport(context.Background(), ids.ExpenseReportID(env.reportID))
	if rep.Status != "converted" || rep.PostedTransactionID.Int64 != firstTxn {
		t.Errorf("re-post changed the linked txn: status=%q txn=%d, want converted/%d", rep.Status, rep.PostedTransactionID.Int64, firstTxn)
	}
}

// TestReviewRejectRoutesBack: reject with a reason -> status=rejected, review_notes=
// reason; a missing (blank) reason -> 422, report stays submitted.
func TestReviewRejectRoutesBack(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "reviewer3", "write", false)
	env := seedSubmittedReport(t, st, 2000)

	// Missing (whitespace-only) reason -> 422; the report stays submitted. p26.27: the
	// 422 re-renders the review PAGE (the prefilled editor + the reject form), NOT the
	// queue -- so the body carries the txn-form + reject-form markers, and the localized
	// reject-reason error.
	blank := url.Values{}
	blank.Set("reason", "   ")
	rec := asUser(t, h, sm, book, http.MethodPost, "/expenses/review/reject/"+itoa(env.reportID), blank)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank-reason reject = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "expreview.reject_reason") {
		t.Error("raw i18n key leaked (not localized)")
	}
	// The 422 is the REVIEW PAGE, not the queue: it has the review editor form + the
	// reject form, and the localized reject-reason error.
	if !strings.Contains(body, `id="txn-form"`) || !strings.Contains(body, "expreview-reject-form") {
		t.Errorf("blank-reason reject 422 did not re-render the review page (missing txn-form / reject-form); body: %s", body)
	}
	if !strings.Contains(body, "expreview-reject-error") {
		t.Errorf("blank-reason reject 422 missing the reject-reason error slot; body: %s", body)
	}
	rep, _ := st.GetExpenseReport(context.Background(), ids.ExpenseReportID(env.reportID))
	if rep.Status != "submitted" {
		t.Fatalf("status after blank-reason reject = %q, want submitted", rep.Status)
	}

	// A real reason -> rejected, notes stored, routed back to the submitter.
	const reason = "Please attach the receipt."
	ok := url.Values{}
	ok.Set("reason", reason)
	rec = asUser(t, h, sm, book, http.MethodPost, "/expenses/review/reject/"+itoa(env.reportID), ok)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reject-with-reason = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	rep, _ = st.GetExpenseReport(context.Background(), ids.ExpenseReportID(env.reportID))
	if rep.Status != "rejected" {
		t.Errorf("status after reject = %q, want rejected", rep.Status)
	}
	if rep.ReviewNotes != reason {
		t.Errorf("review_notes = %q, want %q", rep.ReviewNotes, reason)
	}
}

// TestReviewPermSubmitterForbidden: a PURE submitter (ExpenseSubmit only, txn_perm=none)
// is 403 on the whole review surface (queue, form, post, reject); a TxnWrite user is 200
// on the queue. The two roles are distinct.
func TestReviewPermSubmitterForbidden(t *testing.T) {
	h, st, sm := accountsApp(t)
	submitter := mkSubmitter(t, st, "puresub")
	writer := mkUser(t, st, "editor", "write", false)
	env := seedSubmittedReport(t, st, 2000)

	rp := "/expenses/review/" + itoa(env.reportID)
	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/expenses/review", nil},
		{http.MethodGet, rp, nil},
		{http.MethodPost, "/expenses/review/post/" + itoa(env.reportID), url.Values{"currency": {"USD"}, "rows": {"0"}}},
		{http.MethodPost, "/expenses/review/reject/" + itoa(env.reportID), url.Values{"reason": {"x"}}},
	} {
		if rec := asUser(t, h, sm, submitter, tc.method, tc.path, tc.form); rec.Code != http.StatusForbidden {
			t.Errorf("submitter %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	// A TxnWrite user reaches the queue (200).
	if rec := asUser(t, h, sm, writer, http.MethodGet, "/expenses/review", nil); rec.Code != http.StatusOK {
		t.Errorf("TxnWrite GET queue = %d, want 200", rec.Code)
	}
}

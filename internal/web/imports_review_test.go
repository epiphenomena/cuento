package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"cuento/internal/bankimport"
	"cuento/internal/ids"
	"cuento/internal/store"
)

// p17.3 web tests: the review queue -> post flow. The "edit & post" editor opens
// prefilled from a staged row with the batch's subsidiary LOCKED; posting creates a
// balanced ledger transaction and LINKS the row (visible in the register); discard
// requires a reason (empty -> 422 + i18n key); the actions are TxnWrite (a TxnRead
// user 403s). They stage a batch through the store (simpler than driving the full
// upload) and drive the real router.

// stageReviewBatch creates an asset (checking) + expense chart, stages ONE pending row
// on checking, and returns the store, the row id, the batch id, and the account ids.
type reviewEnv struct {
	st       *store.Store
	rowID    ids.ImportRowID
	batchID  ids.ImportBatchID
	checking ids.AccountID
	expense  ids.AccountID
}

func stageReviewBatch(t *testing.T, st *store.Store, description, memo string) reviewEnv {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	checking, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Checking", "es": "Checking"}, Subsidiaries: []ids.SubsidiaryID{1},
	})
	if err != nil {
		t.Fatalf("CreateAccount checking: %v", err)
	}
	fc := "program"
	rootProg := ids.ProgramID(1)
	expense, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Supplies", "es": "Suministros"}, Subsidiaries: []ids.SubsidiaryID{1},
		FunctionalClass: &fc, DefaultProgramID: &rootProg,
	})
	if err != nil {
		t.Fatalf("CreateAccount expense: %v", err)
	}

	profile, err := st.CreateMappingProfile(ctx, "bank", bankimport.Config{
		Delimiter: bankimport.DelimiterComma, HasHeader: true, Amount: bankimport.AmountSingle,
		DateFmt: bankimport.DateISO, DateCol: 0, AmountCol: 1, DescCol: 2, MemoCol: 3,
	})
	if err != nil {
		t.Fatalf("CreateMappingProfile: %v", err)
	}
	batch, err := st.CreateImportBatch(ctx, "jan.csv", checking, 1, profile, "2025-02-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CreateImportBatch: %v", err)
	}
	staged, err := st.StageImportRows(ctx, batch, checking, []bankimport.ParsedRow{
		{Date: "2025-01-15", AmountMinor: -4200, Description: description, Memo: memo, Raw: []string{"2025-01-15", "-42.00", description, memo}},
	})
	if err != nil {
		t.Fatalf("StageImportRows: %v", err)
	}
	return reviewEnv{st: st, rowID: staged[0].ID, batchID: batch, checking: checking, expense: expense}
}

// TestImportEditPostPrefillLocksSubsidiary: the edit&post editor opens prefilled from
// the staged row with the batch account line and the subsidiary field LOCKED
// (disabled + a hidden carrier), posting to /import/rows/{id}/post.
func TestImportEditPostPrefillLocksSubsidiary(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	env := stageReviewBatch(t, st, "Acme", "Invoice")

	rec := asUser(t, h, sm, book, http.MethodGet, "/import/rows/"+strconv.FormatInt(int64(env.rowID), 10)+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edit = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The form posts to the import-post route (edit&post mode).
	if !strings.Contains(body, `action="/import/rows/`+strconv.FormatInt(int64(env.rowID), 10)+`/post"`) {
		t.Error("form action is not the import-post route")
	}
	// The subsidiary select is LOCKED (disabled) with a hidden carrier.
	if !strings.Contains(body, `id="txn-subsidiary"`) || !strings.Contains(body, "disabled") {
		t.Error("subsidiary select is not disabled (locked)")
	}
	if !strings.Contains(body, `name="subsidiary" value="1"`) {
		t.Error("locked subsidiary hidden carrier missing")
	}
	// The batch account line is prefilled (the checking account is selected in row 0).
	if !strings.Contains(body, `selected>Asset.Checking</option>`) {
		t.Error("batch account line not prefilled in row 0")
	}
}

// TestImportEditPostSeedsBankLineDescription: the edit&post editor seeds the bank
// (batch-account) row's Description from the staged row's descriptive text (the former
// "payee" line). This is the dedupe-hash source, so a posted bank split re-hashes to
// its staging key (p26.20). The payee entity is retired, so there is no counter-split
// template -- the reviewer authors the counter side (one empty counter row).
func TestImportEditPostSeedsBankLineDescription(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	env := stageReviewBatch(t, st, "Acme Supplies Co", "Invoice")

	rec := asUser(t, h, sm, book, http.MethodGet, "/import/rows/"+strconv.FormatInt(int64(env.rowID), 10)+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edit = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The bank line's description input carries the staged descriptive text.
	if !strings.Contains(body, `value="Acme Supplies Co"`) {
		t.Errorf("bank line description not seeded from the staged row; body:\n%s", body)
	}
}

// TestImportRowPostCreatesTxnAndLinks: posting a balanced transaction from the editor
// links the row and the created txn appears in the account register.
func TestImportRowPostCreatesTxnAndLinks(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	env := stageReviewBatch(t, st, "Acme", "Invoice")

	form := url.Values{}
	form.Set("currency", "USD")
	form.Set("date", "2025-01-15")
	form.Set("memo", "Invoice")
	form.Set("rows", "2")
	// Row 0: the bank line (checking, -42.00). Row 1: the expense counter (+42.00,
	// class program). subsidiary is locked -> ignored server-side (uses the batch's).
	form.Set("account_0", strconv.FormatInt(int64(env.checking), 10))
	form.Set("amount_0", "-42.00")
	form.Set("account_1", strconv.FormatInt(int64(env.expense), 10))
	form.Set("amount_1", "42.00")
	form.Set("progclass_1", "p:1") // p26.41 combined control: program node (root) -> class program on expense

	rec := asUser(t, h, sm, book, http.MethodPost, "/import/rows/"+strconv.FormatInt(int64(env.rowID), 10)+"/post", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST post = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/import/batches/"+strconv.FormatInt(int64(env.batchID), 10) {
		t.Errorf("redirect Location = %q, want the batch queue", loc)
	}

	// The row is linked: status=posted with a posted_transaction_id.
	row, err := st.GetImportRow(context.Background(), env.rowID)
	if err != nil {
		t.Fatalf("GetImportRow: %v", err)
	}
	if row.Status != "posted" || row.PostedTxnID == nil {
		t.Fatalf("row after post: status=%q posted=%v, want posted/linked", row.Status, row.PostedTxnID)
	}

	// The created txn is in the checking register.
	regRows, _, _, err := st.RegisterPage(context.Background(), env.checking, store.RegisterCursor{}, store.RegisterFilters{}, 50)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	seen := false
	for _, e := range regRows {
		if e.TxnID == *row.PostedTxnID {
			seen = true
		}
	}
	if !seen {
		t.Error("posted transaction not in the checking register")
	}

	// The batch queue shows the row posted.
	q := asUser(t, h, sm, book, http.MethodGet, "/import/batches/"+strconv.FormatInt(int64(env.batchID), 10), nil)
	if q.Code != http.StatusOK {
		t.Fatalf("GET queue = %d, want 200", q.Code)
	}
	if !strings.Contains(q.Body.String(), "import-row-posted") {
		t.Error("queue does not show the row as posted")
	}
}

// TestImportRowPostUnbalancedRerenders422: an unbalanced post re-renders the editor at
// 422 with the imbalance error (the store is the sole validator).
func TestImportRowPostUnbalancedRerenders422(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	env := stageReviewBatch(t, st, "Acme", "Invoice")

	form := url.Values{}
	form.Set("currency", "USD")
	form.Set("date", "2025-01-15")
	form.Set("rows", "2")
	form.Set("account_0", strconv.FormatInt(int64(env.checking), 10))
	form.Set("amount_0", "-42.00")
	form.Set("account_1", strconv.FormatInt(int64(env.expense), 10))
	form.Set("amount_1", "40.00")  // does not balance
	form.Set("progclass_1", "p:1") // p26.41 combined control: program node (root) -> class program on expense

	rec := asUser(t, h, sm, book, http.MethodPost, "/import/rows/"+strconv.FormatInt(int64(env.rowID), 10)+"/post", form)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unbalanced POST = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// The row stays pending (nothing linked).
	row, _ := st.GetImportRow(context.Background(), env.rowID)
	if row.Status != "pending" {
		t.Errorf("row status = %q after rejected post, want pending", row.Status)
	}
}

// TestImportDiscardRequiresReason: discard with no reason -> 422 + the discard-reason
// i18n key; with a reason -> the row is discarded.
func TestImportDiscardRequiresReason(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	env := stageReviewBatch(t, st, "Acme", "Invoice")

	// Empty reason -> 422, row still pending.
	empty := url.Values{}
	empty.Set("reason", "  ")
	rec := asUser(t, h, sm, book, http.MethodPost, "/import/rows/"+strconv.FormatInt(int64(env.rowID), 10)+"/discard", empty)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty-reason discard = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reason") && !strings.Contains(rec.Body.String(), "requiere") && !strings.Contains(rec.Body.String(), "required") {
		// The localized discard-reason error is rendered (en: "...required...").
		t.Error("422 body does not surface the discard-reason error")
	}
	row, _ := st.GetImportRow(context.Background(), env.rowID)
	if row.Status != "pending" {
		t.Fatalf("row status = %q after empty-reason discard, want pending", row.Status)
	}

	// With a reason -> discarded.
	ok := url.Values{}
	ok.Set("reason", "not our account")
	rec = asUser(t, h, sm, book, http.MethodPost, "/import/rows/"+strconv.FormatInt(int64(env.rowID), 10)+"/discard", ok)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("discard-with-reason = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	row, _ = st.GetImportRow(context.Background(), env.rowID)
	if row.Status != "discarded" {
		t.Errorf("row status = %q, want discarded", row.Status)
	}
}

// TestImportReviewPermTxnReadForbidden: a TxnRead user cannot view the queue, open the
// editor, post, or discard (all TxnWrite); a TxnWrite user can view/act.
func TestImportReviewPermTxnReadForbidden(t *testing.T) {
	h, st, sm := accountsApp(t)
	reader := mkUser(t, st, "viewer", "read", false)
	writer := mkUser(t, st, "writer", "write", false)
	env := stageReviewBatch(t, st, "Acme", "Invoice")

	rowPath := "/import/rows/" + strconv.FormatInt(int64(env.rowID), 10)
	batchPath := "/import/batches/" + strconv.FormatInt(int64(env.batchID), 10)

	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, batchPath, nil},
		{http.MethodGet, rowPath + "/edit", nil},
		{http.MethodPost, rowPath + "/post", url.Values{"currency": {"USD"}, "rows": {"0"}}},
		{http.MethodPost, rowPath + "/discard", url.Values{"reason": {"x"}}},
	} {
		if rec := asUser(t, h, sm, reader, tc.method, tc.path, tc.form); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as TxnRead = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// The writer CAN view the queue.
	if rec := asUser(t, h, sm, writer, http.MethodGet, batchPath, nil); rec.Code != http.StatusOK {
		t.Errorf("GET queue as TxnWrite = %d, want 200", rec.Code)
	}
}

package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/bankimport"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// importApp is accountsApp but also returns the raw db so the tests can assert
// directly on the import_batches / import_rows tables.
func importApp(t *testing.T) (http.Handler, *store.Store, *scs.SessionManager, *sql.DB) {
	t.Helper()
	db := testutil.NewDB(t)
	st := store.New(db)
	app := NewApp(Config{Version: "test"}, db, st)
	return app.handler, st, app.sessions, db
}

// p17.2 web tests: the upload -> preview -> stage flow (happy path with a >20-row
// CSV so the 20-cap is proven), a bad mapping -> clean 422 with no batch created,
// and the explicit perm gate (TxnRead cannot upload/stage; TxnWrite can). They reuse
// accountsApp / mkUser / mintCookie.

// importFields is the mapping + target the import form carries. The defaults map a
// date,amount,desc,memo CSV with a header, comma delimiter, single signed amount.
func importFields(accountID, subsidiaryID int64) map[string]string {
	return map[string]string{
		"account_id":    strconv.FormatInt(accountID, 10),
		"subsidiary_id": strconv.FormatInt(subsidiaryID, 10),
		"delimiter":     ",",
		"has_header":    "1",
		"amount_mode":   "single",
		"date_format":   "ISO",
		"date_col":      "0",
		"amount_col":    "1",
		"desc_col":      "2",
		"memo_col":      "3",
	}
}

// uploadImportCSV posts the multipart preview form (CSV + mapping fields) to
// /import/preview as userID.
func uploadImportCSV(t *testing.T, h http.Handler, sm *scs.SessionManager, userID int64, csv string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	fw, err := mw.CreateFormFile("file", "statement.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(csv)); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/import/preview", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(mintCookie(t, sm, userID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// importChart creates an asset account mapped to the root subsidiary (id 1) and
// returns its id.
func importChart(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	id, err := st.CreateAccount(ctx, store.CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Checking"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return id
}

// bigCSV builds a header + n data rows of date,amount,payee,memo.
func bigCSV(n int) string {
	var b strings.Builder
	b.WriteString("date,amount,payee,memo\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "2025-01-%02d,%d.00,Payee %d,Memo %d\n", (i%28)+1, i+1, i, i)
	}
	return b.String()
}

// TestImportPreviewCapsAt20AndStagesAll: a >20-row CSV previews exactly 20 rows,
// and confirming stages ALL rows into a real batch.
func TestImportPreviewCapsAt20AndStagesAll(t *testing.T) {
	h, st, sm, db := importApp(t)
	book := mkUser(t, st, "book", "write", false)
	acct := importChart(t, st)

	const total = 25
	rec := uploadImportCSV(t, h, sm, book, bigCSV(total), importFields(acct, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The preview shows exactly 20 parsed rows (the DISPLAY cap).
	shown := strings.Count(body, `class="import-preview-row"`)
	if shown != importPreviewCap {
		t.Fatalf("preview showed %d rows, want %d", shown, importPreviewCap)
	}

	// Extract the carried base64 CSV to drive the confirm step.
	csvB64 := extractHidden(t, body, "csv_b64")
	if csvB64 == "" {
		t.Fatal("preview did not carry the CSV base64 forward")
	}

	// Confirm: stage all rows.
	form := url.Values{}
	for k, v := range importFields(acct, 1) {
		form.Set(k, v)
	}
	form.Set("csv_b64", csvB64)
	form.Set("filename", "statement.csv")
	rec2 := asUser(t, h, sm, book, http.MethodPost, "/import", form)
	if rec2.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200; body: %s", rec2.Code, rec2.Body.String())
	}

	// A batch exists and holds ALL 25 rows.
	var batchID int64
	if err := db.QueryRow(`SELECT id FROM import_batches ORDER BY id DESC LIMIT 1`).Scan(&batchID); err != nil {
		t.Fatalf("find batch: %v", err)
	}
	rows, err := st.ImportRowsForBatch(context.Background(), batchID)
	if err != nil {
		t.Fatalf("ImportRowsForBatch: %v", err)
	}
	if len(rows) != total {
		t.Fatalf("staged %d rows, want %d (all rows stage, only preview caps)", len(rows), total)
	}
}

// TestImportDuplicateFlaggedInResult: a CSV listing the same line twice stages both
// but flags the second as a duplicate (within-batch), shown in the result.
func TestImportDuplicateFlaggedInResult(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	acct := importChart(t, st)

	csv := "date,amount,payee,memo\n2025-01-15,100.00,Acme,Invoice\n2025-01-15,100.00,Acme,Invoice\n"
	rec := uploadImportCSV(t, h, sm, book, csv, importFields(acct, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	csvB64 := extractHidden(t, rec.Body.String(), "csv_b64")

	form := url.Values{}
	for k, v := range importFields(acct, 1) {
		form.Set(k, v)
	}
	form.Set("csv_b64", csvB64)
	form.Set("filename", "statement.csv")
	rec2 := asUser(t, h, sm, book, http.MethodPost, "/import", form)
	if rec2.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200; body: %s", rec2.Code, rec2.Body.String())
	}
	if n := strings.Count(rec2.Body.String(), "import-row-duplicate"); n != 1 {
		t.Errorf("duplicate rows flagged = %d, want 1 (second identical line)", n)
	}
}

// TestImportProfileReuse: a saved mapping profile drives the parse when selected,
// even if the inline mapping fields are blank/wrong -- proving the reuse flow (not
// just save).
func TestImportProfileReuse(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)
	acct := importChart(t, st)

	// Save a profile with the CORRECT mapping (date=0, amount=1, payee=2, memo=3).
	pid, err := st.CreateMappingProfile(
		store.WithActor(context.Background(), store.Actor{ID: 1}),
		"bank", bankimport.Config{
			Delimiter: bankimport.DelimiterComma, HasHeader: true, Amount: bankimport.AmountSingle,
			DateFmt: bankimport.DateISO, DateCol: 0, AmountCol: 1, DescCol: 2, MemoCol: 3,
		},
	)
	if err != nil {
		t.Fatalf("CreateMappingProfile: %v", err)
	}

	// Submit with a DELIBERATELY WRONG inline mapping (amount_col points at payee),
	// but SELECT the good profile -> the profile's config wins and the parse succeeds.
	fields := importFields(acct, 1)
	fields["amount_col"] = "2" // wrong on purpose
	fields["profile_id"] = strconv.FormatInt(pid, 10)

	csv := "date,amount,payee,memo\n2025-01-15,100.00,Acme,Invoice\n"
	rec := uploadImportCSV(t, h, sm, book, csv, fields)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview with profile = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), `class="import-preview-row"`); n != 1 {
		t.Fatalf("preview rows = %d, want 1 (profile config drove the parse)", n)
	}
}

// TestImportProfileDelete: a saved profile shows in the upload page's load list and
// manage section; POSTing the delete route soft-deletes it (303), and it is gone from
// the page afterward. A missing profile id is a 404.
func TestImportProfileDelete(t *testing.T) {
	h, st, sm := accountsApp(t)
	book := mkUser(t, st, "book", "write", false)

	pid, err := st.CreateMappingProfile(
		store.WithActor(context.Background(), store.Actor{ID: 1}),
		"deletable", bankimport.Config{
			Delimiter: bankimport.DelimiterComma, HasHeader: true, Amount: bankimport.AmountSingle,
			DateFmt: bankimport.DateISO, DateCol: 0, AmountCol: 1, DescCol: 2, MemoCol: 3,
		},
	)
	if err != nil {
		t.Fatalf("CreateMappingProfile: %v", err)
	}

	// The upload page lists the profile (load option + manage-delete control).
	page := asUser(t, h, sm, book, http.MethodGet, "/import", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "deletable") {
		t.Fatalf("upload page missing the saved profile; code=%d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "/import/profiles/"+strconv.FormatInt(pid, 10)+"/delete") {
		t.Fatalf("upload page missing the delete control for profile %d", pid)
	}

	// Delete it: NO-JS form POST -> 303 back to /import.
	del := asUser(t, h, sm, book, http.MethodPost, "/import/profiles/"+strconv.FormatInt(pid, 10)+"/delete", url.Values{})
	if del.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303; body: %s", del.Code, del.Body.String())
	}

	// Gone from the page.
	after := asUser(t, h, sm, book, http.MethodGet, "/import", nil)
	if strings.Contains(after.Body.String(), "deletable") {
		t.Fatalf("deleted profile still shows on the upload page")
	}

	// A missing/already-gone id is a clean 404.
	miss := asUser(t, h, sm, book, http.MethodPost, "/import/profiles/"+strconv.FormatInt(pid, 10)+"/delete", url.Values{})
	if miss.Code != http.StatusNotFound {
		t.Fatalf("re-delete = %d, want 404", miss.Code)
	}
}

// TestImportBadMappingIs422NoBatch: a mapping pointing the amount column at a
// non-numeric column (payee) makes every row fail to parse -> a clean 422 at the
// PREVIEW step, and NO batch is created.
func TestImportBadMappingIs422NoBatch(t *testing.T) {
	h, st, sm, db := importApp(t)
	book := mkUser(t, st, "book", "write", false)
	acct := importChart(t, st)

	fields := importFields(acct, 1)
	fields["amount_col"] = "2" // points at the payee column -> "Acme" is not money

	csv := "date,amount,payee,memo\n2025-01-15,100.00,Acme,Invoice\n"
	rec := uploadImportCSV(t, h, sm, book, csv, fields)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad mapping = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM import_batches`).Scan(&n); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	if n != 0 {
		t.Fatalf("a bad-mapping preview created %d batches; want 0", n)
	}
}

// TestImportPermTxnReadForbidden: a TxnRead user cannot reach the upload form, the
// preview, or the stage route (403); a TxnWrite user can GET the form (200).
func TestImportPermTxnReadForbidden(t *testing.T) {
	h, st, sm := accountsApp(t)
	reader := mkUser(t, st, "viewer", "read", false)
	writer := mkUser(t, st, "writer", "write", false)
	acct := importChart(t, st)

	// GET /import: reader 403, writer 200.
	if rec := asUser(t, h, sm, reader, http.MethodGet, "/import", nil); rec.Code != http.StatusForbidden {
		t.Errorf("GET /import as TxnRead = %d, want 403", rec.Code)
	}
	if rec := asUser(t, h, sm, writer, http.MethodGet, "/import", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /import as TxnWrite = %d, want 200", rec.Code)
	}

	// POST /import/preview as reader: 403 (no parse even attempted).
	rec := uploadImportCSV(t, h, sm, reader, bigCSV(3), importFields(acct, 1))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /import/preview as TxnRead = %d, want 403", rec.Code)
	}

	// POST /import (stage) as reader: 403.
	form := url.Values{}
	for k, v := range importFields(acct, 1) {
		form.Set(k, v)
	}
	form.Set("csv_b64", "")
	if rec := asUser(t, h, sm, reader, http.MethodPost, "/import", form); rec.Code != http.StatusForbidden {
		t.Errorf("POST /import as TxnRead = %d, want 403", rec.Code)
	}
}

// extractHidden pulls the value of a hidden input by name out of rendered HTML. It
// is a crude but adequate scan for the test's needs (the value has no embedded
// quote).
func extractHidden(t *testing.T, html, name string) string {
	t.Helper()
	marker := `name="` + name + `" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		return ""
	}
	rest := html[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

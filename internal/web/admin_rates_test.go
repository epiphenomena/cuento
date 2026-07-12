package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexedwards/scs/v2"

	"cuento/internal/store"
)

// uploadCSV posts a multipart form with the CSV body as field "file" to /admin/rates
// as userID, returning the recorder. It mirrors asUser but for a multipart upload
// (the first in the app) -- the CSV import is only reachable this way. No Origin
// header is set, so the stdlib cross-origin protection treats it as same-origin (a
// real browser upload from the app's own page).
func uploadCSV(t *testing.T, h http.Handler, sm *scs.SessionManager, userID int64, csv string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "rates.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(csv)); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/rates", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(mintCookie(t, sm, userID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRatesCSVImport: a valid CSV imports every row (verified via RateOn); a bad
// header and a bad data row are each a 422 with NO partial write.
func TestRatesCSVImport(t *testing.T) {
	h, st, sm := accountsApp(t)
	admin := mkUser(t, st, "boss", "none", true)
	ctx := context.Background()

	// --- valid file: two rows over seeded currencies (USD/MXN/EUR) import cleanly.
	good := "rate_date,base,quote,rate,source\n" +
		"2025-01-01,USD,MXN,17.05,manual\n" +
		"2025-01-01,usd,eur,0.92,manual\n" // lowercase codes are accepted (uppercased)

	rec := uploadCSV(t, h, sm, admin, good)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid import = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	mxn, err := st.RateOn(store.WithActor(ctx, store.Actor{ID: 1}), "USD", "MXN", "2025-01-01")
	if err != nil {
		t.Fatalf("RateOn USD->MXN: %v", err)
	}
	if mxn.Rate != 17.05 {
		t.Errorf("USD->MXN = %v, want 17.05", mxn.Rate)
	}
	eur, err := st.RateOn(store.WithActor(ctx, store.Actor{ID: 1}), "USD", "EUR", "2025-01-01")
	if err != nil {
		t.Fatalf("RateOn USD->EUR: %v", err)
	}
	if eur.Rate != 0.92 {
		t.Errorf("USD->EUR = %v, want 0.92", eur.Rate)
	}

	// --- bad header: 422, and NOTHING written (a fresh pair stays missing).
	badHeader := "date,base,quote,rate,source\n2025-02-01,USD,MXN,18.00,manual\n"
	rec = uploadCSV(t, h, sm, admin, badHeader)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad header = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// The only USD->MXN row on-or-before 2025-02-01 must be the earlier valid
	// import (2025-01-01), proving the bad-header file wrote nothing new.
	if r, _ := st.RateOn(store.WithActor(ctx, store.Actor{ID: 1}), "USD", "MXN", "2025-02-01"); r.RateDate == "2025-02-01" {
		t.Error("bad-header import wrote the 2025-02-01 row (want nothing)")
	}

	// --- bad row (unknown currency): 422 and NO partial write. The first (valid)
	// row of this file must NOT be written -- the import is all-or-nothing.
	badRow := "rate_date,base,quote,rate,source\n" +
		"2025-03-01,USD,MXN,19.00,manual\n" +
		"2025-03-01,USD,ZZZ,1.00,manual\n"
	rec = uploadCSV(t, h, sm, admin, badRow)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad row = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// The valid FIRST row must not have leaked in despite the later bad row.
	if r, _ := st.RateOn(store.WithActor(ctx, store.Actor{ID: 1}), "USD", "MXN", "2025-03-01"); r.RateDate == "2025-03-01" {
		t.Error("bad-row import wrote the valid first row (partial write; want all-or-nothing)")
	}
}

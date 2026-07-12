package web

import (
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"cuento/internal/store"
)

// p14.2 admin: manual/backfill exchange-rate CSV upload (/admin/rates, Perm Admin).
// This is the human-driven counterpart to `cuento ratesync` (which auto-fetches from
// Yahoo): an admin uploads a CSV of rates for days the fetch missed or that must be
// corrected. It is the FIRST multipart upload in the app (stdlib r.FormFile).
//
// The whole file is validated BEFORE any write: a bad header or ANY bad row aborts
// with a 422 (naming the offending line) and writes NOTHING -- PutRates gets the
// full batch or nothing, so a partial import can never leave the books half-loaded.
// A valid file imports all rows as ONE change and shows an imported-count summary.
// Every string via {{t}} (rule 9); no inline script (rule 12).

// ratesCSVHeader is the exact, required header row. Order is fixed so a mis-columned
// file is rejected rather than silently mapping the wrong field.
var ratesCSVHeader = []string{"rate_date", "base", "quote", "rate", "source"}

// maxRatesUpload caps the multipart parse to keep a huge upload from exhausting
// memory. Rate CSVs are tiny (a handful of pairs per day); 1 MiB is generous.
const maxRatesUpload = 1 << 20

// ratesPageModel is the GET /admin/rates model: the upload form plus a post-import
// result (imported count) or a form error key. Result/error are mutually exclusive.
type ratesPageModel struct {
	Imported int    // >0 after a successful import (shows the count summary)
	Done     bool   // true after a successful import (distinguishes 0-row from no-op)
	ErrorKey string // i18n key of an upload error (bad header / bad row / no file)
	ErrorArg string // an argument for the error message (e.g. the offending line)
}

// ratesPage handles GET /admin/rates (Admin): the upload form.
func (s *server) ratesPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "admin_rates.tmpl", s.newShellPage(r, ratesPageModel{}))
}

// ratesImport handles POST /admin/rates (Admin): parse the uploaded CSV, validate
// every row against known currencies, and PutRates the whole batch as one change. A
// bad header/row or a missing file re-renders the page at 422 with the error; a
// valid file redirects-free re-renders with the imported count.
func (s *server) ratesImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxRatesUpload); err != nil {
		s.renderRatesError(w, r, "admin.rates.error.no_file", "")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.renderRatesError(w, r, "admin.rates.error.no_file", "")
		return
	}
	defer func() { _ = file.Close() }()

	known, err := s.knownCurrencies(r)
	if err != nil {
		s.serverError(w)
		return
	}

	rows, cerr := parseRatesCSV(file, known)
	if cerr != nil {
		s.renderRatesError(w, r, cerr.key, cerr.arg)
		return
	}

	// PutRates goes through the write funnel (one change for the batch), which needs
	// the acting admin bound as the actor (rule 2/5).
	if err := s.store.PutRates(s.actorCtx(r.Context()), rows); err != nil {
		s.serverError(w)
		return
	}

	s.render(w, r, http.StatusOK, "admin_rates.tmpl",
		s.newShellPage(r, ratesPageModel{Imported: len(rows), Done: true}))
}

// knownCurrencies returns the set of currency codes that exist (active or not) so a
// backfill can reference a since-disabled currency's code (its history is valid).
func (s *server) knownCurrencies(r *http.Request) (map[string]bool, error) {
	curs, err := s.store.Currencies(r.Context())
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(curs))
	for _, c := range curs {
		known[c.Code] = true
	}
	return known, nil
}

// csvError carries a validation failure as an i18n KEY plus an optional argument
// (the offending line number) for the message. It is not an error the user sees
// verbatim (rule 9); the handler renders key+arg via {{t}}.
type csvError struct {
	key string
	arg string
}

// parseRatesCSV reads the whole CSV, validates the header and every row, and returns
// the full []store.Rate batch -- or a csvError on the FIRST problem, having parsed
// nothing into the store. Validation per row: exactly 5 fields; rate_date is
// YYYY-MM-DD; base/quote are known currency codes (uppercased); rate parses as a
// float; source non-empty. Returning all-or-nothing is what makes the import atomic
// upstream (PutRates writes the whole slice as one change).
func parseRatesCSV(f io.Reader, known map[string]bool) ([]store.Rate, *csvError) {
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = len(ratesCSVHeader) // reject rows with the wrong column count
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, &csvError{key: "admin.rates.error.bad_header"}
	}
	if !sameHeader(header) {
		return nil, &csvError{key: "admin.rates.error.bad_header"}
	}

	var out []store.Rate
	line := 1 // header was line 1; data rows start at 2
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			// Wrong column count or malformed quoting -> point at the line.
			return nil, &csvError{key: "admin.rates.error.bad_row", arg: strconv.Itoa(line)}
		}
		rate, rerr := parseRateRow(rec, known)
		if rerr != "" {
			return nil, &csvError{key: rerr, arg: strconv.Itoa(line)}
		}
		out = append(out, rate)
	}
	if len(out) == 0 {
		return nil, &csvError{key: "admin.rates.error.empty"}
	}
	return out, nil
}

// parseRateRow validates one data record and returns the store.Rate, or an i18n
// error KEY (empty string == valid). Codes are uppercased before the known-currency
// check so "usd" and "USD" both resolve.
func parseRateRow(rec []string, known map[string]bool) (store.Rate, string) {
	date := strings.TrimSpace(rec[0])
	base := strings.ToUpper(strings.TrimSpace(rec[1]))
	quote := strings.ToUpper(strings.TrimSpace(rec[2]))
	rateStr := strings.TrimSpace(rec[3])
	source := strings.TrimSpace(rec[4])

	if !validRateDate(date) {
		return store.Rate{}, "admin.rates.error.bad_date"
	}
	if !known[base] || !known[quote] {
		return store.Rate{}, "admin.rates.error.unknown_currency"
	}
	val, err := strconv.ParseFloat(rateStr, 64)
	if err != nil || val <= 0 {
		return store.Rate{}, "admin.rates.error.bad_rate"
	}
	if source == "" {
		return store.Rate{}, "admin.rates.error.bad_source"
	}
	return store.Rate{RateDate: date, Base: base, Quote: quote, Value: val, Source: source}, ""
}

// validRateDate reports whether s is a strict YYYY-MM-DD calendar date. It reuses
// the store's date convention (transactions and rate_date share it); the app never
// uses time.Format/parse in a template path (rule 10), but this is a data-key
// validation on upload, not user-facing rendering.
func validRateDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	y, e1 := strconv.Atoi(s[0:4])
	m, e2 := strconv.Atoi(s[5:7])
	d, e3 := strconv.Atoi(s[8:10])
	if e1 != nil || e2 != nil || e3 != nil {
		return false
	}
	if y < 1 || m < 1 || m > 12 || d < 1 || d > 31 {
		return false
	}
	return true
}

// sameHeader reports whether got matches ratesCSVHeader exactly (case-insensitive on
// the column names, trimmed) -- the header is fixed so a mis-ordered file is caught.
func sameHeader(got []string) bool {
	if len(got) != len(ratesCSVHeader) {
		return false
	}
	for i, col := range ratesCSVHeader {
		if !strings.EqualFold(strings.TrimSpace(got[i]), col) {
			return false
		}
	}
	return true
}

// renderRatesError re-renders the upload page at 422 with the error key+arg shown.
// The file input can't be echoed back on a re-render, so this is a full-page 422
// (like the currencies add-error), not an inline partial swap.
func (s *server) renderRatesError(w http.ResponseWriter, r *http.Request, key, arg string) {
	s.render(w, r, http.StatusUnprocessableEntity, "admin_rates.tmpl",
		s.newShellPage(r, ratesPageModel{ErrorKey: key, ErrorArg: arg}))
}

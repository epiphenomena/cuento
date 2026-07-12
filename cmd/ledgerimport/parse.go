package main

import (
	"encoding/csv"
	"fmt"
	"io"

	"cuento/internal/money"
)

// numFields is the exact column count of a cleaned full-ledger export record
// (docs/ledger-export.md). encoding/csv handles the RFC 4180 hazards the doc
// warns about (embedded commas and newlines inside quoted memos) natively; we
// pin FieldsPerRecord so a malformed row is caught, not silently misaligned.
const numFields = 22

// Column indices in the fixed 22-field order documented in docs/ledger-export.md:
//
//	country, stmt, typ, acct, kat, dt, v, ndb, fndb, kls, klass, tid, desc,
//	donor, currency, xrt, db, fdb, cr, fcr, clr, parent
const (
	colCountry = iota
	colStmt
	colTyp
	colAcct
	colKat
	colDt
	colV
	colNdb
	colFndb
	colKls
	colKlass
	colTid
	colDesc
	colDonor
	colCurrency
	colXrt
	colDb
	colFdb
	colCr
	colFcr
	colClr
	colParent
)

// Record is one parsed split line. Field names mirror docs/ledger-export.md.
// The float-noisy pre-computed columns (v, ndb, fndb) are DELIBERATELY NOT
// carried: the doc marks them lossy and forbids their use for exact math, so the
// parser drops them entirely rather than tempt any downstream code (rule 3).
type Record struct {
	Country  string
	Stmt     string
	Typ      string
	Acct     string
	Kat      string
	Dt       string
	Kls      string
	Klass    string
	Tid      string
	Desc     string
	Donor    string
	Currency string
	Xrt      string // exchange rate, verbatim (parsed lazily by the FX step)
	Db       string // debit, authoritative (>= 0, "" or "0" when this is a credit)
	Fdb      string // foreign debit, authoritative
	Cr       string // credit, authoritative
	Fcr      string // foreign credit, authoritative
	Clr      string // reconciliation flag R/C/blank
	Parent   string
}

// ParseRecords reads the whole export from r and returns the structured splits
// in file order. It uses encoding/csv with FieldsPerRecord pinned to 22 so an
// off-count row is a hard error (the doc's "use a real CSV reader" mandate). The
// header row is required and skipped. The float-noisy net columns are read past
// but never stored.
func ParseRecords(r io.Reader) ([]Record, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = numFields // pin: every data record has exactly 22 fields.
	cr.ReuseRecord = false

	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse csv: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("parse csv: empty input (missing header)")
	}

	out := make([]Record, 0, len(rows)-1)
	for _, f := range rows[1:] { // rows[0] is the header.
		out = append(out, Record{
			Country:  f[colCountry],
			Stmt:     f[colStmt],
			Typ:      f[colTyp],
			Acct:     f[colAcct],
			Kat:      f[colKat],
			Dt:       f[colDt],
			Kls:      f[colKls],
			Klass:    f[colKlass],
			Tid:      f[colTid],
			Desc:     f[colDesc],
			Donor:    f[colDonor],
			Currency: f[colCurrency],
			Xrt:      f[colXrt],
			Db:       f[colDb],
			Fdb:      f[colFdb],
			Cr:       f[colCr],
			Fcr:      f[colFcr],
			Clr:      f[colClr],
			Parent:   f[colParent],
		})
	}
	return out, nil
}

// NetDebit computes the split's exact signed net-debit (debits +, credits -, D2)
// in int64 minor units, from the AUTHORITATIVE db/cr columns ONLY (never
// ndb/fndb/v -- those carry float-subtraction noise, docs/ledger-export.md
// "Amounts"). A blank amount side means zero (one of db/cr is always zero). The
// parse goes through money.Parse (rule 10's exact integer path); ParseFloat is
// never used, which is the whole point -- it would reintroduce the noise the doc
// forbids. exponent is the split currency's minor-unit exponent (D1).
func NetDebit(dbAmt, crAmt string, exponent int) (int64, error) {
	d, err := parseAmount(dbAmt, exponent)
	if err != nil {
		return 0, fmt.Errorf("debit: %w", err)
	}
	c, err := parseAmount(crAmt, exponent)
	if err != nil {
		return 0, fmt.Errorf("credit: %w", err)
	}
	return d - c, nil
}

// parseAmount maps a blank field to 0 (money.Parse rejects "") and otherwise
// parses an exact minor-unit integer with the plain (dot-decimal, no grouping)
// number format the export uses (docs/ledger-export.md "Amounts").
func parseAmount(s string, exponent int) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return money.Parse(s, exponent, money.NumberPlain)
}

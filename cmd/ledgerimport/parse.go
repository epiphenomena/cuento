package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

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

// nativeNetDebit computes a split's exact signed net-debit IN ITS OWN CURRENCY'S
// minor units (rule 3), choosing the authoritative column pair by currency:
//
//   - The `db`/`cr` pair is the BASE-currency (org functional, e.g. USD) amount.
//   - The `fdb`/`fcr` pair is the OTHER-currency (native, e.g. HNL) amount.
//
// These are the two sides of the base/native pair related by `xrt`
// (docs/ledger-export.md "Amounts"), and which pair is native depends ONLY on the
// split's `currency`, NOT on which column reads larger. When the split is already
// in the base currency the two pairs are equal and `xrt = 1`, so `db`/`cr` is
// used. When the split is in the foreign currency the native amount lives in
// `fdb`/`fcr` -- using `db`/`cr` there would store the USD magnitude mislabeled as
// native, which reports then convert a second time (the p26.56 corruption). The
// branch is REQUIRED, not cosmetic: on the real export `db != fdb` on a large
// fraction of BOTH USD splits (their foreign HNL counterpart) and foreign splits
// (their base USD counterpart), so a uniform column choice would corrupt one side
// or the other.
//
// REVERSE-CONVENTION EDGE CASE (p26.67): a handful of rows carry `currency == base`
// (USD) yet VIOLATE the base-currency invariant the doc promises. On a well-formed
// base row `xrt = 1` and the `db`/`cr` pair holds the USD figure. On these rows
// `xrt != 1` AND the `db`/`cr` pair holds the FOREIGN (lempira) figure while `fdb`/`fcr`
// holds the true USD -- exactly BACKWARD from every other base row. The tell is that the
// base pair is the xrt-SCALED (larger) magnitude: `|net(db,cr)| == |net(fdb,fcr)| * xrt`
// (whereas a normal USD row's `db`/`cr` is the smaller USD side and its `fdb`/`fcr` the
// larger foreign counterpart). Taking the base branch's `db`/`cr` there stores the
// lempira magnitude mislabeled as USD (~24x too large). The NARROW guard below detects
// exactly this violated invariant -- verified against the real export to match precisely
// the five Caja Moneda Extranjera rows (one account) and NO other row -- and takes native
// from `fdb`/`fcr` instead. Every normal base row (`db == fdb`, `xrt = 1`, OR the USD side
// being the smaller magnitude) is unchanged.
//
// exp is the split currency's minor-unit exponent (D1); base is the org base
// currency (cfg.BaseCurrency).
func nativeNetDebit(r Record, exp int, base string) (int64, error) {
	if r.Currency == base {
		if reverseConventionBaseRow(r, exp) {
			return NetDebit(r.Fdb, r.Fcr, exp)
		}
		return NetDebit(r.Db, r.Cr, exp)
	}
	return NetDebit(r.Fdb, r.Fcr, exp)
}

// reverseConventionBaseRow reports whether a base-currency (USD) row is one of the
// reverse-convention rows whose `db`/`cr` columns hold the FOREIGN (lempira) figure and
// whose `fdb`/`fcr` hold the true USD (p26.67). It is true ONLY when ALL hold:
//
//   - `xrt != 1` (a genuine cross-currency row; a true base row has xrt = 1), AND
//   - both net-debits are nonzero and DIFFER (the base invariant `db == fdb` is broken), AND
//   - the base magnitude is the LARGER one and equals the foreign magnitude scaled by
//     xrt: `|net(db,cr)| == |net(fdb,fcr)| * xrt` (within a small tolerance). This is the
//     discriminator that separates these rows from a NORMAL USD row -- where `db`/`cr` is
//     the SMALLER (USD) figure and `fdb`/`fcr` the larger xrt-scaled foreign counterpart.
//
// Net-debits are compared as EXACT parsed minor units; only `xrt` (a rate, the one float
// the ledger permits, D12) and the scaling comparison touch float. A row whose `xrt` is
// unparseable, or that fails any clause, keeps the normal `db`/`cr` base path.
func reverseConventionBaseRow(r Record, exp int) bool {
	xrt, err := strconv.ParseFloat(strings.TrimSpace(r.Xrt), 64)
	if err != nil || xrt == 1 {
		return false // xrt = 1 (or unknown) => a true base row; normal db/cr path.
	}
	base, err := NetDebit(r.Db, r.Cr, exp)
	if err != nil {
		return false
	}
	foreign, err := NetDebit(r.Fdb, r.Fcr, exp)
	if err != nil {
		return false
	}
	if base == 0 || foreign == 0 || base == foreign {
		return false // one side blank, or the invariant holds (db == fdb).
	}
	// The base magnitude must be the LARGER of the two AND equal the foreign magnitude
	// scaled by xrt (the reverse-convention tell). A normal USD row fails BOTH: its
	// db/cr is the smaller USD side.
	fb, ff := math.Abs(float64(base)), math.Abs(float64(foreign))
	if fb <= ff {
		return false
	}
	return math.Abs(fb-ff*xrt) <= 0.02*fb
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

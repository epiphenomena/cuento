package bankimport

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
)

// p26.64 horizontal column mapping. Instead of typing a zero-based column INDEX per
// field, the UI shows the file's ACTUAL columns (header name + a sample value) and lets
// the user pick a ROLE for each ("maps to" Date / Description / Amount / Debit / Credit
// / Ignore). The server derives the existing index-based Config from those per-column
// role picks. This file owns the two pure pieces: reading the columns for display, and
// converting between the per-column roles and the Config indices (both directions).

// Role is what a CSV column maps to. RoleIgnore (the zero value / empty string) means
// the column is unused. The set mirrors the Config fields the parser reads.
type Role string

const (
	// RoleIgnore: the column is not mapped (the default for every column).
	RoleIgnore Role = ""
	// RoleDate maps to Config.DateCol.
	RoleDate Role = "date"
	// RoleDescription maps to Config.DescCol.
	RoleDescription Role = "desc"
	// RoleAmount maps to Config.AmountCol (single signed-amount mode).
	RoleAmount Role = "amount"
	// RoleDebit maps to Config.DebitCol (debit/credit mode).
	RoleDebit Role = "debit"
	// RoleCredit maps to Config.CreditCol (debit/credit mode).
	RoleCredit Role = "credit"
)

// ColumnInfo is one CSV column for the horizontal mapping UI: its header NAME (or a
// synthesized "Column N" when the file has no header) and a SAMPLE value taken from the
// first data row (empty when the row is short).
type ColumnInfo struct {
	Index  int    // zero-based column index
	Name   string // header cell, or "Column <n>" when HasHeader is false
	Sample string // first data row's cell at this index, or "" if absent
}

// Columns reads raw under the delimiter/header settings and returns one ColumnInfo per
// column, for the mapping UI to label. It reuses resolveDelimiter (auto-sniff or the
// pinned choice). When HasHeader is true the first row supplies the names and the
// SECOND row is the sample; otherwise names are synthesized ("Column 1"...) and the
// FIRST row is the sample. A file with no readable rows is ErrNoRows; an unreadable CSV
// structure is a wrapped error. This is display-only: it never parses amounts or dates.
func Columns(raw []byte, d Delimiter, hasHeader bool) ([]ColumnInfo, error) {
	delim, err := resolveDelimiter(raw, d)
	if err != nil {
		return nil, err
	}

	cr := csv.NewReader(bytes.NewReader(raw))
	cr.Comma = delim
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("bankimport: read csv: %w", err)
	}
	if len(records) == 0 {
		return nil, ErrNoRows
	}

	// Determine the column count and the names + sample row per the header flag.
	var names []string
	var sample []string
	if hasHeader {
		names = records[0]
		if len(records) > 1 {
			sample = records[1]
		}
	} else {
		// No header: the widest column count across all rows, synthesized names, and
		// the first row as the sample.
		width := 0
		for _, rec := range records {
			if len(rec) > width {
				width = len(rec)
			}
		}
		names = make([]string, width)
		for i := range names {
			names[i] = "Column " + strconv.Itoa(i+1)
		}
		sample = records[0]
	}

	out := make([]ColumnInfo, 0, len(names))
	for i, name := range names {
		ci := ColumnInfo{Index: i, Name: strings.TrimSpace(name)}
		if i < len(sample) {
			ci.Sample = strings.TrimSpace(sample[i])
		}
		out = append(out, ci)
	}
	return out, nil
}

// ConfigFromRoles builds the index-based column mapping from a per-column role slice
// (roles[i] is the role assigned to column i) plus the non-column settings (delimiter,
// header, amount mode, sign flip, date format). The LAST column claiming a role wins if
// two columns pick the same role (a deterministic, forgiving rule). Unclaimed roles
// land as -1 (unmapped); the required Date + the mode's amount role(s) SHOULD be set,
// but validation is the parser's job (a missing required column surfaces as a per-row
// parse error, the existing all-or-nothing 422). This is the inverse of RolesFromConfig.
func ConfigFromRoles(roles []Role, delim Delimiter, hasHeader bool, mode AmountMode, signFlip bool, dateFmt DateLayout) Config {
	cfg := Config{
		Delimiter: delim,
		HasHeader: hasHeader,
		Amount:    mode,
		SignFlip:  signFlip,
		DateFmt:   dateFmt,
		DateCol:   -1,
		AmountCol: -1,
		DebitCol:  -1,
		CreditCol: -1,
		DescCol:   -1,
		MemoCol:   -1,
	}
	for i, role := range roles {
		switch role {
		case RoleDate:
			cfg.DateCol = i
		case RoleDescription:
			cfg.DescCol = i
		case RoleAmount:
			cfg.AmountCol = i
		case RoleDebit:
			cfg.DebitCol = i
		case RoleCredit:
			cfg.CreditCol = i
		}
	}
	return cfg
}

// RolesFromConfig is the inverse: given a Config and a column count, return the role
// each column [0,n) carries (RoleIgnore where none). It is used to PRE-SELECT the "maps
// to" dropdowns when a saved profile is loaded. A Config column index that is negative
// (unmapped) or >= n (a profile built for a WIDER file) simply lands no role on any
// in-range column -- it never panics and never assigns out of range (the p26.64
// "profile made for a different CSV" edge). MemoCol is not a mapping-UI role and is
// dropped from the round trip (the horizontal UI maps only date/desc/amount/dr/cr).
func RolesFromConfig(cfg Config, n int) []Role {
	roles := make([]Role, n)
	set := func(i int, r Role) {
		if i >= 0 && i < n {
			roles[i] = r
		}
	}
	set(cfg.DateCol, RoleDate)
	set(cfg.DescCol, RoleDescription)
	set(cfg.AmountCol, RoleAmount)
	set(cfg.DebitCol, RoleDebit)
	set(cfg.CreditCol, RoleCredit)
	return roles
}

package reports

import (
	"bytes"
	"encoding/csv"
	"testing"
)

// TestCSVEscapingRoundTrips is the p15.1 CSV-correctness test: a cell containing a
// comma, a double-quote, AND a newline must round-trip through encoding/csv intact
// (RFC 4180 quoting/escaping). We build a Table with such a cell, write it, parse it
// back with a csv.Reader, and assert every field is byte-identical.
func TestCSVEscapingRoundTrips(t *testing.T) {
	nasty := "a,b\"c\nd" // comma, embedded double-quote, embedded newline
	idOfLocalize := func(k string) string { return k }

	tbl := Table{
		Columns: []Column{
			{HeaderKey: "col.label", Align: AlignLeft},
			{HeaderKey: "col.amount", Align: AlignRight},
		},
		Rows: []Row{
			{Cells: []Cell{TextCell(nasty), MoneyCell(123456, "USD")}},
			{Cells: []Cell{TextCell("plain"), MoneyCell(-5000, "USD")}},
			{Cells: []Cell{TextCell("blank amount"), BlankMoneyCell()}, Kind: RowSubtotal},
		},
	}

	var buf bytes.Buffer
	if err := WriteCSV(&buf, tbl, idOfLocalize, map[string]int{"USD": 2}); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}

	recs, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("re-parse CSV: %v", err)
	}

	// Header + 3 rows.
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4 (header + 3 rows)\nraw:\n%s", len(recs), buf.String())
	}
	if recs[0][0] != "col.label" || recs[0][1] != "col.amount" {
		t.Errorf("header = %v, want [col.label col.amount]", recs[0])
	}
	// The nasty cell survived quoting/escaping byte-for-byte.
	if recs[1][0] != nasty {
		t.Errorf("nasty cell round-trip = %q, want %q", recs[1][0], nasty)
	}
	// Money is machine-plain (NumberPlain, no grouping separators).
	if recs[1][1] != "1234.56" {
		t.Errorf("money cell = %q, want 1234.56 (plain, ungrouped)", recs[1][1])
	}
	if recs[2][1] != "-50.00" {
		t.Errorf("negative money cell = %q, want -50.00", recs[2][1])
	}
	// A blank money cell is the empty string, not a formatted zero.
	if recs[3][1] != "" {
		t.Errorf("blank money cell = %q, want empty", recs[3][1])
	}
}

// TestCSVLocalizesHeaders proves the header row runs through the caller's localizer
// (so CSV headers read in the request language) while the framework stays i18n-free.
func TestCSVLocalizesHeaders(t *testing.T) {
	loc := func(k string) string {
		if k == "col.label" {
			return "Etiqueta"
		}
		return k
	}
	tbl := Table{Columns: []Column{{HeaderKey: "col.label"}}}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, tbl, loc, nil); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	recs, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(recs) != 1 || recs[0][0] != "Etiqueta" {
		t.Errorf("localized header = %v, want [Etiqueta]", recs)
	}
}

package reports_test

// Small table-inspection + CSV helpers shared by the trial-balance report tests (and
// reusable by p15.4–p15.11's golden tests). They read the framework's public Table
// type and the stdlib csv reader — nothing report-specific — so a later report's test
// reuses them verbatim.

import (
	"bytes"
	"encoding/csv"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cuento/internal/i18n"
	"cuento/internal/reports"
)

// --- the reusable Phase-15 GOLDEN harness (report-agnostic) ----------------
// These are the pieces p15.4–p15.11 REUSE unchanged: the -update flag, the
// compare-or-regenerate mechanism, and the golden localizer. They live in this
// shared file (not a per-report test file) so a second report's *_test.go can just
// CALL checkGolden — declaring `updateGolden`/`checkGolden`/`goldenLocalize` per
// report would redefine the flag at runtime (panic "flag redefined") and redeclare
// the funcs at compile time (all Phase-15 report tests share one reports_test binary).

// updateGolden, set by `-update` (wired to `make golden`), regenerates the committed
// golden artifacts instead of comparing against them. Deterministic: each golden test
// pins its params, currency, and locale, so a regenerated golden is byte-stable.
var updateGolden = flag.Bool("update", false, "regenerate report golden files under testdata/")

// goldenLocalize is the golden's fixed localizer: the request-language {{t}} pinned to
// en, resolving a report's i18n LABEL/header keys against the REAL catalog (i18n.T).
// Tying the golden to the actual catalog — rather than a hand-copied key->text table —
// means a future en.toml header edit can't silently drift a golden away from what the
// app's live CSV/HTML endpoints emit. reports_test -> i18n is cycle-free (i18n imports
// nothing from reports).
func goldenLocalize(key string) string { return i18n.T("en", key) }

// checkGolden compares got to the committed golden at testdata/<name>, or regenerates
// it under -update. The golden dir is versioned; a mismatch prints the path so a human
// reviews the diff (never blind-commits). This is the reusable Phase-15 mechanism
// every report's golden test calls.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `make golden` to create): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %s mismatch — run `make golden` and REVIEW the diff.\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}

// --- table-inspection + CSV helpers ----------------------------------------

// accountNames returns the set of account-name strings appearing in a table's first
// column across DATA rows (skipping subtotal/total/warning rows, whose first cell is a
// label). Used by the scope test to compare account sets.
func accountNames(t reports.Table) map[string]bool {
	out := map[string]bool{}
	for _, row := range t.Rows {
		if row.Kind != reports.RowData || len(row.Cells) == 0 {
			continue
		}
		c := row.Cells[0]
		if c.Kind == reports.CellText && c.Text != "" {
			out[c.Text] = true
		}
	}
	return out
}

// convertedCellFor returns the CONVERTED (column 3) minor amount for the DATA row
// whose account name (column 0) is name and whose native currency (column 1) is
// nativeCcy, and whether such a row was found. Lets a test pin one account's converted
// figure without positional coupling.
func convertedCellFor(t reports.Table, name, nativeCcy string) (int64, bool) {
	for _, row := range t.Rows {
		if row.Kind != reports.RowData || len(row.Cells) < 4 {
			continue
		}
		if row.Cells[0].Text == name && row.Cells[1].Text == nativeCcy {
			return row.Cells[3].Minor, true
		}
	}
	return 0, false
}

// nativeCellFor returns the NATIVE (column 2) minor amount for the DATA row whose
// account name (column 0) is name and whose currency (column 1) is ccy, and whether
// such a row was found — the native counterpart of convertedCellFor, letting a test
// pin one account's native figure directly against the fixture oracle.
func nativeCellFor(t reports.Table, name, ccy string) (int64, bool) {
	for _, row := range t.Rows {
		if row.Kind != reports.RowData || len(row.Cells) < 3 {
			continue
		}
		if row.Cells[0].Text == name && row.Cells[1].Text == ccy {
			return row.Cells[2].Minor, true
		}
	}
	return 0, false
}

// localizeLabels returns a copy of t with each LABEL cell resolved to an English TEXT
// cell (the CSV writer is i18n-free), mirroring the web layer's localizeLabelCells so
// the golden CSV reads in English. Proper-noun TEXT and money cells are untouched.
func localizeLabels(t reports.Table) reports.Table {
	out := reports.Table{Columns: t.Columns}
	out.Rows = make([]reports.Row, len(t.Rows))
	for i, row := range t.Rows {
		nr := reports.Row{Indent: row.Indent, Kind: row.Kind}
		nr.Cells = make([]reports.Cell, len(row.Cells))
		for j, c := range row.Cells {
			if c.Kind == reports.CellLabel {
				c = reports.TextCell(goldenLocalize(c.Text))
			}
			nr.Cells[j] = c
		}
		out.Rows[i] = nr
	}
	return out
}

// parseCSV reads b as CSV and returns all records, failing the test on a parse error
// (proving the report's CSV output is well-formed / round-trips).
func parseCSV(t *testing.T, b []byte) [][]string {
	t.Helper()
	recs, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return recs
}

// parseMinor parses a machine-plain signed decimal (e.g. "-552486.00" or "2182320.00")
// as an exponent-2 minor-unit int64 (both fixture currencies have exponent 2). It is a
// test-only inverse of the CSV money format (NumberPlain, dot decimal), used to re-sum
// the exported native column and confirm the trial balance still balances after export.
func parseMinor(t *testing.T, s string) int64 {
	t.Helper()
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, fracPart, _ := strings.Cut(s, ".")
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	if len(fracPart) > 2 {
		t.Fatalf("parseMinor: %q has more than 2 fractional digits", s)
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		t.Fatalf("parseMinor int %q: %v", intPart, err)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		t.Fatalf("parseMinor frac %q: %v", fracPart, err)
	}
	minor := whole*100 + frac
	if neg {
		minor = -minor
	}
	return minor
}

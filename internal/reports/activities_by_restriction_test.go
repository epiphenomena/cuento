package reports_test

// p15.9 activities-by-restriction report tests. The FASB nonprofit STATEMENT OF
// ACTIVITIES: two donor-restriction columns (Without / With) + a Total column, per
// currency. Every number is HAND-DERIVED from the synthetic fixture (PLAN Appendix D,
// internal/testutil/fixture) — transactions.go is the oracle for the period flows and
// the fund tagging.
//
// GROUP funds — no, "financial" (a net-asset statement; the code-declared Groups()
// doc already files p15.9 under "financial"). NATIVE currency, per-currency rows
// (Line | Currency | Without | With | Total): consistent with p15.8 (the D20 released
// derivation source, also native) so the released line equals Σ p15.8 restricted-fund
// applications by EXACT int64 equality, and consistent with the balance-sheet
// detail-mode per-currency FASB split. Documented in DECISIONS.
//
// HAND-DERIVED figures (root scope, 2025-01-01..2026-06-30):
//
//	Revenue (net-debit credits shown POSITIVE), split by the fund a split lands in —
//	restricted (Beca Agua / Building Fund) => With; NULL / unrestricted => Without:
//	  USD  With    = Contributions(Building Fund) 50,000.00 + GovGrants(Beca) 2,000.00
//	                = 5,200,000
//	  USD  Without = Contributions 2,750.00 + Program Fees 1,200.00 + Event 3,000.00
//	                = 695,000
//	  USD  Total   = 5,895,000
//	  MXN  With    = GovGrants(Beca) 100,000.00 = 10,000,000 ; MXN Without = 0
//	  MXN  Total   = 10,000,000
//
//	Net assets released from restrictions = Σ restricted funds' APPLICATIONS in period
//	(D20, no journaled transfer): Beca Agua expense (MXN 3,000.00 / USD 1,500.00) +
//	Building Fund non-expense (USD 40,000.00, the Building purchase):
//	  USD released = 150,000 + 4,000,000 = 4,150,000
//	  MXN released = 300,000
//	Presented +released in Without, −released in With => the row NETS TO ZERO in Total.
//
//	Expenses (all-fund, net-debit debits shown POSITIVE) land in Without:
//	  USD = 2,327,500 (Salaries 1,650,000 + Supplies 210,000 + Occupancy 305,000 +
//	                   Insurance 60,000 + Bank 2,500 + Event costs 100,000)
//	  MXN =   860,000 (Supplies 500,000 + Food 360,000)
//
//	Change in net assets per column = revenue ± released − expenses:
//	  USD Without = 695,000 + 4,150,000 − 2,327,500 = 2,517,500
//	  USD With    = 5,200,000 − 4,150,000          = 1,050,000
//	  USD Total   = 5,895,000 − 2,327,500          = 3,567,500  (== Without + With)
//	  MXN Without = 0 + 300,000 − 860,000          = −560,000
//	  MXN With    = 10,000,000 − 300,000           = 9,700,000
//	  MXN Total   = 10,000,000 − 860,000           = 9,140,000  (== Without + With)

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// abrReport fetches the registered activities-by-restriction report from Default().
func abrReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.ActivitiesByRestrictionReportID)
	if !ok {
		t.Fatalf("activities-by-restriction report %q not registered in Default()", reports.ActivitiesByRestrictionReportID)
	}
	return rep
}

// abrParams runs the statement over the whole fixture span, root scope, lang en.
func abrParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope: reports.SubsidiaryID(f.IDs.Root),
		From:  f.Expected.ActivityFrom, // 2025-01-01
		To:    f.Expected.AsOf,         // 2026-06-30
		Lang:  "en",
	}
}

// abrCell finds the (Without, With, Total) minor amounts of the DATA/SUBTOTAL/TOTAL row
// whose line label key (col 0) is labelKey and currency (col 1) is ccy.
func abrCell(t *testing.T, tbl reports.Table, labelKey, ccy string) (without, with, total int64) {
	t.Helper()
	for _, row := range tbl.Rows {
		if len(row.Cells) < 5 {
			continue
		}
		if row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == labelKey && row.Cells[1].Text == ccy {
			return row.Cells[2].Minor, row.Cells[3].Minor, row.Cells[4].Minor
		}
	}
	t.Fatalf("activities row %q (%s) not found", labelKey, ccy)
	return 0, 0, 0
}

// TestActivitiesByRestrictionGolden runs the statement and asserts the whole line set BY
// HAND: the With/Without revenue split, the DERIVED released line (which NETS TO ZERO in
// Total), expenses in Without, and the per-column change == Without + With == the total
// change. Golden: activities_by_restriction.{txt,csv}.
func TestActivitiesByRestrictionGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := abrReport(t)

	p := abrParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run activities by restriction: %v", err)
	}

	for _, tc := range []struct {
		ccy                        string
		revWithout, revWith        int64
		released                   int64 // magnitude; +Without, −With, 0 Total
		expenses                   int64
		chWithout, chWith, chTotal int64
	}{
		{"USD", 695_000, 5_200_000, 4_150_000, 2_327_500, 2_517_500, 1_050_000, 3_567_500},
		{"MXN", 0, 10_000_000, 300_000, 860_000, -560_000, 9_700_000, 9_140_000},
	} {
		// Revenue.
		rw, rwith, rtot := abrCell(t, table, "reports.activities_by_restriction.revenue", tc.ccy)
		if rw != tc.revWithout || rwith != tc.revWith || rtot != tc.revWithout+tc.revWith {
			t.Errorf("%s revenue = (without %d, with %d, total %d), want (%d, %d, %d)",
				tc.ccy, rw, rwith, rtot, tc.revWithout, tc.revWith, tc.revWithout+tc.revWith)
		}

		// Net assets released from restrictions: +released Without, −released With, 0 Total.
		lw, lwith, ltot := abrCell(t, table, "reports.activities_by_restriction.released", tc.ccy)
		if lw != tc.released || lwith != -tc.released || ltot != 0 {
			t.Errorf("%s released = (without %d, with %d, total %d), want (%d, %d, 0)",
				tc.ccy, lw, lwith, ltot, tc.released, -tc.released)
		}
		// The released row NETS TO ZERO in Total (Without + With == 0) — the whole point.
		if lw+lwith != 0 {
			t.Errorf("%s released row does not net to zero: %d + %d = %d", tc.ccy, lw, lwith, lw+lwith)
		}

		// Expenses land in Without only (With == 0, Total == Without).
		ew, ewith, etot := abrCell(t, table, "reports.activities_by_restriction.expenses", tc.ccy)
		if ew != tc.expenses || ewith != 0 || etot != tc.expenses {
			t.Errorf("%s expenses = (without %d, with %d, total %d), want (%d, 0, %d)",
				tc.ccy, ew, ewith, etot, tc.expenses, tc.expenses)
		}

		// Change in net assets per column, and Without + With == Total.
		cw, cwith, ctot := abrCell(t, table, "reports.activities_by_restriction.change", tc.ccy)
		if cw != tc.chWithout || cwith != tc.chWith || ctot != tc.chTotal {
			t.Errorf("%s change = (without %d, with %d, total %d), want (%d, %d, %d)",
				tc.ccy, cw, cwith, ctot, tc.chWithout, tc.chWith, tc.chTotal)
		}
		if cw+cwith != ctot {
			t.Errorf("%s change: without %d + with %d = %d != total %d", tc.ccy, cw, cwith, cw+cwith, ctot)
		}
		// Total change == revenue Total − expenses Total (released nets out).
		if ctot != rtot-etot {
			t.Errorf("%s total change %d != revenue %d − expenses %d", tc.ccy, ctot, rtot, etot)
		}
	}

	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "activities_by_restriction.txt", []byte(textDump))
	checkGolden(t, "activities_by_restriction.csv", csvBuf.Bytes())
}

// TestActivitiesReleasedMatchesFundStatements cross-checks the DERIVED released line
// against the SUM of p15.8's single-fund applied figures across the RESTRICTED funds
// (Beca Agua + Building Fund) — the numeric bridge the task requires between the two
// reports. released[ccy] == Σ_f (AppliedExpense[ccy] + AppliedNonExpense[ccy]).
func TestActivitiesReleasedMatchesFundStatements(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := abrReport(t)
	p := abrParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	tk := reports.NewToolkit(f.Store, p)

	// Sum p15.8 restricted-fund applications per currency, independently.
	wantByCcy := map[string]int64{}
	for _, fund := range []reports.FundID{f.IDs.BecaAgua, f.IDs.BuildingFund} {
		st, err := tk.FundPeriodStatement(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, fund, p.From, p.To)
		if err != nil {
			t.Fatalf("fund period statement (fund %d): %v", fund, err)
		}
		for _, ccy := range st.Currencies {
			wantByCcy[ccy] += st.AppliedExpense[ccy] + st.AppliedNonExpense[ccy]
		}
	}

	for _, ccy := range []string{"USD", "MXN"} {
		without, with, total := abrCell(t, table, "reports.activities_by_restriction.released", ccy)
		if without != wantByCcy[ccy] {
			t.Errorf("%s released (Without) = %d, want Σ p15.8 applications %d", ccy, without, wantByCcy[ccy])
		}
		if with != -wantByCcy[ccy] {
			t.Errorf("%s released (With) = %d, want −Σ p15.8 applications %d", ccy, with, -wantByCcy[ccy])
		}
		if total != 0 {
			t.Errorf("%s released Total = %d, want 0 (nets to zero)", ccy, total)
		}
	}
}

// TestActivitiesReleasedDrillReconciles: the released cell drills to the restricted-fund
// APPLICATION splits (D20) across BOTH restricted funds, and the drilled splits' signed
// sum equals the derived figure. This exercises the fund-SET drill (Drill.FundIDs): the
// USD released spans two funds (Beca Agua 150,000 + Building Fund 4,000,000), which a
// single-FundID drill cannot express.
func TestActivitiesReleasedDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := abrReport(t)
	p := abrParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, tc := range []struct {
		ccy  string
		want int64
	}{
		{"USD", 4_150_000},
		{"MXN", 300_000},
	} {
		d := abrReleasedDrill(t, table, tc.ccy)
		if sum := abrDrillSum(t, f, d); sum != tc.want {
			t.Errorf("%s released drill sum = %d, want %d", tc.ccy, sum, tc.want)
		}
	}
}

// TestActivitiesByRestrictionCSVParses: the statement CSV parses to well-formed records
// with the localized header.
func TestActivitiesByRestrictionCSVParses(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := abrReport(t)
	p := abrParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	exps := goldenExps(t, f)
	var buf bytes.Buffer
	if err := reports.WriteCSV(&buf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	recs := parseCSV(t, buf.Bytes())
	if len(recs) < 2 {
		t.Fatalf("csv has %d records, want header + rows", len(recs))
	}
	wantHeader := []string{"Line", "Currency", "Without donor restrictions", "With donor restrictions", "Total"}
	for i, h := range wantHeader {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
}

// --- helpers ----------------------------------------------------------------

// abrReleasedDrill returns the Drill on the released row's TOTAL cell (or the first
// drillable released cell) for currency ccy. The released cells carry the fund-SET drill.
func abrReleasedDrill(t *testing.T, tbl reports.Table, ccy string) *reports.Drill {
	t.Helper()
	for _, row := range tbl.Rows {
		if len(row.Cells) < 5 {
			continue
		}
		if row.Cells[0].Kind == reports.CellLabel &&
			row.Cells[0].Text == "reports.activities_by_restriction.released" &&
			row.Cells[1].Text == ccy {
			// The Without cell (col 2) carries the +released drill (magnitude == figure).
			if d := row.Cells[2].Drill; d != nil {
				return d
			}
		}
	}
	t.Fatalf("released row (%s) has no drillable cell", ccy)
	return nil
}

// abrDrillSum mirrors the web drill handler: it loops the account SET × the fund SET
// (Drill.FundIDs), summing the signed splits each (account, fund) filter selects. When
// FundIDs is empty it falls back to the single FundID (the established drill shape).
func abrDrillSum(t *testing.T, f *fixture.Fixture, d *reports.Drill) int64 {
	t.Helper()
	funds := d.FundIDs
	if len(funds) == 0 {
		// Single-fund (or no-fund) drill: one pass with d.FundID.
		var only []*reports.FundID
		only = append(only, d.FundID)
		return abrDrillSumForFunds(t, f, d, only)
	}
	ptrs := make([]*reports.FundID, len(funds))
	for i := range funds {
		id := funds[i]
		ptrs[i] = &id
	}
	return abrDrillSumForFunds(t, f, d, ptrs)
}

func abrDrillSumForFunds(t *testing.T, f *fixture.Fixture, d *reports.Drill, funds []*reports.FundID) int64 {
	t.Helper()
	var sum int64
	for _, fund := range funds {
		filter := store.DrillFilter{
			Scope:     d.Scope,
			Currency:  d.Currency,
			AsOf:      d.AsOf,
			From:      d.From,
			To:        d.To,
			FundID:    fund,
			ProgramID: d.ProgramID,
			Class:     d.Class,
		}
		for _, acct := range d.AccountIDs {
			filter.AccountID = acct
			splits, err := f.Store.DrillSplits(context.Background(), filter)
			if err != nil {
				t.Fatalf("drill splits: %v", err)
			}
			for _, s := range splits {
				sum += s.Amount
			}
		}
	}
	return sum
}

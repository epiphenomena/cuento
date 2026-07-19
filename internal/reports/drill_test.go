package reports_test

// p15.3d drill framework unit tests: the Drill filter encodes -> query string ->
// decodes round-trip (so the web layer can put it in a link and reconstruct it in
// the handler), and a cell with no Drill is not drillable (nil), while a cell that
// opts in carries the exact descriptor. These are pure (no store, no HTTP) — the
// RECONCILIATION test (drill sum == report figure) lives in reports_drill_test.go in
// the web package, where it can hit the real store query through the mounted route.

import (
	"net/url"
	"reflect"
	"testing"

	"cuento/internal/reports"
)

// TestDrillRoundTrip: a fully-populated Drill (as-of and period variants, with
// fund/program/class filters) survives Encode -> parse -> DecodeDrill unchanged.
func TestDrillRoundTrip(t *testing.T) {
	fund := reports.FundID(7)
	prog := reports.ProgramID(9)
	class := "program"

	cases := []struct {
		name string
		d    reports.Drill
	}{
		{
			name: "asof single account",
			d: reports.Drill{
				Scope:      1,
				AccountIDs: []reports.AccountID{42},
				Currency:   "MXN",
				Mode:       reports.DrillAsOf,
				AsOf:       "2026-06-30",
			},
		},
		{
			name: "asof multi account with fund",
			d: reports.Drill{
				Scope:      3,
				AccountIDs: []reports.AccountID{10, 11, 12},
				Currency:   "USD",
				FundID:     &fund,
				Mode:       reports.DrillAsOf,
				AsOf:       "2026-06-30",
			},
		},
		{
			name: "period with fund SET (p15.9 released)",
			d: reports.Drill{
				Scope:      1,
				AccountIDs: []reports.AccountID{5, 6, 7},
				Currency:   "USD",
				FundIDs:    []reports.FundID{2, 3},
				Mode:       reports.DrillPeriod,
				From:       "2025-01-01",
				To:         "2026-06-30",
			},
		},
		{
			name: "period with program and class",
			d: reports.Drill{
				Scope:      1,
				AccountIDs: []reports.AccountID{5},
				Currency:   "USD",
				ProgramID:  &prog,
				Class:      &class,
				Mode:       reports.DrillPeriod,
				From:       "2025-01-01",
				To:         "2026-06-30",
			},
		},
		{
			name: "period with program SET (p15.10 rollup cell)",
			d: reports.Drill{
				Scope:      1,
				AccountIDs: []reports.AccountID{5, 6},
				Currency:   "USD",
				ProgramIDs: []reports.ProgramID{1, 2, 3},
				Mode:       reports.DrillPeriod,
				From:       "2025-01-01",
				To:         "2026-06-30",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := tc.d.Encode()
			q, err := url.ParseQuery(enc)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", enc, err)
			}
			got := reports.DecodeDrill(q)
			if !reflect.DeepEqual(got, tc.d) {
				t.Errorf("round-trip mismatch:\n encoded = %q\n  want   = %+v\n  got    = %+v", enc, tc.d, got)
			}
		})
	}
}

// TestDrillEmptyQueryDecodesEmpty: the permission matrix hits /reports/{id}/drill
// with NO query string. That must decode to a zero Drill with no accounts (the
// handler then renders an empty list, a 200 — not a 500), so the drill route is
// matrix-reachable with a bare hit.
func TestDrillEmptyQueryDecodesEmpty(t *testing.T) {
	got := reports.DecodeDrill(url.Values{})
	if len(got.AccountIDs) != 0 {
		t.Errorf("empty query decoded AccountIDs = %v, want none", got.AccountIDs)
	}
	if got.Scope != 0 || got.Currency != "" {
		t.Errorf("empty query decoded non-zero drill: %+v", got)
	}
	// A malformed query degrades to empty, never panics/errors.
	got = reports.DecodeDrill(url.Values{"accts": {"not-a-number"}, "scope": {"xx"}})
	if len(got.AccountIDs) != 0 || got.Scope != 0 {
		t.Errorf("malformed query decoded to %+v, want empty", got)
	}
}

// TestCellDrillOptIn: a plain money cell is NOT drillable (nil Drill); WithDrill
// attaches the descriptor and returns a cell carrying exactly it, leaving the value
// fields intact. This is the p15.4+ attach pattern.
func TestCellDrillOptIn(t *testing.T) {
	plain := reports.MoneyCell(1234, "USD")
	if plain.Drill != nil {
		t.Errorf("plain MoneyCell is drillable (Drill=%+v), want nil", plain.Drill)
	}

	d := &reports.Drill{Scope: 1, AccountIDs: []reports.AccountID{42}, Currency: "USD", Mode: reports.DrillAsOf, AsOf: "2026-06-30"}
	drillable := reports.MoneyCell(1234, "USD").WithDrill(d)
	if drillable.Drill != d {
		t.Errorf("WithDrill did not attach the drill descriptor")
	}
	// The value fields survive.
	if drillable.Minor != 1234 || drillable.Currency != "USD" || drillable.Kind != reports.CellMoney {
		t.Errorf("WithDrill mutated the cell value: %+v", drillable)
	}
	// A label cell stays non-drillable.
	if reports.LabelCell("x").Drill != nil {
		t.Errorf("LabelCell is drillable, want nil")
	}
}

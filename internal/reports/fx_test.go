package reports_test

import (
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// TestFXRemeasurementOracle proves the Phase 31 remeasurement toolkit reproduces the
// fixture's INDEPENDENTLY hand-computed FX loss (fixture.FXExpected), so the FX-detail
// report and the Statement-of-Activities FX line are later validated against a number
// the report code did not itself produce. After ExtendRates + ExtendFX, the only
// foreign-currency, monetary, NON-intercompany balance in the org is Banco Lempira (HNL
// in the USD-functional US sub); its remeasurement is the −$461.74 loss. The
// intercompany USD payable in the MXN sub (Due to RV Internacional) is EXCLUDED from the
// income path (it flows through the CTA/translation carve, p26.70), so it must NOT appear.
func TestFXRemeasurementOracle(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t) // MXN schedule (for the whole org's conversion)
	f.ExtendFX(t)    // HNL schedule + the Lempira monetary item
	ctx := context.Background()

	tk := reports.NewToolkit(f.Store, reports.Params{Scope: f.IDs.Root, TargetCurrency: "USD"})
	got, err := tk.FXRemeasurementAsOf(ctx, reports.Scope{Sub: f.IDs.Root}, f.Expected.FX.AsOf)
	if err != nil {
		t.Fatalf("FXRemeasurementAsOf: %v", err)
	}

	// Exactly one income-path item: Banco Lempira. (Due to RV Internacional is
	// intercompany -> CTA path, excluded; FX Clearing is equity-class -> not monetary.)
	if len(got.Items) != 1 {
		t.Fatalf("FX items = %d, want 1 (only Banco Lempira); got %+v", len(got.Items), got.Items)
	}
	it := got.Items[0]
	if it.Account != f.Expected.FX.Bank {
		t.Errorf("FX item account = %d, want Banco Lempira %d", it.Account, f.Expected.FX.Bank)
	}
	if it.Currency != "HNL" || it.Functional != "USD" {
		t.Errorf("FX item currency/functional = %s/%s, want HNL/USD", it.Currency, it.Functional)
	}
	if it.NativeMinor != f.Expected.FX.NativeHNL {
		t.Errorf("native = %d, want %d", it.NativeMinor, f.Expected.FX.NativeHNL)
	}
	if it.ClosingMinor != f.Expected.FX.EndingUSDMinor {
		t.Errorf("closing USD = %d, want %d", it.ClosingMinor, f.Expected.FX.EndingUSDMinor)
	}
	if it.RemeasureMinor != f.Expected.FX.RemeasurementUSDMinor {
		t.Errorf("remeasurement = %d, want %d (loss $%.2f)",
			it.RemeasureMinor, f.Expected.FX.RemeasurementUSDMinor,
			float64(f.Expected.FX.RemeasurementUSDMinor)/100)
	}

	// The per-functional total equals the single item (USD-functional US sub).
	if got.ByFunctional["USD"] != f.Expected.FX.RemeasurementUSDMinor {
		t.Errorf("USD remeasurement total = %d, want %d",
			got.ByFunctional["USD"], f.Expected.FX.RemeasurementUSDMinor)
	}
}

// TestFXArticulationGap is the load-bearing invariant behind Phase 31 recognition: the
// remeasurement FX loss is EXACTLY the gap by which the current reports fail to
// articulate. Measured as a with/without-ExtendFX difference (so opening balances,
// intercompany, and every other item cancel), the change ExtendFX makes to the
// balance-sheet net assets (closing rate) is SHORT of the change it makes to the
// Statement-of-Activities surplus (transaction-date rate) by precisely the FX
// remeasurement. Recognition (p31.2) closes this gap by putting the FX line on the SoA;
// this test pins the number the line must carry, at both a single-sub and a consolidated
// scope. If this ever fails, the SoA/BS conversion model changed and recognition must be
// rethought before goldens move.
func TestFXArticulationGap(t *testing.T) {
	base := fixture.New(t)
	base.ExtendRates(t)
	withFX := fixture.New(t)
	withFX.ExtendRates(t)
	withFX.ExtendFX(t)

	for _, sc := range []struct {
		name  string
		scope reports.SubsidiaryID
	}{
		{"US (single sub)", base.IDs.US},
		{"Root (consolidated)", base.IDs.Root},
	} {
		naDiff := netAssetsClosing(t, withFX, sc.scope) - netAssetsClosing(t, base, sc.scope)
		soaDiff := soaChangeTxnDate(t, withFX, sc.scope) - soaChangeTxnDate(t, base, sc.scope)
		gap := soaDiff - naDiff // SoA surplus minus BS net-asset increase = the FX loss magnitude

		wantFX := withFX.Expected.FX.RemeasurementUSDMinor // -46,174 (a loss)
		if gap != -wantFX {
			t.Errorf("[%s] articulation gap = %d, want %d (== −remeasurement). naDiff=%d soaDiff=%d",
				sc.name, gap, -wantFX, naDiff, soaDiff)
		}
	}
}

// netAssetsClosing sums assets − liabilities at the closing rate (USD) for a scope --
// the net-assets total the balance sheet plugs to. Used by the articulation test as a
// with/without-ExtendFX difference, so intercompany/CTA subtleties cancel.
func netAssetsClosing(t *testing.T, f *fixture.Fixture, scope reports.SubsidiaryID) int64 {
	t.Helper()
	ctx := context.Background()
	tk := reports.NewToolkit(f.Store, reports.Params{Scope: scope, TargetCurrency: "USD"})
	bals, err := tk.BalancesAsOf(ctx, reports.Scope{Sub: scope}, f.Expected.AsOf,
		reports.ConvertOpts{To: "USD", Mode: reports.RateClosing})
	if err != nil {
		t.Fatal(err)
	}
	var assets, liabilities int64
	for acct, amts := range bals {
		a, err := f.Store.GetAccount(ctx, acct)
		if err != nil {
			t.Fatal(err)
		}
		for _, amt := range amts {
			switch a.Type {
			case "asset":
				assets += amt.Minor
			case "liability":
				liabilities += amt.Minor // credit balance (negative net-debit)
			}
		}
	}
	return assets - liabilities // net assets = assets − liabilities
}

// soaChangeTxnDate is the Statement-of-Activities change in net assets (revenue −
// expenses) over the whole fixture window, converted at transaction-date rates (USD).
func soaChangeTxnDate(t *testing.T, f *fixture.Fixture, scope reports.SubsidiaryID) int64 {
	t.Helper()
	ctx := context.Background()
	tk := reports.NewToolkit(f.Store, reports.Params{Scope: scope, TargetCurrency: "USD"})
	ni, err := tk.NetIncome(ctx, reports.Scope{Sub: scope}, "2025-01-01", f.Expected.AsOf,
		reports.ConvertOpts{To: "USD", Mode: reports.RateTxnDate})
	if err != nil {
		t.Fatal(err)
	}
	return -ni.Minor // net-debit subtotal -> change in net assets = −subtotal
}

// TestIncomeStatementFXArticulates proves the p31.2 recognition closes the gap ON THE
// ACTUAL REPORT: with the Lempira exposure present, the income statement grows an "FX
// remeasurement gain/(loss)" line carrying exactly the −$461.74 loss, and its "change in
// net assets" total now moves in lockstep with the balance sheet's net-asset change (both
// +$5,836.58, the closing value of the residual Lempira balance) -- whereas the operating
// surplus alone would have moved by +$6,298.32 (the transaction-date flows), the old
// articulation gap. Base fixture (no exposure): NO FX line, byte-identical statement.
func TestIncomeStatementFXArticulates(t *testing.T) {
	base := fixture.New(t)
	base.ExtendRates(t)
	withFX := fixture.New(t)
	withFX.ExtendRates(t)
	withFX.ExtendFX(t)
	ctx := context.Background()

	rep, ok := reports.Default().Get(reports.IncomeStatementReportID)
	if !ok {
		t.Fatal("income statement not registered")
	}

	pBase := isGoldenParams(base)
	baseTbl, err := rep.Run(ctx, reports.NewToolkit(base.Store, pBase), pBase)
	if err != nil {
		t.Fatal(err)
	}
	pFX := isGoldenParams(withFX)
	fxTbl, err := rep.Run(ctx, reports.NewToolkit(withFX.Store, pFX), pFX)
	if err != nil {
		t.Fatal(err)
	}

	// Base statement: the FX line is SUPPRESSED (no exposure) — no golden movement.
	if _, ok := isTotalFor(baseTbl, "reports.income_statement.fx_gain_loss"); ok {
		t.Error("base income statement should NOT carry an FX line (no exposure)")
	}

	// With exposure: the FX line carries the −46,174 loss (the total column).
	fxLine, ok := isTotalFor(fxTbl, "reports.income_statement.fx_gain_loss")
	if !ok {
		t.Fatal("income statement with FX exposure is missing the FX line")
	}
	if fxLine != withFX.Expected.FX.RemeasurementUSDMinor {
		t.Errorf("FX line total = %d, want %d", fxLine, withFX.Expected.FX.RemeasurementUSDMinor)
	}

	// Articulation: the change in the report's "change in net assets" total equals the
	// change in the balance sheet's net assets (both driven by ExtendFX), because the FX
	// line is now included. Without p31.2 this would differ by the remeasurement.
	netBase, _ := isTotalFor(baseTbl, "reports.income_statement.net")
	netFX, _ := isTotalFor(fxTbl, "reports.income_statement.net")
	naDiff := netAssetsClosing(t, withFX, withFX.IDs.Root) - netAssetsClosing(t, base, base.IDs.Root)
	if netFX-netBase != naDiff {
		t.Errorf("income statement ΔNA change = %d, balance sheet net-asset change = %d (must articulate)",
			netFX-netBase, naDiff)
	}
	if naDiff != withFX.Expected.FX.EndingUSDMinor {
		t.Errorf("net-asset change = %d, want %d (closing value of residual Lempira balance)",
			naDiff, withFX.Expected.FX.EndingUSDMinor)
	}
}

// TestFXRemeasurementBaseFixtureNoIncome proves that WITHOUT the Lempira seam the base
// fixture recognizes ZERO remeasurement in income: its only foreign monetary item is the
// intercompany USD payable (excluded → CTA), so the income path is empty. This is the
// empirical basis for adding ExtendFX (the base fixture cannot demonstrate p31.2 alone).
func TestFXRemeasurementBaseFixtureNoIncome(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	tk := reports.NewToolkit(f.Store, reports.Params{Scope: f.IDs.Root, TargetCurrency: "USD"})
	got, err := tk.FXRemeasurementAsOf(ctx, reports.Scope{Sub: f.IDs.Root}, f.Expected.AsOf)
	if err != nil {
		t.Fatalf("FXRemeasurementAsOf: %v", err)
	}
	if len(got.Items) != 0 {
		t.Errorf("base fixture income-path items = %d, want 0; got %+v", len(got.Items), got.Items)
	}
	if got.ByFunctional["USD"] != 0 || got.ByFunctional["MXN"] != 0 {
		t.Errorf("base fixture remeasurement totals nonzero: %+v", got.ByFunctional)
	}
}

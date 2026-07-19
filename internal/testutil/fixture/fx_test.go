package fixture_test

import (
	"context"
	"math"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/testutil/fixture"
)

// TestExtendFXRemeasurement proves the Phase 31 ExtendFX seam produces the Lempira
// remeasurement scenario and that its hand-computed oracle (FXExpected) is reproduced
// from the STORE (native balance + on-or-before rates), independently of any report
// code. This is the income-path oracle the FX-detail report and the Statement-of-
// Activities FX line are later validated against, so it must stand on its own.
//
// Scenario (US sub, USD-functional): a 250,000.00 HNL contribution funds Banco Lempira
// (2025-03-15), then a 100,000.00 HNL Food Purchases expense is paid out of it
// (2025-09-20), leaving a 150,000.00 HNL residual. The HNL flows are measured at their
// transaction-date rates; the residual monetary balance remeasures to USD at the closing
// rate. The difference is the remeasurement FX loss recognized in income (ASC 830-20).
func TestExtendFXRemeasurement(t *testing.T) {
	f := fixture.New(t)

	// Opt-in: before the seam, FX is the zero value and Banco Lempira does not exist.
	if f.Expected.FX.Bank != 0 {
		t.Fatalf("FX populated before ExtendFX; seam should be opt-in")
	}

	f.ExtendFX(t)
	ctx := context.Background()
	fx := f.Expected.FX

	if fx.Bank == 0 {
		t.Fatal("ExtendFX did not capture BancoLempira id")
	}

	// (1) Native residual balance at AsOf, ROOT scope, must equal the oracle (a
	// double-entry tally the seam commits: 250,000.00 - 100,000.00 = 150,000.00 HNL).
	bals, err := f.Store.SubtreeBalancesAsOf(ctx, fx.AsOf, f.IDs.Root)
	if err != nil {
		t.Fatalf("SubtreeBalancesAsOf: %v", err)
	}
	var gotHNL int64
	var found bool
	for _, b := range bals {
		if b.AccountID == fx.Bank && b.Currency == "HNL" {
			gotHNL, found = b.Amount, true
		}
	}
	if !found {
		t.Fatal("Banco Lempira HNL balance not found at AsOf")
	}
	if gotHNL != fx.NativeHNL {
		t.Errorf("Banco Lempira native = %d HNL minor, want %d", gotHNL, fx.NativeHNL)
	}

	// (2) Ending balance converted to USD at the AsOf closing rate (HNL->USD reciprocal,
	// half-even) must equal the oracle EndingUSDMinor. Both currencies are exponent 2.
	closing, err := f.Store.RateOn(ctx, "HNL", "USD", fx.AsOf)
	if err != nil {
		t.Fatalf("RateOn HNL->USD AsOf: %v", err)
	}
	endingUSD := int64(math.RoundToEven(float64(gotHNL) * closing.Rate))
	if endingUSD != fx.EndingUSDMinor {
		t.Errorf("ending USD = %d minor, want %d", endingUSD, fx.EndingUSDMinor)
	}

	// (3) Remeasurement G/L, recomputed from the STORE's on-or-before transaction-date
	// rates: EndingUSD − (contribution@2025-03 − foodExpense@2025-09), all in USD.
	// This is the number recognized in the change in net assets (negative = loss).
	rContrib, err := f.Store.RateOn(ctx, "HNL", "USD", "2025-03-15")
	if err != nil {
		t.Fatalf("RateOn contribution date: %v", err)
	}
	rSpend, err := f.Store.RateOn(ctx, "HNL", "USD", "2025-09-20")
	if err != nil {
		t.Fatalf("RateOn spend date: %v", err)
	}
	contribUSD := float64(fixtureContributionHNL) * rContrib.Rate
	spendUSD := float64(fixtureFoodExpenseHNL) * rSpend.Rate
	netFlowUSD := contribUSD - spendUSD
	gl := int64(math.RoundToEven(float64(gotHNL)*closing.Rate - netFlowUSD))
	if gl != fx.RemeasurementUSDMinor {
		t.Errorf("remeasurement G/L = %d USD minor, want %d (loss $%.2f)",
			gl, fx.RemeasurementUSDMinor, float64(fx.RemeasurementUSDMinor)/100)
	}

	// (4) The seam is audit-neutral: ledger.Check still returns zero Errors and only the
	// fixture's one Z19 warning (Event Income deliberately unmapped).
	vs, err := ledger.Check(ctx, f.DB)
	if err != nil {
		t.Fatalf("ledger.Check after ExtendFX: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("unexpected Error violation after ExtendFX: %s: %s", v.Rule, v.Detail)
		}
		if v.Severity == ledger.Warning && v.Rule != "Z19" {
			t.Errorf("unexpected warning rule %s after ExtendFX (only Z19 expected)", v.Rule)
		}
	}
}

// The native HNL flow magnitudes, mirrored from synth so the test's independent G/L
// recomputation does not import the seam's derived oracle constants.
const (
	fixtureContributionHNL int64 = 25_000_000 // 250,000.00 HNL
	fixtureFoodExpenseHNL  int64 = 10_000_000 // 100,000.00 HNL
)

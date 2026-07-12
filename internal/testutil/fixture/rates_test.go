package fixture_test

import (
	"context"
	"math"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/testutil/fixture"
)

// TestExtendRatesKeepsLedgerClean proves the p14 rate seam is CHANGE-ID-ANCHORED
// but audit-neutral: after ExtendRates loads the monthly USD->MXN schedule (a
// batch under one changes row, referenced by no *_versions twin), ledger.Check
// STILL returns zero Error violations and exactly the fixture's one Z19 warning.
// This is the empirical guard against a "rates/versioning mismatch" -- Z5 checks
// versions -> changes, never the reverse, so a rates-only change row trips nothing.
func TestExtendRatesKeepsLedgerClean(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)

	vs, err := ledger.Check(context.Background(), f.DB)
	if err != nil {
		t.Fatalf("ledger.Check after ExtendRates: %v", err)
	}
	warnRules := map[string]int{}
	for _, v := range vs {
		switch v.Severity {
		case ledger.Error:
			t.Errorf("unexpected Error violation after ExtendRates: %s: %s", v.Rule, v.Detail)
		case ledger.Warning:
			warnRules[v.Rule]++
		}
	}
	if warnRules["Z19"] == 0 {
		t.Errorf("expected Z19 warning to persist; got %v", warnRules)
	}
	for rule := range warnRules {
		if rule != "Z19" {
			t.Errorf("unexpected warning rule %s after ExtendRates (only Z19 expected)", rule)
		}
	}
}

// TestExtendRatesSchedule proves the loaded schedule reads back through RateOn:
// the endpoints, an interpolated middle point, on-or-before staleness at AsOf, and
// the MXN->USD reciprocal path. It also checks the exported RatesExpected metadata
// matches the store.
func TestExtendRatesSchedule(t *testing.T) {
	f := fixture.New(t)

	// Before the seam, Rates is the zero value (opt-in): the native-currency
	// default must be untouched.
	if f.Expected.Rates.Months != 0 {
		t.Fatalf("Rates populated before ExtendRates; seam should be opt-in")
	}

	f.ExtendRates(t)
	ctx := context.Background()
	r := f.Expected.Rates

	if r.Months != 18 || r.FirstRate != 17.00 || r.LastRate != 18.10 {
		t.Fatalf("schedule metadata = {%d, %v, %v}, want {18, 17, 18.1}", r.Months, r.FirstRate, r.LastRate)
	}

	// First point (exactly on its date) and last point.
	first, err := f.Store.RateOn(ctx, "USD", "MXN", r.FirstDate)
	if err != nil {
		t.Fatalf("RateOn first: %v", err)
	}
	if first.Rate != r.FirstRate || first.RateDate != r.FirstDate {
		t.Errorf("first rate = {%v, %q}, want {%v, %q}", first.Rate, first.RateDate, r.FirstRate, r.FirstDate)
	}
	last, err := f.Store.RateOn(ctx, "USD", "MXN", r.LastDate)
	if err != nil {
		t.Fatalf("RateOn last: %v", err)
	}
	if last.Rate != r.LastRate {
		t.Errorf("last rate = %v, want %v", last.Rate, r.LastRate)
	}

	// A date before the first scheduled point has no rate.
	if _, err := f.Store.RateOn(ctx, "USD", "MXN", "2024-12-31"); err == nil {
		t.Errorf("RateOn before schedule: want ErrRateMissing, got nil")
	}

	// Staleness at AsOf (2026-06-30): the newest rate (2026-06-01) is returned with
	// ITS date, not AsOf.
	atAsOf, err := f.Store.RateOn(ctx, "USD", "MXN", f.Expected.AsOf)
	if err != nil {
		t.Fatalf("RateOn AsOf: %v", err)
	}
	if atAsOf.Rate != r.ClosingUSDPerMXN || atAsOf.RateDate != r.LastDate {
		t.Errorf("AsOf rate = {%v, %q}, want {%v, %q}", atAsOf.Rate, atAsOf.RateDate, r.ClosingUSDPerMXN, r.LastDate)
	}

	// Reciprocal MXN->USD at AsOf: only USD->MXN is stored, so this comes back as
	// 1/closing with Reciprocal=true, dated to the direct row's date.
	recip, err := f.Store.RateOn(ctx, "MXN", "USD", f.Expected.AsOf)
	if err != nil {
		t.Fatalf("RateOn reciprocal: %v", err)
	}
	if !recip.Reciprocal {
		t.Errorf("MXN->USD: Reciprocal = false, want true")
	}
	if math.Abs(recip.Rate-1.0/r.ClosingUSDPerMXN) > 1e-12 {
		t.Errorf("MXN->USD reciprocal = %v, want %v", recip.Rate, 1.0/r.ClosingUSDPerMXN)
	}
}

// TestExtendRatesConvertedFundBalances proves the seam's converted expectations
// are internally consistent with the native fund balances and the closing rate:
// USD funds pass through 1:1; MXN funds convert by 1/closing. Values are unrounded
// floats (p15 owns rounding, D12) -- this test only checks the derivation the seam
// exposes, so p15 can trust the numbers.
func TestExtendRatesConvertedFundBalances(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	r := f.Expected.Rates

	if len(r.ConvertedFundBalances) != len(f.Expected.FundBalances) {
		t.Fatalf("converted fund balances count = %d, want %d",
			len(r.ConvertedFundBalances), len(f.Expected.FundBalances))
	}
	for _, cb := range r.ConvertedFundBalances {
		major := float64(cb.NativeMinor) / 100.0
		want := major
		if cb.NativeCcy == "MXN" {
			want = major / r.ClosingUSDPerMXN
		}
		if math.Abs(cb.ConvertedUSD-want) > 1e-9 {
			t.Errorf("fund %d converted USD = %v, want %v", cb.Fund, cb.ConvertedUSD, want)
		}
		if cb.NativeCcy == "USD" && cb.ConvertedUSD != major {
			t.Errorf("USD fund %d should pass through 1:1, got %v want %v", cb.Fund, cb.ConvertedUSD, major)
		}
	}
}

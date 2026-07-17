package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
	"cuento/internal/synth"
)

// ExtendCapitalCampaign is the p26.51 CAPITAL-CAMPAIGN seam wrapper: it builds the
// campaign accounts + fund + multi-quarter, multi-currency lifecycle
// (synth.ExtendCapitalCampaign) and records the HAND-DERIVED per-currency oracle the
// Capital Campaign report golden asserts against. It is OPT-IN: New does NOT call it,
// so the base fixture and every existing golden/tally stay byte-identical.
//
// HAND-DERIVED per-currency figures (the report's oracle; every campaign split is
// tagged the fund so each transaction nets to zero WITHIN the fund):
//
//	USD (US subsidiary):
//	  Q1 2025-01-15 gift        Contributions -20,000.00 / Checking US +20,000.00
//	  Q1 2025-03-20 Land buy    Land          + 8,000.00 / Checking US - 8,000.00
//	  Q2 2025-06-01 supplies    Program Suppl + 1,500.00 / Checking US - 1,500.00
//	  Q3 2025-09-15 construct   Construction  + 5,000.00 / Checking US - 5,000.00
//	  Q3 2025-09-25 loan-fin    Construction  + 2,000.00 / Construction Loan - 2,000.00
//	  => Gross Revenue 22,000.00 (20,000 gift + 2,000 loan proceeds, a receipt D20) ;
//	     Gross Expense 1,500.00 ;
//	     Land 8,000.00 ; Construction 7,000.00 (Capitalized 15,000.00 -- the loan
//	     itself is NOT a capital asset) ;
//	     RNA (spendable) = 22,000 - 1,500 - 15,000 = 5,500.00
//
//	MXN (MX subsidiary):
//	  Q2 2025-05-10 grant       Contributions -100,000.00 / Checking MX +100,000.00
//	  Q3 2025-08-05 construct   Construction  + 60,000.00 / Checking MX - 60,000.00
//	  => Gross Revenue 100,000.00 ; Gross Expense 0 ;
//	     Land 0 ; Construction 60,000.00 (Capitalized 60,000.00) ;
//	     RNA (spendable) = 100,000 - 0 - 60,000 = 40,000.00
//
// The seam is designed so it can be layered WITH ExtendRates (the campaign
// transactions fall inside the 2025 rate schedule so every split has an on-or-before
// rate).
func (f *Fixture) ExtendCapitalCampaign(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	if err := synth.ExtendCapitalCampaign(ctx, f.Store, &f.IDs); err != nil {
		t.Fatalf("fixture: ExtendCapitalCampaign: %v", err)
	}

	f.Expected.Campaign = CampaignExpected{
		Fund:            f.IDs.Campaign,
		LandAccount:     f.IDs.CampaignLand,
		ConstrAccount:   f.IDs.Construction,
		FixedAssets:     f.IDs.FixedAssets,
		From:            "2025-01-01",
		To:              "2025-12-31",
		GrossRevenueUSD: 2_200_000, // 2,000,000 gift + 200,000 loan proceeds (a receipt, D20)
		GrossExpenseUSD: 150_000,
		LandUSD:         800_000,
		ConstructionUSD: 700_000, // 500,000 cash-paid + 200,000 loan-financed
		RNAUSD:          550_000, // 2,200,000 - 150,000 - (800,000 + 700,000)
		GrossRevenueMXN: 10_000_000,
		GrossExpenseMXN: 0,
		LandMXN:         0,
		ConstructionMXN: 6_000_000,
		RNAMXN:          4_000_000, // 10,000,000 - 0 - 6,000,000
	}
}

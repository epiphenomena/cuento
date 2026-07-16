package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
)

// ExtendCapitalCampaign is the p26.51 CAPITAL-CAMPAIGN seam: it adds a restricted
// capital-campaign fund ("Restore the Way") plus its capital accounts (a Fixed
// Assets placeholder parent with a Land leaf and a Construction leaf) and posts a
// multi-quarter, multi-currency (USD + MXN) campaign lifecycle -- restricted
// revenue partly DEPLOYED into a Land purchase and a Construction (fixed-asset)
// purchase across three quarters, leaving an unspent restricted (spendable)
// balance. It is the data the Capital Campaign report's golden asserts against.
//
// Like ExtendRates / ExtendReconciliation it is OPT-IN: New does NOT call it, so
// the base fixture and every existing golden/tally stay byte-identical. A test
// that wants the campaign calls it explicitly; the capital_campaign report golden
// is the only artifact that changes.
//
// HAND-DERIVED per-currency figures (the report's oracle; every campaign split is
// tagged the fund so each transaction nets to zero WITHIN the fund):
//
//	USD (US subsidiary):
//	  Q1 2025-01-15 gift        Contributions -20,000.00 / Checking US +20,000.00
//	  Q1 2025-03-20 Land buy    Land          + 8,000.00 / Checking US - 8,000.00
//	  Q2 2025-06-01 supplies    Program Suppl + 1,500.00 / Checking US - 1,500.00
//	  Q3 2025-09-15 construct   Construction  + 5,000.00 / Checking US - 5,000.00
//	  => Gross Revenue 20,000.00 ; Gross Expense 1,500.00 ;
//	     Land 8,000.00 ; Construction 5,000.00 (Capitalized 13,000.00) ;
//	     RNA (spendable) = 20,000 - 1,500 - 13,000 = 5,500.00
//
//	MXN (MX subsidiary):
//	  Q2 2025-05-10 grant       Contributions -100,000.00 / Checking MX +100,000.00
//	  Q3 2025-08-05 construct   Construction  + 60,000.00 / Checking MX - 60,000.00
//	  => Gross Revenue 100,000.00 ; Gross Expense 0 ;
//	     Land 0 ; Construction 60,000.00 (Capitalized 60,000.00) ;
//	     RNA (spendable) = 100,000 - 0 - 60,000 = 40,000.00
//
// The seam is designed so it can be layered WITH ExtendRates (call ExtendRates too
// for a converted-currency report run); the campaign transactions fall inside the
// 2025 rate schedule so every split has an on-or-before rate.
func (f *Fixture) ExtendCapitalCampaign(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), systemActor)
	ids := &f.IDs

	// --- accounts: a Fixed Assets placeholder parent with Land + Construction leaves.
	// Both US-mapped USD leaves except Construction is multi-sub (US+MX) so it can
	// carry an MXN purchase in the MX subsidiary. Land is the campus.py Land line;
	// Construction is a fixed-asset rollup member.
	fa, err := f.Store.CreateAccount(ctx, store.CreateAccountInput{
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Fixed Assets", "es": "Activos fijos"},
		Subsidiaries:    []int64{ids.Root, ids.US, ids.MX},
	})
	if err != nil {
		t.Fatalf("fixture: create Fixed Assets parent: %v", err)
	}
	ids.FixedAssets = fa

	land, err := f.Store.CreateAccount(ctx, store.CreateAccountInput{
		ParentID:        &fa,
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Land", "es": "Terreno"},
		Subsidiaries:    []int64{ids.US},
	})
	if err != nil {
		t.Fatalf("fixture: create Land account: %v", err)
	}
	ids.CampaignLand = land

	constr, err := f.Store.CreateAccount(ctx, store.CreateAccountInput{
		ParentID:        &fa,
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Construction in Progress", "es": "Construccion en proceso"},
		Subsidiaries:    []int64{ids.US, ids.MX},
	})
	if err != nil {
		t.Fatalf("fixture: create Construction account: %v", err)
	}
	ids.Construction = constr

	// --- the restricted campaign fund, spanning US + MX (so it holds USD and MXN).
	fund, err := f.Store.CreateFund(ctx, store.CreateFundInput{
		Name:         "Restore the Way",
		Funder:       "Capital Campaign Donors",
		Purpose:      "Restore the Way capital campaign",
		Restriction:  "purpose",
		Subsidiaries: []int64{ids.US, ids.MX},
	})
	if err != nil {
		t.Fatalf("fixture: create campaign fund: %v", err)
	}
	ids.Campaign = fund

	genProg := ids.General

	// --- Q1 2025: a gift and a Land purchase (USD).
	post(t, ctx, f.Store, "2025-01-15", ids.US, "USD", "Campaign gift Q1",
		sp{acct: ids.CheckingUS, amount: 2_000_000, fund: &fund, desc: "Capital campaign gift"},
		sp{acct: ids.Contributions, amount: -2_000_000, fund: &fund, prog: &genProg},
	)
	post(t, ctx, f.Store, "2025-03-20", ids.US, "USD", "Campaign land purchase",
		sp{acct: ids.CampaignLand, amount: 800_000, fund: &fund},
		sp{acct: ids.CheckingUS, amount: -800_000, fund: &fund, desc: "Land acquisition"},
	)

	// --- Q2 2025: an MXN grant and a USD campaign expense.
	post(t, ctx, f.Store, "2025-05-10", ids.MX, "MXN", "Campaign grant Q2",
		sp{acct: ids.CheckingMX, amount: 10_000_000, fund: &fund, desc: "Restricted campaign grant"},
		sp{acct: ids.Contributions, amount: -10_000_000, fund: &fund, prog: &genProg},
	)
	post(t, ctx, f.Store, "2025-06-01", ids.US, "USD", "Campaign supplies",
		sp{acct: ids.ProgramSupplies, amount: 150_000, fund: &fund, prog: &ids.Educacion},
		sp{acct: ids.CheckingUS, amount: -150_000, fund: &fund, desc: "Campaign supplies payment"},
	)

	// --- Q3 2025: construction purchases in both currencies.
	post(t, ctx, f.Store, "2025-08-05", ids.MX, "MXN", "Construction draw (MX)",
		sp{acct: ids.Construction, amount: 6_000_000, fund: &fund},
		sp{acct: ids.CheckingMX, amount: -6_000_000, fund: &fund, desc: "Construction contractor (MX)"},
	)
	post(t, ctx, f.Store, "2025-09-15", ids.US, "USD", "Construction draw (US)",
		sp{acct: ids.Construction, amount: 500_000, fund: &fund},
		sp{acct: ids.CheckingUS, amount: -500_000, fund: &fund, desc: "Construction contractor (US)"},
	)

	f.Expected.Campaign = CampaignExpected{
		Fund:            fund,
		LandAccount:     land,
		ConstrAccount:   constr,
		FixedAssets:     fa,
		From:            "2025-01-01",
		To:              "2025-12-31",
		GrossRevenueUSD: 2_000_000,
		GrossExpenseUSD: 150_000,
		LandUSD:         800_000,
		ConstructionUSD: 500_000,
		RNAUSD:          550_000, // 2,000,000 - 150,000 - (800,000 + 500,000)
		GrossRevenueMXN: 10_000_000,
		GrossExpenseMXN: 0,
		LandMXN:         0,
		ConstructionMXN: 6_000_000,
		RNAMXN:          4_000_000, // 10,000,000 - 0 - 6,000,000
	}
}

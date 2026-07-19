package synth

import (
	"context"
	"fmt"

	entids "cuento/internal/ids"
	"cuento/internal/store"
)

// RatesSource is the source string every seam rate row carries. Synthetic, so it is
// distinguishable from a real provider's rows and honest that these are synthetic
// data (rule 11).
const RatesSource = "fixture"

// rateSchedule constants: 18 monthly USD->MXN points, first 2025-01-01 @ 17.00,
// last 2026-06-01 @ 18.10 -- spanning the transaction range (2025-01 .. 2026-06).
// Deterministic, no clock/network. The i-th point (i=0..17) is the exact linear
// interpolation FirstRate + i*(LastRate-FirstRate)/(RateMonths-1).
const (
	RateMonths = 18
	FirstRate  = 17.00
	LastRate   = 18.10
)

// RateSchedule returns the deterministic monthly USD->MXN rate rows (18 points, one
// per month 2025-01 .. 2026-06). Exported so the fixture seam can both store them
// and derive its converted-balance oracle from the same source of truth.
func RateSchedule() []store.Rate {
	rates := make([]store.Rate, 0, RateMonths)
	y, m := 2025, 1
	for i := 0; i < RateMonths; i++ {
		date := fmt.Sprintf("%04d-%02d-01", y, m)
		val := FirstRate + float64(i)*(LastRate-FirstRate)/float64(RateMonths-1)
		rates = append(rates, store.Rate{
			RateDate: date,
			Base:     "USD",
			Quote:    "MXN",
			Value:    val,
			Source:   RatesSource,
		})
		if m == 12 {
			y, m = y+1, 1
		} else {
			m++
		}
	}
	return rates
}

// ExtendRates loads the deterministic monthly USD->MXN rate schedule via the store's
// audited PutRates (ONE change for the whole batch). It is OPT-IN: Build does not
// call it. Only USD->MXN rows are stored (18 points); MXN->USD conversions use the
// reciprocal (RateOn's fallback).
func ExtendRates(ctx context.Context, s *store.Store) error {
	if err := s.PutRates(ctx, RateSchedule()); err != nil {
		return fmt.Errorf("ExtendRates PutRates: %w", err)
	}
	return nil
}

// ReconStatementBalance is the 2026-05-31 Checking US (USD) statement balance the
// reconciliation seam finalizes to. It equals the Checking US USD live balance
// (3,593,500) with the two UNCLEARED items backed out: + 155,000 (May rent, a credit
// left out) - 75,000 (June donation, a debit left out) = 3,673,500. Opening is 0
// (first finalized recon on the pair). Exported so both the seam and the fixture
// oracle agree on one number.
const ReconStatementBalance int64 = 3_673_500

// ExtendReconciliation finalizes the 2026-05-31 Checking US (USD) reconciliation over
// the account's restricted AND unrestricted splits (the D13/D20 payoff -- one
// statement spans all funds), leaving EXACTLY the two 2026-05-25 / 2026-06-10 items
// uncleared, WITHOUT renumbering any transaction. It is OPT-IN: Build does not call
// it. It sets ids.CheckingUSRecon to the created reconciliation id and returns the
// number of cleared splits (the oracle needs it).
//
// It uses the store lifecycle end to end: StartReconciliation on Checking US/USD,
// SetSplitReconciled(on) for every live non-deleted Checking US USD split EXCEPT the
// two on the captured uncleared txns, then Finalize at ReconStatementBalance. Live
// split ids are QUERIED (not hardcoded): the edited txn's Checking US split is a
// 3rd-generation id, so a live query is the only deterministic source. The read goes
// through the store's SplitsByAccountCurrency (sqlc, ORDER BY s.id) -- rule 2, no raw
// SQL in the shipped binary -- so the seam threads no *sql.DB (all reads and writes
// stay in the store).
func ExtendReconciliation(ctx context.Context, s *store.Store, ids *IDs) (clearedCount int, err error) {
	reconID, err := s.StartReconciliation(ctx, ids.CheckingUS, "USD", "2026-05-31", ReconStatementBalance)
	if err != nil {
		return 0, fmt.Errorf("StartReconciliation: %w", err)
	}
	ids.CheckingUSRecon = reconID

	// Every live, non-deleted Checking US split on a USD transaction, plus the id of
	// its transaction (so we can skip the two uncleared ones), ordered by split id.
	splits, err := s.SplitsByAccountCurrency(ctx, ids.CheckingUS, "USD")
	if err != nil {
		return 0, fmt.Errorf("load Checking US splits: %w", err)
	}

	skip := map[entids.TransactionID]bool{ids.MayRentTxn: true, ids.JuneDonationTxn: true}
	var toClear []int64
	for _, sp := range splits {
		if skip[sp.TransactionID] {
			continue
		}
		toClear = append(toClear, sp.ID)
	}

	for _, splitID := range toClear {
		if err := s.SetSplitReconciled(ctx, reconID, splitID, true); err != nil {
			return 0, fmt.Errorf("clear split %d: %w", splitID, err)
		}
	}

	if err := s.Finalize(ctx, reconID); err != nil {
		return 0, fmt.Errorf("finalize reconciliation: %w", err)
	}
	return len(toClear), nil
}

// ExtendCapitalCampaign adds a restricted capital-campaign fund ("Restore the Way")
// plus its capital accounts (a Fixed Assets placeholder parent with a Land leaf and
// a Construction leaf) and posts a multi-quarter, multi-currency (USD + MXN) campaign
// lifecycle -- restricted revenue partly DEPLOYED into a Land purchase and a
// Construction (fixed-asset) purchase across three quarters, leaving an unspent
// restricted (spendable) balance. It is OPT-IN: Build does not call it. It captures
// the created ids into ids (Campaign/FixedAssets/CampaignLand/Construction/ConstrLoan).
func ExtendCapitalCampaign(ctx context.Context, s *store.Store, ids *IDs) error {
	// --- accounts: a Fixed Assets placeholder parent with Land + Construction leaves.
	fa, err := s.CreateAccount(ctx, store.CreateAccountInput{
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Fixed Assets", "es": "Activos fijos"},
		Subsidiaries:    []entids.SubsidiaryID{ids.Root, ids.US, ids.MX},
	})
	if err != nil {
		return fmt.Errorf("create Fixed Assets parent: %w", err)
	}
	ids.FixedAssets = fa

	land, err := s.CreateAccount(ctx, store.CreateAccountInput{
		ParentID:        &fa,
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Land", "es": "Terreno"},
		Subsidiaries:    []entids.SubsidiaryID{ids.US},
	})
	if err != nil {
		return fmt.Errorf("create Land account: %w", err)
	}
	ids.CampaignLand = land

	constr, err := s.CreateAccount(ctx, store.CreateAccountInput{
		ParentID:        &fa,
		Type:            "asset",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Construction in Progress", "es": "Construccion en proceso"},
		Subsidiaries:    []entids.SubsidiaryID{ids.US, ids.MX},
	})
	if err != nil {
		return fmt.Errorf("create Construction account: %w", err)
	}
	ids.Construction = constr

	// A construction-loan LIABILITY that DIRECTLY financed a Construction purchase
	// (DR Construction / CR Construction Loan -- no cash leg). The loan credit is a
	// receipt (Received / Gross Revenue), NOT Capitalized.
	loan, err := s.CreateAccount(ctx, store.CreateAccountInput{
		Type:            "liability",
		DefaultCurrency: "USD",
		Names:           map[string]string{"en": "Construction Loan", "es": "Prestamo de construccion"},
		Subsidiaries:    []entids.SubsidiaryID{ids.US},
	})
	if err != nil {
		return fmt.Errorf("create Construction Loan account: %w", err)
	}
	ids.ConstrLoan = loan

	// --- the restricted campaign fund, spanning US + MX (so it holds USD and MXN).
	fund, err := s.CreateFund(ctx, store.CreateFundInput{
		Name:         "Restore the Way",
		Funder:       "Capital Campaign Donors",
		Purpose:      "Restore the Way capital campaign",
		Restriction:  "purpose",
		Subsidiaries: []entids.SubsidiaryID{ids.US, ids.MX},
	})
	if err != nil {
		return fmt.Errorf("create campaign fund: %w", err)
	}
	ids.Campaign = fund

	genProg := ids.General

	posts := []struct {
		date   string
		sub    entids.SubsidiaryID
		ccy    string
		memo   string
		splits []sp
	}{
		// Q1 2025: a gift and a Land purchase (USD).
		{"2025-01-15", ids.US, "USD", "Campaign gift Q1", []sp{
			{acct: ids.CheckingUS, amount: 2_000_000, fund: &fund, desc: "Capital campaign gift"},
			{acct: ids.Contributions, amount: -2_000_000, fund: &fund, prog: &genProg},
		}},
		{"2025-03-20", ids.US, "USD", "Campaign land purchase", []sp{
			{acct: ids.CampaignLand, amount: 800_000, fund: &fund},
			{acct: ids.CheckingUS, amount: -800_000, fund: &fund, desc: "Land acquisition"},
		}},
		// Q2 2025: an MXN grant and a USD campaign expense.
		{"2025-05-10", ids.MX, "MXN", "Campaign grant Q2", []sp{
			{acct: ids.CheckingMX, amount: 10_000_000, fund: &fund, desc: "Restricted campaign grant"},
			{acct: ids.Contributions, amount: -10_000_000, fund: &fund, prog: &genProg},
		}},
		{"2025-06-01", ids.US, "USD", "Campaign supplies", []sp{
			{acct: ids.ProgramSupplies, amount: 150_000, fund: &fund, prog: &ids.Educacion},
			{acct: ids.CheckingUS, amount: -150_000, fund: &fund, desc: "Campaign supplies payment"},
		}},
		// Q3 2025: construction purchases in both currencies.
		{"2025-08-05", ids.MX, "MXN", "Construction draw (MX)", []sp{
			{acct: ids.Construction, amount: 6_000_000, fund: &fund},
			{acct: ids.CheckingMX, amount: -6_000_000, fund: &fund, desc: "Construction contractor (MX)"},
		}},
		{"2025-09-15", ids.US, "USD", "Construction draw (US)", []sp{
			{acct: ids.Construction, amount: 500_000, fund: &fund},
			{acct: ids.CheckingUS, amount: -500_000, fund: &fund, desc: "Construction contractor (US)"},
		}},
		// Q3 2025: a construction purchase DIRECTLY financed by a loan (no cash leg).
		{"2025-09-25", ids.US, "USD", "Loan-financed construction", []sp{
			{acct: ids.Construction, amount: 200_000, fund: &fund},
			{acct: ids.ConstrLoan, amount: -200_000, fund: &fund},
		}},
	}
	for _, p := range posts {
		if _, err := post(ctx, s, p.date, p.sub, p.ccy, p.memo, p.splits...); err != nil {
			return err
		}
	}
	return nil
}

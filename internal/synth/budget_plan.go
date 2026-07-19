package synth

import (
	"context"
	"fmt"

	entids "cuento/internal/ids"
	"cuento/internal/store"
)

// SampleBudgetPlanName is the name the sample budget-PLAN seam gives its plan.
// Exported so a caller can locate it (and so it is documented as the one synthetic
// budget-plan name).
const SampleBudgetPlanName = "Sample Cash-Flow Plan"

// SampleBudgetPlanSplits is the number of budget-splits ExtendSampleBudgetPlan
// creates.
const SampleBudgetPlanSplits = 8

// ExtendSampleBudgetPlan creates a sample budget PLAN (the p27.2 split-derived
// model) scoped to the US subsidiary, plus several PROJECTED, dated budget-splits
// across >=2 programs, mixing R/E legs with one open_item A/R leg, on VARIED dates
// -- so the p27.3 cash-flow projection and budget-variance reports have data. It is
// OPT-IN: Build does not call it; the demo generator and the fixture seam call it. It
// sets ids.SampleBudgetPlan to the created plan id.
//
// This is the split-derived model (the old schedule-based sample was retired in
// p27.3). Each
// split carries an explicit date (no schedule object); R/E legs name a program
// (required; prefilled where an account default exists), the open_item A/R leg
// carries NONE (forbidden on A/L). Currency is the resolved account's default so a
// split always references an existing currency. Amounts are SYNTHETIC round figures
// (rule 11).
func ExtendSampleBudgetPlan(ctx context.Context, s *store.Store, ids *IDs) error {
	planID, err := s.CreateBudgetPlan(ctx, store.BudgetPlanInput{
		Name:         SampleBudgetPlanName,
		SubsidiaryID: ids.US,
		Notes:        "Synthetic sample cash-flow plan (split-derived budgeting).",
	})
	if err != nil {
		return fmt.Errorf("create sample budget plan: %w", err)
	}
	ids.SampleBudgetPlan = planID

	general := ids.General
	educacion := ids.Educacion
	buildingFund := ids.BuildingFund

	splits := []struct {
		desc    string
		date    string
		account entids.AccountID
		fund    *entids.FundID
		program *entids.ProgramID // nil => rely on account default (R/E) or none (A/L)
		amount  int64
	}{
		// Revenue, General program, unrestricted -- monthly-ish inflows, varied dates.
		{"Individual gifts Q1", "2026-01-15", ids.Contributions, nil, &general, 400_000},
		{"Individual gifts Q2", "2026-04-15", ids.Contributions, nil, &general, 350_000},
		// Revenue, Educacion (ProgramFees default program) -- program PREFILLED.
		{"Program fees spring", "2026-03-01", ids.ProgramFees, nil, nil, 120_000},
		// Expense, General -- payroll-ish, restricted to BuildingFund on occupancy.
		{"Payroll Feb", "2026-02-01", ids.Salaries, nil, &general, 180_000},
		{"Payroll May", "2026-05-01", ids.Salaries, nil, &general, 185_000},
		// Expense, Educacion program, restricted to BuildingFund.
		{"Rent Q1 (building fund)", "2026-01-01", ids.Occupancy, &buildingFund, &educacion, 90_000},
		{"Rent Q2 (building fund)", "2026-04-01", ids.Occupancy, &buildingFund, &educacion, 90_000},
		// Open-item A/R leg (DueFromMX, open_item asset): an EXPECTED COLLECTION. NO
		// program (forbidden on A/L). desc is the party name (design #2).
		{"RV Mexico settlement", "2026-06-01", ids.DueFromMX, nil, nil, 250_000},
	}

	for _, sp := range splits {
		acct, err := s.GetAccount(ctx, sp.account)
		if err != nil {
			return fmt.Errorf("get sample budget-plan account %d: %w", sp.account, err)
		}
		if _, err := s.CreateBudgetSplit(ctx, planID, store.BudgetSplitInput{
			Description: sp.desc,
			Date:        sp.date,
			AccountID:   sp.account,
			FundID:      sp.fund,
			ProgramID:   sp.program,
			Amount:      sp.amount,
			Currency:    acct.DefaultCurrency,
		}); err != nil {
			return fmt.Errorf("create sample budget-split (%q): %w", sp.desc, err)
		}
	}
	return nil
}

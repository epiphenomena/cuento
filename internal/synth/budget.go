package synth

import (
	"context"
	"database/sql"
	"fmt"

	"cuento/internal/store"
)

// queryer is the subset of *sql.DB the reconciliation seam needs to READ live split
// ids (a builder wiring read, not an app write path -- all writes go through the
// store). *sql.DB satisfies it.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SampleBudgetName is the name the sample-budget seam gives its budget. Exported so a
// caller can locate it (and so it is documented as the one synthetic budget name).
const SampleBudgetName = "Sample Operating Budget"

// SampleBudgetLines is the number of budget lines ExtendSampleBudget creates.
const SampleBudgetLines = 6

// ExtendSampleBudget creates a sample operating budget (a budget + several budget
// lines across a few programs / R-E accounts / funds / subsidiaries, on the seeded
// COMMON schedules, with SYNTHETIC amounts) so the budget-group reports
// (actuals_vs_budget / cashflow_projection) have a budget to exercise. It is OPT-IN:
// Build does not call it. It sets ids.SampleBudget to the created budget id.
//
// The lines mix budgeted-only and budgeted-with-actual so the reports show BOTH a
// plan view AND real variance. Each line's currency is the resolved ACCOUNT's default
// currency, so a line always references an existing currency. Schedules are RESOLVED
// BY NAME from the seeded set (p26.28's 00022 + p26.79's 00025), NOT created here.
// Amounts are SYNTHETIC round figures (rule 11).
func ExtendSampleBudget(ctx context.Context, s *store.Store, ids *IDs) error {
	const from, to = "2026-01-01", "2026-12-31"

	budgetID, err := s.CreateBudget(ctx, store.BudgetInput{
		Name:        SampleBudgetName,
		PeriodStart: from,
		PeriodEnd:   to,
		Notes:       "Synthetic sample operating budget.",
	})
	if err != nil {
		return fmt.Errorf("create sample budget: %w", err)
	}
	ids.SampleBudget = budgetID

	// Resolve the seeded schedules by name.
	names := []string{"Monthly (1st)", "Monthly (15th)", "Semimonthly (1st & 15th)", "Biweekly", "Weekly (Monday)", "Annual (Jan 1)"}
	sched := make(map[string]int64, len(names))
	for _, n := range names {
		id, err := scheduleByName(ctx, s, n)
		if err != nil {
			return err
		}
		sched[n] = id
	}

	beca := ids.BecaAgua

	lines := []struct {
		sub     int64
		account int64
		fund    *int64
		program int64
		amount  int64
		sched   int64
	}{
		// Revenue, US, unrestricted, General, monthly-on-the-1st.
		{ids.US, ids.Contributions, nil, ids.General, 500_000, sched["Monthly (1st)"]},
		// Revenue, MX, BecaAgua (restricted), Educacion, semimonthly.
		{ids.MX, ids.GovernmentGrants, &beca, ids.Educacion, 250_000, sched["Semimonthly (1st & 15th)"]},
		// Expense, US, unrestricted, General, biweekly (payroll cadence).
		{ids.US, ids.Salaries, nil, ids.General, 180_000, sched["Biweekly"]},
		// Expense, MX, BecaAgua (restricted), Educacion, monthly-on-the-15th.
		{ids.MX, ids.ProgramSupplies, &beca, ids.Educacion, 40_000, sched["Monthly (15th)"]},
		// Expense, MX, unrestricted, Food Pantry, weekly on Monday.
		{ids.MX, ids.FoodPurchases, nil, ids.FoodPantry, 15_000, sched["Weekly (Monday)"]},
		// Expense, US, unrestricted, General, annual.
		{ids.US, ids.Occupancy, nil, ids.General, 1_200_000, sched["Annual (Jan 1)"]},
	}

	for _, ln := range lines {
		acct, err := s.GetAccount(ctx, ln.account)
		if err != nil {
			return fmt.Errorf("get sample-budget account %d: %w", ln.account, err)
		}
		if _, err := s.CreateBudgetLine(ctx, budgetID, store.BudgetLineInput{
			SubsidiaryID: ln.sub,
			AccountID:    ln.account,
			FundID:       ln.fund,
			ProgramID:    ln.program,
			Amount:       ln.amount,
			Currency:     acct.DefaultCurrency,
			ScheduleID:   ln.sched,
		}); err != nil {
			return fmt.Errorf("create sample budget line (account %d): %w", ln.account, err)
		}
	}
	return nil
}

// scheduleByName resolves a seeded schedule by name from the store, erroring if it is
// missing (the migrations seed it, so absence means the db was not migrated).
func scheduleByName(ctx context.Context, s *store.Store, name string) (int64, error) {
	rows, err := s.ListSchedules(ctx)
	if err != nil {
		return 0, fmt.Errorf("list schedules: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, fmt.Errorf("seeded schedule %q not found (db not migrated?)", name)
}

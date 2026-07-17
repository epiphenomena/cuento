package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
)

// SampleBudgetName is the name the sample-budget seam gives its budget. Exported so
// a test can locate it (and so it is documented as the one synthetic budget name).
const SampleBudgetName = "Sample Operating Budget"

// ExtendSampleBudget is the p26.80 SAMPLE-BUDGET seam: it creates a sample operating
// budget (a budget + several budget lines across a few programs / R-E accounts /
// funds / subsidiaries, on the p26.79 seeded COMMON schedules, with SYNTHETIC
// amounts) so the budget-group reports (actuals_vs_budget / cashflow_projection)
// finally have a budget to exercise. A prior report audit found those reports had no
// fixture budget to run over -- each report test had to build one inline; this seam
// gives them a canonical, opt-in one.
//
// Like ExtendRates / ExtendReconciliation / ExtendCapitalCampaign it is OPT-IN: New
// does NOT call it, so the base fixture and every existing golden/tally stay
// byte-identical. A test that wants the budget calls it explicitly; only the
// seam-driven budget goldens (actuals_vs_budget_sample / cashflow_projection_sample)
// change.
//
// The lines mix budgeted-only and budgeted-with-actual so the golden shows BOTH a
// plan view AND real variance. Each line's currency is the resolved ACCOUNT's default
// currency (all these fixture R/E accounts default to USD except Food Purchases,
// which is MXN), so a line always references an existing currency. Because a budget
// key includes currency, the USD unrestricted lines (Salaries, Occupancy,
// Contributions -- all posted in USD in the fixture's 2026-H1 window) surface actual
// activity and non-zero VARIANCE (e.g. Salaries +8,500 in Feb, Occupancy +1,550 in
// May, Contributions -750 in Jun); the restricted BecaAgua lines are USD-budgeted
// while the fixture's BecaAgua activity is MXN, so those read as pure PLAN (actual 0)
// -- an intentional plan/actual mix, not an error. Amounts are SYNTHETIC round
// figures (rule 11), never copied from any real budget.
//
// Schedules are RESOLVED BY NAME from the seeded set (p26.28's 00022 + p26.79's
// 00025), NOT created here -- so the seam exercises the seeded schedules and stays
// aligned with them. The seeded schedules use weekend_adjust=prev_business_day, so a
// day-of-month occurrence landing on a Sat/Sun rolls back to the prior Friday (which
// can cross a month boundary): that is why "Monthly (1st)" Contributions doubles in
// Jan/Jul/Oct and is absent in Feb/Aug/Nov in the golden (Feb/Aug/Nov 1 2026 are a
// weekend, rolling into the prior month). Correct behavior, visible in the golden.
func (f *Fixture) ExtendSampleBudget(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), systemActor)
	ids := &f.IDs

	const from, to = "2026-01-01", "2026-12-31"

	budgetID, err := f.Store.CreateBudget(ctx, store.BudgetInput{
		Name:        SampleBudgetName,
		PeriodStart: from,
		PeriodEnd:   to,
		Notes:       "Synthetic sample operating budget (fixture seam).",
	})
	if err != nil {
		t.Fatalf("fixture: create sample budget: %v", err)
	}
	ids.SampleBudget = budgetID

	// Resolve the seeded schedules by name (they are seeded by the 00022/00025
	// migrations New() applies, so they always exist on the fixture db).
	monthly1 := scheduleID(t, f, "Monthly (1st)")
	monthly15 := scheduleID(t, f, "Monthly (15th)")
	semi := scheduleID(t, f, "Semimonthly (1st & 15th)")
	biweekly := scheduleID(t, f, "Biweekly")
	weeklyMon := scheduleID(t, f, "Weekly (Monday)")
	annual := scheduleID(t, f, "Annual (Jan 1)")

	beca := ids.BecaAgua

	// Each entry: subsidiary, R-E account, optional fund, program, per-occurrence
	// SYNTHETIC amount (positive magnitude, minor units), schedule. Currency is the
	// account's own default currency (resolved below), so it always exists.
	lines := []struct {
		sub     int64
		account int64
		fund    *int64
		program int64
		amount  int64
		sched   int64
	}{
		// Revenue, US, unrestricted, General, monthly-on-the-1st.
		{ids.US, ids.Contributions, nil, ids.General, 500_000, monthly1},
		// Revenue, MX, BecaAgua (restricted), Educacion, semimonthly. USD-budgeted
		// (the account default); the fixture's BecaAgua activity is MXN, so this reads
		// as pure plan (a restricted budget-vs-nothing row).
		{ids.MX, ids.GovernmentGrants, &beca, ids.Educacion, 250_000, semi},
		// Expense, US, unrestricted, General, biweekly (payroll cadence).
		{ids.US, ids.Salaries, nil, ids.General, 180_000, biweekly},
		// Expense, MX, BecaAgua (restricted), Educacion, monthly-on-the-15th.
		{ids.MX, ids.ProgramSupplies, &beca, ids.Educacion, 40_000, monthly15},
		// Expense, MX, unrestricted, Food Pantry, weekly on Monday.
		{ids.MX, ids.FoodPurchases, nil, ids.FoodPantry, 15_000, weeklyMon},
		// Expense, US, unrestricted, General, annual.
		{ids.US, ids.Occupancy, nil, ids.General, 1_200_000, annual},
	}

	for _, ln := range lines {
		acct, err := f.Store.GetAccount(ctx, ln.account)
		if err != nil {
			t.Fatalf("fixture: get sample-budget account %d: %v", ln.account, err)
		}
		if _, err := f.Store.CreateBudgetLine(ctx, budgetID, store.BudgetLineInput{
			SubsidiaryID: ln.sub,
			AccountID:    ln.account,
			FundID:       ln.fund,
			ProgramID:    ln.program,
			Amount:       ln.amount,
			Currency:     acct.DefaultCurrency,
			ScheduleID:   ln.sched,
		}); err != nil {
			t.Fatalf("fixture: create sample budget line (account %d): %v", ln.account, err)
		}
	}

	f.Expected.SampleBudget = SampleBudgetExpected{
		Budget: budgetID,
		From:   from,
		To:     to,
		Lines:  len(lines),
	}
}

// scheduleID resolves a seeded schedule by name from the store, failing the test if
// it is missing (the migrations seed it, so absence is a fixture-infrastructure bug).
func scheduleID(t *testing.T, f *Fixture, name string) int64 {
	t.Helper()
	rows, err := f.Store.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("fixture: list schedules: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("fixture: seeded schedule %q not found", name)
	return 0
}

package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
	"cuento/internal/synth"
)

// SampleBudgetName is the name the sample-budget seam gives its budget. Re-exported
// from synth so a test can locate it (and so it is documented as the one synthetic
// budget name).
const SampleBudgetName = synth.SampleBudgetName

// ExtendSampleBudget is the p26.80 SAMPLE-BUDGET seam wrapper: it creates the sample
// operating budget + lines (synth.ExtendSampleBudget) and records the expected shape.
// A prior report audit found the budget-group reports (actuals_vs_budget /
// cashflow_projection) had no fixture budget to run over; this seam gives them a
// canonical, opt-in one. It is OPT-IN: New does NOT call it, so the base fixture and
// every existing golden/tally stay byte-identical.
//
// The lines mix budgeted-only and budgeted-with-actual so the golden shows BOTH a
// plan view AND real variance. Each line's currency is the resolved ACCOUNT's default
// currency (all these R/E accounts default to USD except Food Purchases, which is
// MXN). Because a budget key includes currency, the USD unrestricted lines (Salaries,
// Occupancy, Contributions -- all posted in USD in the 2026-H1 window) surface actual
// activity and non-zero VARIANCE; the restricted BecaAgua lines are USD-budgeted while
// the BecaAgua activity is MXN, so those read as pure PLAN (actual 0). Amounts are
// SYNTHETIC round figures (rule 11).
//
// Schedules are RESOLVED BY NAME from the seeded set (p26.28's 00022 + p26.79's
// 00025). The seeded schedules use weekend_adjust=prev_business_day, so a day-of-month
// occurrence landing on a Sat/Sun rolls back to the prior Friday (which can cross a
// month boundary): that is why "Monthly (1st)" Contributions doubles in Jan/Jul/Oct
// and is absent in Feb/Aug/Nov in the golden. Correct behavior, visible in the golden.
func (f *Fixture) ExtendSampleBudget(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	if err := synth.ExtendSampleBudget(ctx, f.Store, &f.IDs); err != nil {
		t.Fatalf("fixture: ExtendSampleBudget: %v", err)
	}

	f.Expected.SampleBudget = SampleBudgetExpected{
		Budget: f.IDs.SampleBudget,
		From:   "2026-01-01",
		To:     "2026-12-31",
		Lines:  synth.SampleBudgetLines,
	}
}

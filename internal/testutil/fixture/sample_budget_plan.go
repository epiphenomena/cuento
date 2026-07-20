package fixture

import (
	"context"
	"testing"

	"cuento/internal/store"
	"cuento/internal/synth"
)

// SampleBudgetPlanName is the name the sample budget-PLAN seam gives its plan.
// Re-exported from synth so a test can locate it.
const SampleBudgetPlanName = synth.SampleBudgetPlanName

// ExtendSampleBudgetPlan is the p27.2 SAMPLE-BUDGET-PLAN seam wrapper: it creates the
// sample budget PLAN + projected splits (synth.ExtendSampleBudgetPlan) and records
// the expected shape. It is the NEW split-derived model's fixture seam, the
// counterpart the p27.3 cash-flow / budget-variance reports run over (replacing the
// retired ExtendSampleBudget). OPT-IN: New does NOT call it, so the base fixture and
// every existing golden/tally stay byte-identical.
//
// The plan is US-scoped; its splits span 2026-01-01 .. 2026-06-01 (the report window),
// mixing revenue/expense legs across the General + Educacion programs (one restricted
// to the Building Fund) with an receivable_payable A/R leg. The US current-cash accounts
// (Checking US) carry base-fixture actuals, so the cash-flow projection shows a
// non-zero PER-FUND opening; the R/E legs line up with actual activity for a real
// variance row. Amounts are SYNTHETIC (rule 11).
func (f *Fixture) ExtendSampleBudgetPlan(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	if err := synth.ExtendSampleBudgetPlan(ctx, f.Store, &f.IDs); err != nil {
		t.Fatalf("fixture: ExtendSampleBudgetPlan: %v", err)
	}

	f.Expected.SampleBudgetPlan = SampleBudgetPlanExpected{
		Plan:   f.IDs.SampleBudgetPlan,
		From:   "2026-01-01",
		To:     "2026-06-01",
		Splits: synth.SampleBudgetPlanSplits,
	}
}

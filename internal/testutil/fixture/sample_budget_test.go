package fixture_test

import (
	"context"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/testutil"
	"cuento/internal/testutil/fixture"
)

// TestExtendSampleBudgetSeamOptIn proves the p26.80 sample-budget seam is OPT-IN:
// New leaves no budget (the Expected zero value), so the default fixture is unchanged
// and no "Sample Operating Budget" exists.
func TestExtendSampleBudgetSeamOptIn(t *testing.T) {
	f := fixture.New(t)
	if f.Expected.SampleBudget.Budget != 0 {
		t.Fatalf("sample budget populated before ExtendSampleBudget; seam should be opt-in")
	}
	var n int
	if err := f.DB.QueryRow(`SELECT COUNT(*) FROM budgets WHERE name = ?`, fixture.SampleBudgetName).Scan(&n); err != nil {
		t.Fatalf("count sample budgets: %v", err)
	}
	if n != 0 {
		t.Errorf("sample budgets before seam = %d, want 0", n)
	}
}

// TestExtendSampleBudgetVersioned proves the seam writes the budget + every line
// through the store write funnel with the required op='create' version snapshots
// (rule 5), and that the sample budget lands with the expected shape.
func TestExtendSampleBudgetVersioned(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudget(t)

	exp := f.Expected.SampleBudget
	if exp.Budget == 0 {
		t.Fatalf("seam did not populate the sample budget")
	}

	// The budget itself is versioned create.
	testutil.AssertVersioned(t, f.DB, "budgets", exp.Budget, "create")

	// Every line the seam created is versioned create.
	lines, err := f.Store.BudgetLines(context.Background(), exp.Budget)
	if err != nil {
		t.Fatalf("list sample budget lines: %v", err)
	}
	if len(lines) != exp.Lines {
		t.Fatalf("sample budget has %d lines, want %d", len(lines), exp.Lines)
	}
	for _, ln := range lines {
		testutil.AssertVersioned(t, f.DB, "budget_lines", ln.ID, "create")
		if ln.Amount <= 0 {
			t.Errorf("sample budget line %d amount = %d, want positive", ln.ID, ln.Amount)
		}
	}
}

// TestExtendSampleBudgetLedgerClean proves the seam introduces no ledger violation:
// a budget is not a transaction, so it cannot unbalance the ledger, and the budget
// tables' version parity (Z-version checks, 00016 header) must stay clean. The only
// allowed warning is the fixture's baseline Z19 (unmapped Event Income).
func TestExtendSampleBudgetLedgerClean(t *testing.T) {
	f := fixture.New(t)
	f.ExtendSampleBudget(t)

	vs, err := ledger.Check(context.Background(), f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		switch v.Severity {
		case ledger.Error:
			t.Errorf("unexpected Error violation after ExtendSampleBudget: %s: %s", v.Rule, v.Detail)
		case ledger.Warning:
			if v.Rule != "Z19" {
				t.Errorf("unexpected warning rule %s after ExtendSampleBudget", v.Rule)
			}
		}
	}
}

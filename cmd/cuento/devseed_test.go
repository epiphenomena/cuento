package main

import (
	"context"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/store"
	"cuento/internal/testutil"
	"cuento/internal/testutil/fixture"
)

// TestSeedSampleBudget proves the devseed budget builder resolves accounts /
// programs / fund / subsidiary / schedules dynamically from an existing db (the
// canonical fixture) and creates a budget + lines through the store write funnel
// (versioned), leaving the ledger clean, and is idempotent on a second run.
func TestSeedSampleBudget(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), systemActor)

	created, err := seedSampleBudget(ctx, f.Store)
	if err != nil {
		t.Fatalf("seedSampleBudget: %v", err)
	}
	if !created {
		t.Fatalf("first seed reported not-created")
	}

	// The budget exists with its lines and is versioned (write funnel, rule 5).
	budgets, err := f.Store.ListBudgets(ctx)
	if err != nil {
		t.Fatalf("list budgets: %v", err)
	}
	var budgetID int64
	for _, b := range budgets {
		if b.Name == devseedBudgetName {
			budgetID = b.ID
		}
	}
	if budgetID == 0 {
		t.Fatalf("sample budget %q not created", devseedBudgetName)
	}
	testutil.AssertVersioned(t, f.DB, "budgets", budgetID, "create")

	lines, err := f.Store.BudgetLines(ctx, budgetID)
	if err != nil {
		t.Fatalf("budget lines: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("sample budget has no lines")
	}
	for _, ln := range lines {
		testutil.AssertVersioned(t, f.DB, "budget_lines", ln.ID, "create")
	}

	// Ledger stays clean (a budget is not a transaction); only the baseline Z19.
	vs, err := ledger.Check(ctx, f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("unexpected Error after seed: %s: %s", v.Rule, v.Detail)
		}
	}

	// Idempotent: a second run is a no-op (created=false, no duplicate budget).
	created2, err := seedSampleBudget(ctx, f.Store)
	if err != nil {
		t.Fatalf("second seedSampleBudget: %v", err)
	}
	if created2 {
		t.Errorf("second seed created a duplicate budget")
	}
	budgets2, err := f.Store.ListBudgets(ctx)
	if err != nil {
		t.Fatalf("list budgets after rerun: %v", err)
	}
	n := 0
	for _, b := range budgets2 {
		if b.Name == devseedBudgetName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("found %d sample budgets after rerun, want 1", n)
	}
}

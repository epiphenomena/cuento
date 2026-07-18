package main

import (
	"context"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/store"
	"cuento/internal/testutil"
	"cuento/internal/testutil/fixture"
)

// TestSeedSampleBudgetPlan proves the devseed budget builder resolves accounts /
// programs / fund / subsidiary dynamically from an existing db (the canonical
// fixture) and creates a budget PLAN + splits through the store write funnel
// (versioned), leaving the ledger clean, and is idempotent on a second run.
func TestSeedSampleBudgetPlan(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), systemActor)

	created, err := seedSampleBudgetPlan(ctx, f.Store)
	if err != nil {
		t.Fatalf("seedSampleBudgetPlan: %v", err)
	}
	if !created {
		t.Fatalf("first seed reported not-created")
	}

	// The plan exists with its splits and is versioned (write funnel, rule 5).
	plans, err := f.Store.ListBudgetPlans(ctx)
	if err != nil {
		t.Fatalf("list budget plans: %v", err)
	}
	var planID int64
	for _, p := range plans {
		if p.Name == devseedBudgetName {
			planID = p.ID
		}
	}
	if planID == 0 {
		t.Fatalf("sample budget plan %q not created", devseedBudgetName)
	}
	testutil.AssertVersioned(t, f.DB, "budget_plans", planID, "create")

	splits, err := f.Store.BudgetSplits(ctx, planID)
	if err != nil {
		t.Fatalf("budget splits: %v", err)
	}
	if len(splits) == 0 {
		t.Fatalf("sample budget plan has no splits")
	}
	for _, sp := range splits {
		testutil.AssertVersioned(t, f.DB, "budget_splits", sp.ID, "create")
	}

	// Ledger stays clean (a budget plan is not a transaction); only the baseline Z19.
	vs, err := ledger.Check(ctx, f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("unexpected Error after seed: %s: %s", v.Rule, v.Detail)
		}
	}

	// Idempotent: a second run is a no-op (created=false, no duplicate plan).
	created2, err := seedSampleBudgetPlan(ctx, f.Store)
	if err != nil {
		t.Fatalf("second seedSampleBudgetPlan: %v", err)
	}
	if created2 {
		t.Errorf("second seed created a duplicate plan")
	}
	plans2, err := f.Store.ListBudgetPlans(ctx)
	if err != nil {
		t.Fatalf("list budget plans after rerun: %v", err)
	}
	n := 0
	for _, p := range plans2 {
		if p.Name == devseedBudgetName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("found %d sample plans after rerun, want 1", n)
	}
}

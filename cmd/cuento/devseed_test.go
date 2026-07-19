package main

import (
	"context"
	"testing"

	"cuento/internal/ids"
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
	var planID ids.BudgetPlanID
	for _, p := range plans {
		if p.Name == devseedBudgetName {
			planID = p.ID
		}
	}
	if planID == 0 {
		t.Fatalf("sample budget plan %q not created", devseedBudgetName)
	}
	testutil.AssertVersioned(t, f.DB, "budget_plans", int64(planID), "create")

	splits, err := f.Store.BudgetSplits(ctx, planID)
	if err != nil {
		t.Fatalf("budget splits: %v", err)
	}
	if len(splits) == 0 {
		t.Fatalf("sample budget plan has no splits")
	}
	for _, sp := range splits {
		testutil.AssertVersioned(t, f.DB, "budget_splits", int64(sp.ID), "create")
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

// TestSeedSampleExpenseReport proves the devseed expense-report builder resolves the
// submitter / subsidiary / expense leaf / program dynamically from an existing db,
// creates a SUBMITTED report + lines through the store write funnel (versioned), keeps
// the ledger clean, and is idempotent on a second run.
func TestSeedSampleExpenseReport(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), systemActor)

	// The canonical fixture seeds no users; an expense report needs a submitter.
	if _, err := f.Store.CreateUser(ctx, store.CreateUserInput{
		Username: "seed-submitter", DisplayName: "Seed Submitter", TxnPerm: "none",
	}); err != nil {
		t.Fatalf("create submitter: %v", err)
	}

	created, err := seedSampleExpenseReport(ctx, f.Store)
	if err != nil {
		t.Fatalf("seedSampleExpenseReport: %v", err)
	}
	if !created {
		t.Fatalf("first seed reported not-created")
	}

	// Exactly one submitted report, carrying the marker line, all versioned (rule 5).
	submitted, err := f.Store.ExpenseReportsByStatus(ctx, "submitted")
	if err != nil {
		t.Fatalf("list submitted reports: %v", err)
	}
	if len(submitted) != 1 {
		t.Fatalf("found %d submitted reports, want 1", len(submitted))
	}
	reportID := submitted[0].ID
	// The report went through the write funnel: created, then the submit appended an
	// 'update' version (draft -> submitted), which is the latest op (rule 5).
	testutil.AssertVersioned(t, f.DB, "expense_reports", reportID, "update")

	lines, err := f.Store.ExpenseReportLines(ctx, reportID)
	if err != nil {
		t.Fatalf("expense report lines: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("sample report has %d lines, want 2", len(lines))
	}
	hasMarker := false
	for _, ln := range lines {
		if ln.Description == devseedExpenseMarker {
			hasMarker = true
		}
	}
	if !hasMarker {
		t.Errorf("sample report is missing its marker line %q", devseedExpenseMarker)
	}

	// Ledger stays clean (a submitted report is not yet a posted transaction).
	vs, err := ledger.Check(ctx, f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	for _, v := range vs {
		if v.Severity == ledger.Error {
			t.Errorf("unexpected Error after seed: %s: %s", v.Rule, v.Detail)
		}
	}

	// Idempotent: a second run is a no-op (created=false, still one report).
	created2, err := seedSampleExpenseReport(ctx, f.Store)
	if err != nil {
		t.Fatalf("second seedSampleExpenseReport: %v", err)
	}
	if created2 {
		t.Errorf("second seed created a duplicate report")
	}
	after, err := f.Store.ExpenseReportsByStatus(ctx, "submitted")
	if err != nil {
		t.Fatalf("list submitted reports after rerun: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("found %d submitted reports after rerun, want 1", len(after))
	}
}

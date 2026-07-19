package store

import (
	"errors"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/testutil"
)

// Budget-PLAN + budget-SPLIT operations (p27.2) -- the split-derived budget model.
// These tests copy the versioned-entity discipline the fund/budget tests
// established: each mutation is ONE change, live+version share it, AssertVersioned
// proves the snapshot op, and each invariant has a rejecting negative test. The
// pure cadence date math is tested in internal/budget; here the focus is
// persistence, versioning, and validation (esp. the R/E-program-required /
// A/L-program-forbidden rule).

// splitSetup builds the common references a budget-split needs: a subsidiary, a
// plan on it, an R/E (expense) account mapped to it, a revenue account, an
// open_item receivable account, a program, and a fund scoped to the sub.
type splitSetup struct {
	sub                          ids.SubsidiaryID
	expense, revenue, receivable ids.AccountID
	prog                         ids.ProgramID
	fund                         ids.FundID
	plan                         ids.BudgetPlanID
}

func mkSplitSetup(t *testing.T, s *Store) splitSetup {
	t.Helper()
	sub := newSub(t, s, rootID, "Sub")
	plan, err := s.CreateBudgetPlan(mutCtx(), BudgetPlanInput{Name: "FY26", SubsidiaryID: sub})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	prog, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Ops"})
	if err != nil {
		t.Fatalf("create program: %v", err)
	}
	expense, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: enName("Rent"), Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("create expense account: %v", err)
	}
	revenue, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "revenue", DefaultCurrency: "USD", Names: enName("Donations"), Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("create revenue account: %v", err)
	}
	receivable, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Due from"), Subsidiaries: []ids.SubsidiaryID{sub}, OpenItem: true,
	})
	if err != nil {
		t.Fatalf("create receivable account: %v", err)
	}
	fund, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Grant", Restriction: "purpose", Subsidiaries: []ids.SubsidiaryID{sub},
	})
	if err != nil {
		t.Fatalf("create fund: %v", err)
	}
	return splitSetup{sub: sub, plan: plan, expense: expense, revenue: revenue, receivable: receivable, prog: prog, fund: fund}
}

// fundp returns a pointer to a fund id (the typed FundID pointer BudgetSplitInput
// carries).
func fundp(id ids.FundID) *ids.FundID { return &id }

// progp returns a pointer to a program id (the typed ProgramID pointer
// BudgetSplitInput carries), the program-typed sibling of fundp.
func progp(id ids.ProgramID) *ids.ProgramID { return &id }

// TestDeleteBudgetPlanCascade: deleting a plan HARD-deletes it and all its splits
// under one change, appending a 'delete' version for the plan AND each split (rule 14
// audit completeness), and a second delete on a gone plan is a clean ErrBudgetPlanNotFound.
func TestDeleteBudgetPlanCascade(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)

	sp1, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Description: "Rent", Date: "2026-01-01", AccountID: st.expense,
		ProgramID: progp(st.prog), Amount: 100_000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create split: %v", err)
	}

	if err := s.DeleteBudgetPlan(mutCtx(), st.plan); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	// The plan and its split are gone; both carry a 'delete' version (audit intact).
	testutil.AssertVersioned(t, d, "budget_plans", int64(st.plan), "delete")
	testutil.AssertVersioned(t, d, "budget_splits", int64(sp1), "delete")
	if _, err := s.GetBudgetPlan(mutCtx(), st.plan); err == nil {
		t.Errorf("plan still exists after delete")
	}
	if splits, err := s.BudgetSplits(mutCtx(), st.plan); err != nil {
		t.Fatalf("list splits after delete: %v", err)
	} else if len(splits) != 0 {
		t.Errorf("plan has %d splits after delete, want 0", len(splits))
	}

	// A second delete on the gone plan is a clean typed error.
	if err := s.DeleteBudgetPlan(mutCtx(), st.plan); !errors.Is(err, ErrBudgetPlanNotFound) {
		t.Errorf("delete gone plan: err = %v, want ErrBudgetPlanNotFound", err)
	}
}

func TestCreateBudgetPlanVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	sub := newSub(t, s, rootID, "Sub")
	id, err := s.CreateBudgetPlan(mutCtx(), BudgetPlanInput{Name: "FY26", SubsidiaryID: sub, Notes: "n"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_plans", int64(id), "create")
	if err := s.UpdateBudgetPlan(mutCtx(), id, BudgetPlanInput{Name: "FY26b", SubsidiaryID: sub}); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_plans", int64(id), "update")
	got, err := s.GetBudgetPlan(mutCtx(), id)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.Name != "FY26b" {
		t.Errorf("plan name = %q, want FY26b", got.Name)
	}
}

func TestCreateBudgetPlanBadSubsidiary(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	if _, err := s.CreateBudgetPlan(mutCtx(), BudgetPlanInput{Name: "X", SubsidiaryID: 9999}); !errors.Is(err, ErrBudgetSplitRefMissing) {
		t.Fatalf("want ErrBudgetSplitRefMissing, got %v", err)
	}
}

func TestCreateBudgetSplitRE(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	id, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Description: "Monthly rent", Date: "2026-03-15", AccountID: st.expense,
		FundID: fundp(st.fund), ProgramID: progp(st.prog), Amount: 120000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create R/E split: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_splits", int64(id), "create")
	got, err := s.GetBudgetSplit(mutCtx(), id)
	if err != nil {
		t.Fatalf("get split: %v", err)
	}
	if !got.ProgramID.Valid || got.ProgramID.Int64 != int64(st.prog) {
		t.Errorf("R/E split program = %v, want %d", got.ProgramID, st.prog)
	}
	if got.Amount != 120000 {
		t.Errorf("amount = %d, want 120000", got.Amount)
	}
}

// TestCreateBudgetSplitREProgramPrefill: an R/E split with no explicit program
// prefills from the account's default_program (like the ledger).
func TestCreateBudgetSplitREProgramPrefill(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	// Give the expense account a default program.
	if err := s.UpdateAccount(mutCtx(), st.expense, UpdateAccountInput{DefaultProgramID: progp(st.prog)}); err != nil {
		t.Fatalf("set default program: %v", err)
	}
	id, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Date: "2026-03-15", AccountID: st.expense, Amount: 500, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create split (prefill): %v", err)
	}
	got, _ := s.GetBudgetSplit(mutCtx(), id)
	if !got.ProgramID.Valid || got.ProgramID.Int64 != int64(st.prog) {
		t.Errorf("prefilled program = %v, want %d", got.ProgramID, st.prog)
	}
}

// TestCreateBudgetSplitREProgramRequired: an R/E split with neither an explicit
// program nor an account default is REJECTED (DECISIONS tension 3).
func TestCreateBudgetSplitREProgramRequired(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	_, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Date: "2026-03-15", AccountID: st.revenue, Amount: 500, Currency: "USD",
	})
	if !errors.Is(err, ErrBudgetSplitProgramRequired) {
		t.Fatalf("want ErrBudgetSplitProgramRequired, got %v", err)
	}
}

// TestCreateBudgetSplitOpenItemAL: an open_item receivable split is allowed and
// carries NO program.
func TestCreateBudgetSplitOpenItemAL(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	id, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Description: "Acme Corp", Date: "2026-04-01", AccountID: st.receivable,
		Amount: 8000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create open-item split: %v", err)
	}
	got, _ := s.GetBudgetSplit(mutCtx(), id)
	if got.ProgramID.Valid {
		t.Errorf("A/L split program = %v, want NULL", got.ProgramID)
	}
}

// TestCreateBudgetSplitALProgramForbidden: an A/L split carrying a program is
// REJECTED (mirrors the ledger's program-on-balance-sheet rule).
func TestCreateBudgetSplitALProgramForbidden(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	_, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Date: "2026-04-01", AccountID: st.receivable, ProgramID: progp(st.prog),
		Amount: 8000, Currency: "USD",
	})
	if !errors.Is(err, ErrBudgetSplitProgramForbidden) {
		t.Fatalf("want ErrBudgetSplitProgramForbidden, got %v", err)
	}
}

// TestCreateBudgetSplitPlainBalanceSheet: an account that is neither R/E nor an
// open_item A/L (a plain asset) is REJECTED.
func TestCreateBudgetSplitPlainBalanceSheet(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	plain, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Checking"), Subsidiaries: []ids.SubsidiaryID{st.sub},
	})
	if err != nil {
		t.Fatalf("create plain asset: %v", err)
	}
	_, err = s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Date: "2026-04-01", AccountID: plain, Amount: 100, Currency: "USD",
	})
	if !errors.Is(err, ErrBudgetSplitAccountType) {
		t.Fatalf("want ErrBudgetSplitAccountType, got %v", err)
	}
}

// TestCreateBudgetSplitAccountNotInSubsidiary: an account not mapped to the plan's
// subsidiary is REJECTED (D18).
func TestCreateBudgetSplitAccountNotInSubsidiary(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	other := newSub(t, s, rootID, "Other")
	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: enName("Elsewhere"), Subsidiaries: []ids.SubsidiaryID{other},
	})
	if err != nil {
		t.Fatalf("create account in other sub: %v", err)
	}
	_, err = s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Date: "2026-04-01", AccountID: acct, ProgramID: progp(st.prog), Amount: 100, Currency: "USD",
	})
	if !errors.Is(err, ErrBudgetSplitAccountSub) {
		t.Fatalf("want ErrBudgetSplitAccountSub, got %v", err)
	}
}

// TestReplaceBudgetSplitsAtomicRollback: a replace whose FIRST desired row is invalid
// (R/E with no program) must roll the WHOLE change back, leaving the plan's prior splits
// INTACT -- the atomicity guarantee (a per-call delete-then-insert would have wiped them).
func TestReplaceBudgetSplitsAtomicRollback(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	// Seed two valid R/E splits.
	valid := []BudgetSplitInput{
		{Date: "2026-01-01", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 100, Currency: "USD"},
		{Date: "2026-02-01", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 200, Currency: "USD"},
	}
	if _, err := s.ReplaceBudgetSplits(mutCtx(), st.plan, valid); err != nil {
		t.Fatalf("seed replace: %v", err)
	}
	before, _ := s.BudgetSplits(mutCtx(), st.plan)
	if len(before) != 2 {
		t.Fatalf("seeded %d splits, want 2", len(before))
	}
	// Now attempt a replace whose ROW 0 is an R/E revenue split with NO program (the
	// revenue account has no default) -> rejected at insert time.
	bad := []BudgetSplitInput{
		{Date: "2026-03-01", AccountID: st.revenue, Amount: 500, Currency: "USD"},
		{Date: "2026-03-02", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 300, Currency: "USD"},
	}
	failedIdx, err := s.ReplaceBudgetSplits(mutCtx(), st.plan, bad)
	if !errors.Is(err, ErrBudgetSplitProgramRequired) {
		t.Fatalf("want ErrBudgetSplitProgramRequired, got %v", err)
	}
	if failedIdx != 0 {
		t.Errorf("failedIdx = %d, want 0", failedIdx)
	}
	// The prior two splits must still be present (nothing lost).
	after, _ := s.BudgetSplits(mutCtx(), st.plan)
	if len(after) != 2 {
		t.Fatalf("after rejected replace, plan has %d splits, want the original 2", len(after))
	}
	if after[0].Amount != 100 || after[1].Amount != 200 {
		t.Errorf("prior splits changed: %d/%d, want 100/200", after[0].Amount, after[1].Amount)
	}
}

// TestReplaceBudgetSplitsSuccess: a valid replace swaps the whole set atomically.
func TestReplaceBudgetSplitsSuccess(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	if _, err := s.ReplaceBudgetSplits(mutCtx(), st.plan, []BudgetSplitInput{
		{Date: "2026-01-01", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 100, Currency: "USD"},
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if _, err := s.ReplaceBudgetSplits(mutCtx(), st.plan, []BudgetSplitInput{
		{Date: "2026-02-01", AccountID: st.receivable, Amount: 800, Currency: "USD"},
		{Date: "2026-02-02", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 250, Currency: "USD"},
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	got, _ := s.BudgetSplits(mutCtx(), st.plan)
	if len(got) != 2 {
		t.Fatalf("after replace, %d splits, want 2", len(got))
	}
}

// TestAppendBudgetSplitsAtomicRollback: a CSV-import batch whose second row is invalid
// rolls the whole batch back (no partial append that a retry would duplicate).
func TestAppendBudgetSplitsAtomicRollback(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	failedIdx, err := s.AppendBudgetSplits(mutCtx(), st.plan, []BudgetSplitInput{
		{Date: "2026-01-01", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 100, Currency: "USD"},
		{Date: "2026-01-02", AccountID: st.receivable, ProgramID: progp(st.prog), Amount: 200, Currency: "USD"}, // A/L + program -> forbidden
	})
	if !errors.Is(err, ErrBudgetSplitProgramForbidden) {
		t.Fatalf("want ErrBudgetSplitProgramForbidden, got %v", err)
	}
	if failedIdx != 1 {
		t.Errorf("failedIdx = %d, want 1", failedIdx)
	}
	got, _ := s.BudgetSplits(mutCtx(), st.plan)
	if len(got) != 0 {
		t.Fatalf("after rejected append, plan has %d splits, want 0 (whole batch rolled back)", len(got))
	}
}

func TestUpdateDeleteBudgetSplit(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	st := mkSplitSetup(t, s)
	id, err := s.CreateBudgetSplit(mutCtx(), st.plan, BudgetSplitInput{
		Date: "2026-03-15", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 100, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpdateBudgetSplit(mutCtx(), id, BudgetSplitInput{
		Date: "2026-03-16", AccountID: st.expense, ProgramID: progp(st.prog), Amount: 200, Currency: "USD",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_splits", int64(id), "update")
	got, _ := s.GetBudgetSplit(mutCtx(), id)
	if got.Amount != 200 || got.Date != "2026-03-16" {
		t.Errorf("after update amount=%d date=%q, want 200/2026-03-16", got.Amount, got.Date)
	}
	if err := s.DeleteBudgetSplit(mutCtx(), id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_splits", int64(id), "delete")
	splits, _ := s.BudgetSplits(mutCtx(), st.plan)
	if len(splits) != 0 {
		t.Errorf("after delete, plan has %d splits, want 0", len(splits))
	}
}

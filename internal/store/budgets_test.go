package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/testutil"
)

// Budget operations (p19.1) -- named schedules + budgets + budget lines, the
// DISCRETE-dated-occurrence budgeting model (PLAN Phase 19). These tests copy the
// versioned-entity discipline the fund tests (p07.3) established: each mutation is
// ONE change, live+version share it, AssertVersioned proves the snapshot op, and
// each invariant has a rejecting negative test. The pure date math is tested in
// internal/budget; here the focus is persistence, versioning, and validation.

// mkRESetup builds the common references a budget line needs: a subsidiary, an
// R/E account mapped to it, a program, a fund scoped to the sub, a budget, and a
// simple monthly schedule. Returns the ids.
type reSetup struct {
	sub, acct, prog, fund, budget, sched int64
}

func mkRESetup(t *testing.T, s *Store) reSetup {
	t.Helper()
	sub := newSub(t, s, rootID, "Sub")
	acct, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD", Names: enName("Rent"), Subsidiaries: []int64{sub},
	})
	if err != nil {
		t.Fatalf("create R/E account: %v", err)
	}
	prog, err := s.CreateProgram(mutCtx(), CreateProgramInput{ParentID: rootProgramID, Name: "Ops"})
	if err != nil {
		t.Fatalf("create program: %v", err)
	}
	fund, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Grant", Restriction: "purpose", Subsidiaries: []int64{sub},
	})
	if err != nil {
		t.Fatalf("create fund: %v", err)
	}
	budget, err := s.CreateBudget(mutCtx(), BudgetInput{
		Name: "FY26", PeriodStart: "2026-01-01", PeriodEnd: "2026-12-31",
	})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	sched, err := s.CreateSchedule(mutCtx(), ScheduleInput{
		Name: "15th monthly", Kind: "monthly", DayOfMonth: intp(15),
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return reSetup{sub: sub, acct: acct, prog: prog, fund: fund, budget: budget, sched: sched}
}

func intp(n int) *int     { return &n }
func i64p(n int64) *int64 { return &n }

// --- schedules --------------------------------------------------------------

// TestCreateScheduleVersioned: creating a schedule with a custom date list mints
// ONE change and versions the schedule (create) plus each imported date (create).
func TestCreateScheduleVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	id, err := s.CreateSchedule(mutCtx(), ScheduleInput{
		Name: "Payroll dates", Kind: "custom",
		CustomDates: []string{"2026-01-15", "2026-02-13", "2026-01-15"}, // dup deduped
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if id <= 0 {
		t.Fatalf("CreateSchedule id %d, want positive", id)
	}
	if got := countChanges(t, d) - before; got != 1 {
		t.Fatalf("created schedule minted %d changes, want 1", got)
	}
	testutil.AssertVersioned(t, d, "budget_schedules", id, "create")

	// Both distinct dates persisted + versioned; the duplicate collapsed.
	dates, err := s.ScheduleDates(context.Background(), id)
	if err != nil {
		t.Fatalf("ScheduleDates: %v", err)
	}
	if len(dates) != 2 {
		t.Fatalf("schedule dates = %v, want 2 distinct", dates)
	}
	assertScheduleDateVersioned(t, d, id, "2026-01-15", "create")
	assertScheduleDateVersioned(t, d, id, "2026-02-13", "create")
}

// TestUpdateScheduleVersioned: updating fields + diffing the date set versions the
// schedule (update), the removed date (delete), and the added date (create).
func TestUpdateScheduleVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateSchedule(mutCtx(), ScheduleInput{
		Name: "old", Kind: "custom", CustomDates: []string{"2026-01-15", "2026-02-15"},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	// Keep 2026-01-15, drop 2026-02-15, add 2026-03-15.
	if err := s.UpdateSchedule(mutCtx(), id, ScheduleInput{
		Name: "new", Kind: "custom", CustomDates: []string{"2026-01-15", "2026-03-15"},
	}); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_schedules", id, "update")
	assertScheduleDateVersioned(t, d, id, "2026-02-15", "delete")
	assertScheduleDateVersioned(t, d, id, "2026-03-15", "create")

	got, err := s.GetSchedule(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if got.Name != "new" {
		t.Fatalf("schedule name = %q, want new", got.Name)
	}
}

// TestUpdateCustomScheduleNameOnly: a name-only edit of a custom schedule (nil
// CustomDates) must SUCCEED and leave the imported date list untouched -- a nil
// list means "leave the set alone", it is NOT an empty-list rejection.
func TestUpdateCustomScheduleNameOnly(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	id, err := s.CreateSchedule(mutCtx(), ScheduleInput{
		Name: "before", Kind: "custom", CustomDates: []string{"2026-01-15", "2026-02-13"},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if err := s.UpdateSchedule(mutCtx(), id, ScheduleInput{Name: "after", Kind: "custom"}); err != nil {
		t.Fatalf("UpdateSchedule(name only) err = %v, want nil", err)
	}
	testutil.AssertVersioned(t, d, "budget_schedules", id, "update")

	got, err := s.GetSchedule(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if got.Name != "after" {
		t.Fatalf("schedule name = %q, want after", got.Name)
	}
	dates, err := s.ScheduleDates(context.Background(), id)
	if err != nil {
		t.Fatalf("ScheduleDates: %v", err)
	}
	if len(dates) != 2 {
		t.Fatalf("dates after name-only update = %v, want the original 2 untouched", dates)
	}
}

// TestCreateScheduleRejectsInvalidFields: a monthly schedule with neither a
// day-of-month nor an ordinal is rejected (ErrScheduleInvalid) and writes nothing.
func TestCreateScheduleRejectsInvalidFields(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	_, err := s.CreateSchedule(mutCtx(), ScheduleInput{Name: "bad", Kind: "monthly"})
	if !errors.Is(err, ErrScheduleInvalid) {
		t.Fatalf("CreateSchedule(bad monthly) err = %v, want ErrScheduleInvalid", err)
	}
	if got := countChanges(t, d) - before; got != 0 {
		t.Fatalf("rejected schedule minted %d changes, want 0", got)
	}
}

// --- budgets ----------------------------------------------------------------

// TestCreateBudgetVersioned + TestUpdateBudgetVersioned: standard versioned CRUD.
func TestCreateBudgetVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)

	before := countChanges(t, d)
	id, err := s.CreateBudget(mutCtx(), BudgetInput{Name: "FY26", PeriodStart: "2026-01-01", PeriodEnd: "2026-12-31"})
	if err != nil {
		t.Fatalf("CreateBudget: %v", err)
	}
	if got := countChanges(t, d) - before; got != 1 {
		t.Fatalf("created budget minted %d changes, want 1", got)
	}
	testutil.AssertVersioned(t, d, "budgets", id, "create")

	if err := s.UpdateBudget(mutCtx(), id, BudgetInput{Name: "FY26 rev", PeriodStart: "2026-01-01", PeriodEnd: "2026-12-31"}); err != nil {
		t.Fatalf("UpdateBudget: %v", err)
	}
	testutil.AssertVersioned(t, d, "budgets", id, "update")
	if _, err := s.GetBudget(context.Background(), id); err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
}

func TestUpdateBudgetNotFound(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	if err := s.UpdateBudget(mutCtx(), 9999, BudgetInput{Name: "x", PeriodStart: "2026-01-01", PeriodEnd: "2026-12-31"}); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("UpdateBudget(missing) err = %v, want ErrBudgetNotFound", err)
	}
}

// --- budget lines -----------------------------------------------------------

// TestBudgetLineLifecycleVersioned: create -> update -> delete, each a versioned op
// under one change; the line carries the full (sub,account,fund,program,amount,
// currency,schedule) key.
func TestBudgetLineLifecycleVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	r := mkRESetup(t, s)

	id, err := s.CreateBudgetLine(mutCtx(), r.budget, BudgetLineInput{
		SubsidiaryID: r.sub, AccountID: r.acct, FundID: i64p(r.fund), ProgramID: r.prog,
		Amount: 120000, Currency: "USD", ScheduleID: r.sched,
	})
	if err != nil {
		t.Fatalf("CreateBudgetLine: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_lines", id, "create")

	line, err := s.GetBudgetLine(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBudgetLine: %v", err)
	}
	if line.Amount != 120000 || line.Currency != "USD" || line.ScheduleID != r.sched ||
		line.AccountID != r.acct || line.SubsidiaryID != r.sub || line.ProgramID != r.prog ||
		!line.FundID.Valid || line.FundID.Int64 != r.fund {
		t.Fatalf("budget line = %+v, want the created key", line)
	}

	// Update: bump the amount and clear the fund (unrestricted).
	if err := s.UpdateBudgetLine(mutCtx(), id, BudgetLineInput{
		SubsidiaryID: r.sub, AccountID: r.acct, FundID: nil, ProgramID: r.prog,
		Amount: 150000, Currency: "USD", ScheduleID: r.sched,
	}); err != nil {
		t.Fatalf("UpdateBudgetLine: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_lines", id, "update")
	line, _ = s.GetBudgetLine(context.Background(), id)
	if line.Amount != 150000 || line.FundID.Valid {
		t.Fatalf("updated line = %+v, want amount 150000 and NULL fund", line)
	}

	// Delete: hard delete + op='delete' version (rule 14).
	if err := s.DeleteBudgetLine(mutCtx(), id); err != nil {
		t.Fatalf("DeleteBudgetLine: %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_lines", id, "delete")
	if _, err := s.GetBudgetLine(context.Background(), id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBudgetLine after delete err = %v, want sql.ErrNoRows", err)
	}
}

// TestBudgetLineRejectsBalanceSheetAccount: a line on a balance-sheet (asset)
// account is rejected -- a budget is of R/E flows.
func TestBudgetLineRejectsBalanceSheetAccount(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	r := mkRESetup(t, s)

	asset, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Cash"), Subsidiaries: []int64{r.sub},
	})
	if err != nil {
		t.Fatalf("create asset account: %v", err)
	}
	before := countChanges(t, d)
	_, err = s.CreateBudgetLine(mutCtx(), r.budget, BudgetLineInput{
		SubsidiaryID: r.sub, AccountID: asset, ProgramID: r.prog,
		Amount: 1000, Currency: "USD", ScheduleID: r.sched,
	})
	if !errors.Is(err, ErrBudgetLineAccountNotRE) {
		t.Fatalf("CreateBudgetLine(asset) err = %v, want ErrBudgetLineAccountNotRE", err)
	}
	if got := countChanges(t, d) - before; got != 0 {
		t.Fatalf("rejected line minted %d changes, want 0", got)
	}
}

// TestBudgetLineRejectsBadRefs: a missing fund/program/subsidiary/schedule, a bad
// currency, and a non-positive amount are each rejected and write nothing.
func TestBudgetLineRejectsBadRefs(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	r := mkRESetup(t, s)

	base := BudgetLineInput{
		SubsidiaryID: r.sub, AccountID: r.acct, FundID: i64p(r.fund), ProgramID: r.prog,
		Amount: 1000, Currency: "USD", ScheduleID: r.sched,
	}
	cases := []struct {
		name    string
		mutate  func(BudgetLineInput) BudgetLineInput
		wantErr error
	}{
		{"bad fund", func(in BudgetLineInput) BudgetLineInput { in.FundID = i64p(9999); return in }, ErrBudgetRefMissing},
		{"bad program", func(in BudgetLineInput) BudgetLineInput { in.ProgramID = 9999; return in }, ErrBudgetRefMissing},
		{"bad subsidiary", func(in BudgetLineInput) BudgetLineInput { in.SubsidiaryID = 9999; return in }, ErrBudgetRefMissing},
		{"bad schedule", func(in BudgetLineInput) BudgetLineInput { in.ScheduleID = 9999; return in }, ErrBudgetRefMissing},
		{"bad currency", func(in BudgetLineInput) BudgetLineInput { in.Currency = "ZZZ"; return in }, ErrBudgetRefMissing},
		{"bad account", func(in BudgetLineInput) BudgetLineInput { in.AccountID = 9999; return in }, ErrBudgetRefMissing},
		{"zero amount", func(in BudgetLineInput) BudgetLineInput { in.Amount = 0; return in }, ErrBudgetAmount},
		{"negative amount", func(in BudgetLineInput) BudgetLineInput { in.Amount = -5; return in }, ErrBudgetAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countChanges(t, d)
			_, err := s.CreateBudgetLine(mutCtx(), r.budget, tc.mutate(base))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateBudgetLine(%s) err = %v, want %v", tc.name, err, tc.wantErr)
			}
			if got := countChanges(t, d) - before; got != 0 {
				t.Fatalf("rejected line (%s) minted %d changes, want 0", tc.name, got)
			}
		})
	}
}

// TestBudgetLineUnrestrictedFundOK: a line with a NULL fund (unrestricted) is
// accepted (fund_id is nullable by design).
func TestBudgetLineUnrestrictedFundOK(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	r := mkRESetup(t, s)

	id, err := s.CreateBudgetLine(mutCtx(), r.budget, BudgetLineInput{
		SubsidiaryID: r.sub, AccountID: r.acct, FundID: nil, ProgramID: r.prog,
		Amount: 5000, Currency: "USD", ScheduleID: r.sched,
	})
	if err != nil {
		t.Fatalf("CreateBudgetLine(unrestricted): %v", err)
	}
	testutil.AssertVersioned(t, d, "budget_lines", id, "create")
	line, _ := s.GetBudgetLine(context.Background(), id)
	if line.FundID.Valid {
		t.Fatalf("unrestricted line fund_id = %v, want NULL", line.FundID)
	}
}

// assertScheduleDateVersioned checks the composite (schedule_id, occurs_on) list
// row's latest version has the expected op (the AssertVersionedFundSub analog for
// budget_schedule_dates).
func assertScheduleDateVersioned(t *testing.T, d *sql.DB, scheduleID int64, occursOn, wantOp string) {
	t.Helper()
	var gotOp string
	err := d.QueryRow(
		`SELECT op FROM budget_schedule_dates_versions
		  WHERE entity_id = ? AND occurs_on = ?
		  ORDER BY valid_from DESC, id DESC LIMIT 1`, scheduleID, occursOn,
	).Scan(&gotOp)
	if err != nil {
		t.Fatalf("schedule date version (%d,%s): %v", scheduleID, occursOn, err)
	}
	if gotOp != wantOp {
		t.Errorf("schedule date (%d,%s) latest op = %q, want %q", scheduleID, occursOn, gotOp, wantOp)
	}
}

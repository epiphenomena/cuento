package reports_test

// TDD for p19.2 -- the BUDGET toolkit (occurrences, actuals-vs-budget, cashflow
// projection). Every expected number is HAND-COMPUTED from the canonical fixture
// (PLAN Appendix D) and the budget lines each test constructs, BEFORE the
// implementation exists. The budgeting design point (PLAN Phase 19) is DISCRETE
// dated occurrences with NO pro-rata: an occurrence's FULL amount lands in the
// single period bucket its date falls in. These tests pin that.
//
// Sign convention (documented in DECISIONS p19.2): a budget line's stored amount is
// a POSITIVE magnitude; the toolkit signs it by ACCOUNT TYPE to match Activity's
// net-debit space -- an EXPENSE line budgets a POSITIVE net-debit, a REVENUE line a
// NEGATIVE one. Variance = actual - budgeted (positive = higher net-debit than
// budget: expense OVERSPEND / revenue UNDER-collection). CashflowProjection flips
// the sign for a fund's spendable position: delta = -(net-debit), so revenue
// INCREASES and expense DECREASES the fund.

import (
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// newBudget creates a budget over [start,end] and returns its id.
func newBudget(t *testing.T, ctx context.Context, s *store.Store, start, end string) int64 {
	t.Helper()
	id, err := s.CreateBudget(ctx, store.BudgetInput{Name: "test", PeriodStart: start, PeriodEnd: end})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	return id
}

// onetimeSchedule creates a schedule with a single occurrence on d.
func onetimeSchedule(t *testing.T, ctx context.Context, s *store.Store, d string) int64 {
	t.Helper()
	id, err := s.CreateSchedule(ctx, store.ScheduleInput{Name: "once-" + d, Kind: "onetime", AnchorDate: &d})
	if err != nil {
		t.Fatalf("create onetime schedule %s: %v", d, err)
	}
	return id
}

// monthlyDoMSchedule creates a monthly day-of-month schedule (weekend policy
// 'actual' so no rolling perturbs a hand-computed occurrence date).
func monthlyDoMSchedule(t *testing.T, ctx context.Context, s *store.Store, dom int) int64 {
	t.Helper()
	id, err := s.CreateSchedule(ctx, store.ScheduleInput{
		Name: "monthly", Kind: "monthly", DayOfMonth: &dom, WeekendAdjust: "actual",
	})
	if err != nil {
		t.Fatalf("create monthly schedule dom %d: %v", dom, err)
	}
	return id
}

// addLine adds a budget line and returns its id.
func addLine(t *testing.T, ctx context.Context, s *store.Store, budgetID int64, in store.BudgetLineInput) int64 {
	t.Helper()
	id, err := s.CreateBudgetLine(ctx, budgetID, in)
	if err != nil {
		t.Fatalf("create budget line: %v", err)
	}
	return id
}

// bucketFind returns the CurAmt minor for currency ccy in the AVB cell for
// (bucket,key), and whether a matching cell was found.
func avbFind(cells []reports.BudgetVsActualCell, bucket string, key reports.BudgetKey) (reports.BudgetVsActualCell, bool) {
	for _, c := range cells {
		if c.Bucket == bucket && c.Key == key {
			return c, true
		}
	}
	return reports.BudgetVsActualCell{}, false
}

// TestBudgetNoProRataWeekly is the WHOLE design point: a MONTHLY line shown at
// WEEKLY granularity puts its full amount in the ONE week its occurrence falls in --
// NOT spread across the ~4.3 weeks of the month. A single monthly occurrence over a
// one-month window (day-of-month 15, July 2026) => exactly one weekly bucket
// carrying the FULL 100,000, and no other bucket carries anything.
func TestBudgetNoProRataWeekly(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	tk := tkFor(f, f.IDs.US)

	budgetID := newBudget(t, ctx, f.Store, "2026-07-01", "2026-07-31")
	sched := monthlyDoMSchedule(t, ctx, f.Store, 15) // occurrence 2026-07-15 (Wed)
	line := addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.US, AccountID: f.IDs.Salaries, ProgramID: f.IDs.General,
		Amount: 100_000, Currency: "USD", ScheduleID: sched,
	})
	_ = line

	// Weekly bucketing over the budget period.
	cells, err := tk.BudgetVsActual(ctx, reports.Scope{Sub: f.IDs.US}, budgetID, "2026-07-01", "2026-07-31", reports.GranWeek)
	if err != nil {
		t.Fatalf("BudgetVsActual weekly: %v", err)
	}

	// The key: US / Salaries / unrestricted(0) / General / USD.
	key := reports.BudgetKey{
		Subsidiary: f.IDs.US, Account: f.IDs.Salaries, Fund: 0,
		Program: f.IDs.General, Currency: "USD",
	}

	// EXACTLY one bucket carries budgeted, and it is the week of 2026-07-15 = Monday
	// 2026-07-13. The full 100,000 lands there (expense => positive net-debit). NO
	// pro-rata: no other bucket carries any part of the amount.
	var budgetedBuckets int
	for _, c := range cells {
		if c.Key == key && c.Budgeted != 0 {
			budgetedBuckets++
			if c.Bucket != "2026-07-13" {
				t.Errorf("budgeted lands in bucket %q, want week 2026-07-13", c.Bucket)
			}
			if c.Budgeted != 100_000 {
				t.Errorf("budgeted in week = %d, want full 100000 (no pro-rata)", c.Budgeted)
			}
		}
	}
	if budgetedBuckets != 1 {
		t.Fatalf("budgeted appears in %d weekly buckets, want exactly 1 (no pro-rata spread)", budgetedBuckets)
	}
}

// TestActualsVsBudget hand-computes budgeted / actual / variance per bucket for two
// keys that share sub/account/program/currency but DIFFER by fund -- the
// discriminator the fixture bakes into the 2025-05-10 mixed-funding transaction:
//
//	(MX, ProgramSupplies, BecaAgua, Educacion, MXN)   actual 2025-05 = +300,000
//	(MX, ProgramSupplies, unrestricted(0), Educacion, MXN) actual 2025-05 = +200,000
//
// A budget line for the BecaAgua key (onetime 2025-05-15, amount 250,000, expense =>
// +250,000 net-debit) lands its full amount in the monthly bucket 2025-05.
// Variance = actual - budgeted = 300,000 - 250,000 = +50,000 (overspend).
func TestActualsVsBudget(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	tk := tkFor(f, f.IDs.MX)

	budgetID := newBudget(t, ctx, f.Store, "2025-01-01", "2025-12-31")
	sched := onetimeSchedule(t, ctx, f.Store, "2025-05-15")
	beca := f.IDs.BecaAgua
	addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.MX, AccountID: f.IDs.ProgramSupplies, FundID: &beca,
		ProgramID: f.IDs.Educacion, Amount: 250_000, Currency: "MXN", ScheduleID: sched,
	})

	cells, err := tk.BudgetVsActual(ctx, reports.Scope{Sub: f.IDs.MX}, budgetID, "2025-01-01", "2025-12-31", reports.GranMonth)
	if err != nil {
		t.Fatalf("BudgetVsActual monthly: %v", err)
	}

	becaKey := reports.BudgetKey{
		Subsidiary: f.IDs.MX, Account: f.IDs.ProgramSupplies, Fund: f.IDs.BecaAgua,
		Program: f.IDs.Educacion, Currency: "MXN",
	}
	unrestrictedKey := reports.BudgetKey{
		Subsidiary: f.IDs.MX, Account: f.IDs.ProgramSupplies, Fund: 0,
		Program: f.IDs.Educacion, Currency: "MXN",
	}

	// BecaAgua key, 2025-05 bucket: budgeted 250,000, actual 300,000, variance +50,000.
	c, ok := avbFind(cells, "2025-05-01", becaKey)
	if !ok {
		t.Fatalf("no BecaAgua cell in bucket 2025-05-01")
	}
	if c.Budgeted != 250_000 || c.Actual != 300_000 || c.Variance != 50_000 {
		t.Errorf("BecaAgua 2025-05: budgeted=%d actual=%d variance=%d, want 250000/300000/50000",
			c.Budgeted, c.Actual, c.Variance)
	}

	// Unrestricted key, 2025-05 bucket: NO budget line => budgeted 0, actual 200,000
	// (the OTHER half of the mixed txn -- proving the fund-0 key is isolated, not
	// merged with the BecaAgua half), variance +200,000.
	u, ok := avbFind(cells, "2025-05-01", unrestrictedKey)
	if !ok {
		t.Fatalf("no unrestricted cell in bucket 2025-05-01")
	}
	if u.Budgeted != 0 || u.Actual != 200_000 || u.Variance != 200_000 {
		t.Errorf("unrestricted 2025-05: budgeted=%d actual=%d variance=%d, want 0/200000/200000",
			u.Budgeted, u.Actual, u.Variance)
	}
}

// TestCashflowProjection starts from the CURRENT actual net-asset fund balances
// (FundBalancesAsOf at the period start) and rolls BUDGETED occurrence flows forward
// per fund. Period 2026-07-01..2026-12-31: no fixture transactions after 2026-06-10,
// so FundBalancesAsOf(2026-07-01) == the AsOf oracle FundBalances.
//
// Two budget lines on the BecaAgua/MXN fund:
//   - REVENUE (GovernmentGrants, onetime 2026-08-15, 1,000,000): delta = +1,000,000
//     (a revenue occurrence INCREASES the fund's spendable position).
//   - EXPENSE (ProgramSupplies, onetime 2026-09-15, 400,000): delta = -400,000.
//
// Start BecaAgua MXN = 9,700,000 (oracle). Projected end = 9,700,000 + 1,000,000
// - 400,000 = 10,300,000. BecaAgua USD is untouched: start == end == 50,000.
func TestCashflowProjection(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	// Root scope: BecaAgua spans US+MX, so its consolidated fund balances (MXN 9.7M,
	// USD 50k -- the root-scope oracle FundBalances) are only visible at root.
	tk := tkFor(f, f.IDs.Root)

	budgetID := newBudget(t, ctx, f.Store, "2026-07-01", "2026-12-31")
	revSched := onetimeSchedule(t, ctx, f.Store, "2026-08-15")
	expSched := onetimeSchedule(t, ctx, f.Store, "2026-09-15")
	beca := f.IDs.BecaAgua
	addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.MX, AccountID: f.IDs.GovernmentGrants, FundID: &beca,
		ProgramID: f.IDs.Educacion, Amount: 1_000_000, Currency: "MXN", ScheduleID: revSched,
	})
	addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.MX, AccountID: f.IDs.ProgramSupplies, FundID: &beca,
		ProgramID: f.IDs.Educacion, Amount: 400_000, Currency: "MXN", ScheduleID: expSched,
	})

	proj, err := tk.CashflowProjection(ctx, reports.Scope{Sub: f.IDs.Root}, budgetID, "2026-07-01", "2026-12-31")
	if err != nil {
		t.Fatalf("CashflowProjection: %v", err)
	}

	becaMXN := reports.FundCurrency{Fund: f.IDs.BecaAgua, Currency: "MXN"}
	fund, ok := proj[becaMXN]
	if !ok {
		t.Fatalf("no BecaAgua/MXN projection series")
	}
	// Start = current actual balance (FundBalancesAsOf period start).
	if fund.Start != 9_700_000 {
		t.Errorf("BecaAgua/MXN start = %d, want 9700000 (FundBalancesAsOf period start)", fund.Start)
	}
	// End = start + revenue - expense = 10,300,000.
	if fund.End != 10_300_000 {
		t.Errorf("BecaAgua/MXN end = %d, want 10300000 (9700000 + 1000000 - 400000)", fund.End)
	}
	// The running balance At each budgeted-occurrence date, in chronological order:
	// after the 2026-08-15 revenue +1,000,000 => 10,700,000; after the 2026-09-15
	// expense -400,000 => 10,300,000. FlowDates lists exactly those two dates.
	if got := []string{"2026-08-15", "2026-09-15"}; len(fund.FlowDates) != 2 ||
		fund.FlowDates[0] != got[0] || fund.FlowDates[1] != got[1] {
		t.Errorf("BecaAgua/MXN FlowDates = %v, want %v", fund.FlowDates, got)
	}
	if fund.At["2026-08-15"] != 10_700_000 {
		t.Errorf("BecaAgua/MXN At[2026-08-15] = %d, want 10700000 (after revenue)", fund.At["2026-08-15"])
	}
	if fund.At["2026-09-15"] != 10_300_000 {
		t.Errorf("BecaAgua/MXN At[2026-09-15] = %d, want 10300000 (after expense)", fund.At["2026-09-15"])
	}

	// BecaAgua/USD carries no budgeted flow: start == end == 50,000 (the oracle).
	becaUSD := reports.FundCurrency{Fund: f.IDs.BecaAgua, Currency: "USD"}
	usd, ok := proj[becaUSD]
	if !ok {
		t.Fatalf("no BecaAgua/USD projection series")
	}
	if usd.Start != 50_000 || usd.End != 50_000 {
		t.Errorf("BecaAgua/USD start/end = %d/%d, want 50000/50000 (no budgeted flow)", usd.Start, usd.End)
	}
}

// TestBudgetBucketBoundaries pins the deterministic bucket keys at quarter and year
// granularity: an occurrence on a quarter boundary (Q3 start 2026-07-01) buckets to
// 2026-07-01 (quarter) and 2026-01-01 (year), an occurrence just before a boundary
// (2026-09-30, Q3 end) buckets to the SAME quarter (2026-07-01), and one in Q4
// (2026-10-01) buckets to 2026-10-01 -- proving the bucketing is by the occurrence
// date's period, consistently.
func TestBudgetBucketBoundaries(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	tk := tkFor(f, f.IDs.US)

	budgetID := newBudget(t, ctx, f.Store, "2026-07-01", "2026-12-31")
	key := reports.BudgetKey{
		Subsidiary: f.IDs.US, Account: f.IDs.Salaries, Fund: 0,
		Program: f.IDs.General, Currency: "USD",
	}
	addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.US, AccountID: f.IDs.Salaries, ProgramID: f.IDs.General,
		Amount: 10_000, Currency: "USD", ScheduleID: onetimeSchedule(t, ctx, f.Store, "2026-07-01"),
	})
	addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.US, AccountID: f.IDs.Salaries, ProgramID: f.IDs.General,
		Amount: 20_000, Currency: "USD", ScheduleID: onetimeSchedule(t, ctx, f.Store, "2026-09-30"),
	})
	addLine(t, ctx, f.Store, budgetID, store.BudgetLineInput{
		SubsidiaryID: f.IDs.US, AccountID: f.IDs.Salaries, ProgramID: f.IDs.General,
		Amount: 30_000, Currency: "USD", ScheduleID: onetimeSchedule(t, ctx, f.Store, "2026-10-01"),
	})

	// Quarter: 2026-07-01 and 2026-09-30 share Q3 bucket 2026-07-01 (10000 + 20000 =
	// 30000); 2026-10-01 is Q4 bucket 2026-10-01 (30000).
	q, err := tk.BudgetVsActual(ctx, reports.Scope{Sub: f.IDs.US}, budgetID, "2026-07-01", "2026-12-31", reports.GranQuarter)
	if err != nil {
		t.Fatalf("BudgetVsActual quarter: %v", err)
	}
	if c, ok := avbFind(q, "2026-07-01", key); !ok || c.Budgeted != 30_000 {
		t.Errorf("Q3 bucket budgeted = %d/%v, want 30000", c.Budgeted, ok)
	}
	if c, ok := avbFind(q, "2026-10-01", key); !ok || c.Budgeted != 30_000 {
		t.Errorf("Q4 bucket budgeted = %d/%v, want 30000", c.Budgeted, ok)
	}

	// Year: all three occurrences are in 2026 => one bucket 2026-01-01 (60000).
	y, err := tk.BudgetVsActual(ctx, reports.Scope{Sub: f.IDs.US}, budgetID, "2026-07-01", "2026-12-31", reports.GranYear)
	if err != nil {
		t.Fatalf("BudgetVsActual year: %v", err)
	}
	if c, ok := avbFind(y, "2026-01-01", key); !ok || c.Budgeted != 60_000 {
		t.Errorf("year bucket budgeted = %d/%v, want 60000", c.Budgeted, ok)
	}
}

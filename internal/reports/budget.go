package reports

// budget.go is the p19.2 BUDGET toolkit: occurrence bucketing, actuals-vs-budget,
// and cashflow projection, layered over p19.1 (internal/budget.ExpandSchedule + the
// budget store CRUD) and the p15.2 actuals toolkit (BudgetKeyActivity /
// FundBalancesAsOf). Like the rest of the toolkit every method is a pure read
// (rule 2): it composes store reads and never writes. Money stays int64 minor units
// (rule 3), native currency (no conversion -- these methods default native, like the
// other activity toolkits; a converting variant is a later concern).
//
// The budgeting design point (PLAN Phase 19): DISCRETE dated occurrences, NO
// pro-rata. A budget line's schedule expands (via ExpandSchedule) to concrete
// occurrence DATES; each occurrence's FULL amount lands in the SINGLE period bucket
// its date falls in -- a monthly line shown weekly puts its whole amount in ONE
// week, never spread across the ~4.3 weeks. Bucketing is deterministic and
// clock-free (the period is a param).
//
// Sign convention (DECISIONS p19.2): a budget line's stored amount is a POSITIVE
// magnitude; the toolkit signs it by ACCOUNT TYPE into Activity's net-debit space --
// an EXPENSE line budgets a POSITIVE net-debit, a REVENUE line a NEGATIVE one -- so
// budgeted and actual are directly comparable. Variance = actual - budgeted
// (positive = higher net-debit than budget: expense OVERSPEND / revenue UNDER-
// collection). CashflowProjection flips the sign for a fund's SPENDABLE position:
// delta = -(net-debit), so a revenue occurrence INCREASES and an expense occurrence
// DECREASES the fund (mirroring FundPeriodStatement's Received/Applied folding).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"cuento/internal/budget"
)

// BudgetKey is the (subsidiary, account, fund, program, currency) tuple a budget
// line and its actual share -- the grain ActualsVsBudget compares. Fund 0 is the
// unrestricted group (D20), matching BudgetKeyActivity's COALESCE.
type BudgetKey struct {
	Subsidiary SubsidiaryID
	Account    AccountID
	Fund       FundID // 0 = unrestricted
	Program    ProgramID
	Currency   string
}

// BudgetVsActualCell is one (bucket, key) row of ActualsVsBudget: the budgeted and
// actual net-debit amounts (int64 minor) and their variance. Bucket is the period
// bucket key (a "YYYY-MM-DD" -- ISO-Monday week start / month first / quarter first /
// year first). A cell exists when EITHER budgeted or actual is present, so a report
// renders unbudgeted-actual and unspent-budget rows alike.
type BudgetVsActualCell struct {
	Bucket   string
	Key      BudgetKey
	Budgeted int64 // signed net-debit (expense +, revenue -)
	Actual   int64 // signed net-debit (p15.2 Activity grain)
	Variance int64 // actual - budgeted
}

// FundCurrency identifies one projection series: a fund (0 = unrestricted) and a
// currency. CashflowProjection keys its result by this so restricted and
// unrestricted, and each currency, are tracked SEPARATELY (D20).
type FundCurrency struct {
	Fund     FundID
	Currency string
}

// ProjectionSeries is one fund/currency's cashflow projection: the Start balance
// (the current actual net-asset fund balance at the period start), the End balance
// (Start rolled forward through every budgeted occurrence to period end), and the
// running balance At each budgeted-occurrence DATE. FlowDates are the sorted
// occurrence dates (ascending) at which a budgeted flow moves this fund/currency;
// At[d] is the running balance immediately AFTER that date's flow(s). Dates with no
// flow for this fund/currency are not listed (a report that wants period buckets
// re-buckets FlowDates itself). Between two listed dates the balance is constant, so
// the running balance on any calendar day is At of the latest FlowDate on-or-before
// it (or Start before the first).
type ProjectionSeries struct {
	Start     int64            // current actual balance at period start (minor)
	End       int64            // Start + all budgeted flows to period end (minor)
	FlowDates []string         // sorted budgeted-occurrence dates (ascending)
	At        map[string]int64 // occurrence date -> running balance just after its flow(s)
}

// budgetLineForExpand is a resolved budget line ready for schedule expansion: its
// key, its per-occurrence magnitude, and its expanded Schedule.
type budgetLineForExpand struct {
	key      BudgetKey
	amount   int64
	schedule budget.Schedule
}

// BudgetVsActual computes, per (bucket, key), the budgeted vs actual net-debit
// activity over [from,to] at granularity g. BUDGETED = the sum of the budget's line
// occurrences (each line's schedule expanded to dates in [from,to], each occurrence
// contributing its FULL signed amount to its date's bucket -- NO pro-rata). ACTUAL =
// the p15.2 net-debit activity for the same (sub,account,fund,program,currency) key,
// bucketed by each split's own date. Variance = actual - budgeted. The result
// carries a cell wherever either side is nonzero.
func (tk *Toolkit) BudgetVsActual(ctx context.Context, s Scope, budgetID int64, from, to string, g Granularity) ([]BudgetVsActualCell, error) {
	lines, err := tk.resolveBudgetLines(ctx, budgetID)
	if err != nil {
		return nil, err
	}

	// cell[bucket][key] accumulates budgeted and actual.
	type bk struct {
		bucket string
		key    BudgetKey
	}
	budgeted := make(map[bk]int64)
	actual := make(map[bk]int64)

	// BUDGETED: expand each line's schedule and bucket each occurrence's full amount.
	for _, ln := range lines {
		occ, err := budget.ExpandSchedule(ln.schedule, from, to)
		if err != nil {
			return nil, fmt.Errorf("budget vs actual: expand line schedule: %w", err)
		}
		for _, d := range occ {
			b, err := bucketKey(d, g)
			if err != nil {
				return nil, err
			}
			budgeted[bk{b, ln.key}] += ln.amount
		}
	}

	// ACTUAL: p15.2 net-debit activity per key, bucketed by each split's date.
	cells, err := tk.store.BudgetKeyActivity(ctx, from, to, s.Sub)
	if err != nil {
		return nil, fmt.Errorf("budget vs actual: actuals: %w", err)
	}
	for _, c := range cells {
		b, err := bucketKey(c.Date, g)
		if err != nil {
			return nil, err
		}
		key := BudgetKey{
			Subsidiary: c.SubsidiaryID, Account: c.AccountID, Fund: c.FundID,
			Program: c.ProgramID, Currency: c.Currency,
		}
		actual[bk{b, key}] += c.Amount
	}

	// Union the two maps into cells, sorted deterministically.
	seen := make(map[bk]bool)
	for k := range budgeted {
		seen[k] = true
	}
	for k := range actual {
		seen[k] = true
	}
	out := make([]BudgetVsActualCell, 0, len(seen))
	for k := range seen {
		bAmt, aAmt := budgeted[k], actual[k]
		out = append(out, BudgetVsActualCell{
			Bucket:   k.bucket,
			Key:      k.key,
			Budgeted: bAmt,
			Actual:   aAmt,
			Variance: aAmt - bAmt,
		})
	}
	sortCells(out)
	return out, nil
}

// CashflowProjection projects each fund/currency's SPENDABLE net-asset position over
// [from,to]: Start = the current actual FundBalancesAsOf at the period start, rolled
// FORWARD chronologically through the budget's occurrence flows to period end.
// Revenue occurrences INCREASE, expense occurrences DECREASE the fund's spendable
// position (delta = -(net-debit) -- the opposite sign to BudgetVsActual, mirroring
// FundPeriodStatement). Per fund and per currency (restricted vs unrestricted
// tracked separately, D20). Native currency (no conversion).
func (tk *Toolkit) CashflowProjection(ctx context.Context, s Scope, budgetID int64, from, to string) (map[FundCurrency]ProjectionSeries, error) {
	lines, err := tk.resolveBudgetLines(ctx, budgetID)
	if err != nil {
		return nil, err
	}

	// Start balances: the current actual net-asset fund balances at the period start
	// (as-of `from`; there is no occurrence on `from` by construction of the caller,
	// so from vs from-1 cannot move a number -- DECISIONS p19.2).
	startFB, err := tk.store.FundBalancesAsOf(ctx, from, s.Sub)
	if err != nil {
		return nil, fmt.Errorf("cashflow projection: start balances: %w", err)
	}

	series := make(map[FundCurrency]ProjectionSeries)
	for _, fb := range startFB {
		fc := FundCurrency{Fund: fb.FundID, Currency: fb.Currency}
		series[fc] = ProjectionSeries{Start: fb.Amount, End: fb.Amount, At: map[string]int64{}}
	}

	// flow[fc][date] = the day's signed spendable delta from budgeted occurrences.
	type fcDate struct {
		fc   FundCurrency
		date string
	}
	flows := make(map[fcDate]int64)
	dateSet := make(map[string]bool)
	for _, ln := range lines {
		occ, err := budget.ExpandSchedule(ln.schedule, from, to)
		if err != nil {
			return nil, fmt.Errorf("cashflow projection: expand line schedule: %w", err)
		}
		fc := FundCurrency{Fund: ln.key.Fund, Currency: ln.key.Currency}
		// Spendable delta = -(net-debit budgeted amount): revenue (negative net-debit)
		// increases, expense (positive net-debit) decreases the fund.
		delta := -ln.amount
		for _, d := range occ {
			flows[fcDate{fc, d}] += delta
			dateSet[d] = true
			// Ensure a series exists even if the fund had no starting balance.
			if _, ok := series[fc]; !ok {
				series[fc] = ProjectionSeries{At: map[string]int64{}}
			}
		}
	}

	// Sorted flow dates for the chronological roll-forward.
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	// Roll each series forward, recording the running balance at each flow date and
	// the final End.
	for fc, ser := range series {
		running := ser.Start
		for _, d := range dates {
			if delta, ok := flows[fcDate{fc, d}]; ok {
				running += delta
				ser.At[d] = running
				ser.FlowDates = append(ser.FlowDates, d)
			}
		}
		ser.End = running
		series[fc] = ser
	}
	return series, nil
}

// resolveBudgetLines reads a budget's lines and maps each to its BudgetKey, amount,
// and expandable Schedule (via the schedule store rows). Fund NULL -> fund 0.
func (tk *Toolkit) resolveBudgetLines(ctx context.Context, budgetID int64) ([]budgetLineForExpand, error) {
	// Confirm the budget exists (a clean error over an empty result for a bad id).
	if _, err := tk.store.GetBudget(ctx, budgetID); err != nil {
		return nil, fmt.Errorf("cashflow/actuals: load budget %d: %w", budgetID, err)
	}
	lines, err := tk.store.BudgetLines(ctx, budgetID)
	if err != nil {
		return nil, fmt.Errorf("load budget %d lines: %w", budgetID, err)
	}
	// Account type per id (revenue/expense) to sign the amount, read once.
	tree, err := tk.store.Tree(ctx, "en", nil)
	if err != nil {
		return nil, fmt.Errorf("load account tree: %w", err)
	}
	acctType := make(map[AccountID]string, len(tree))
	for _, r := range tree {
		acctType[r.ID] = r.Type
	}

	// Cache resolved schedules by id (a schedule is reusable across lines).
	schedCache := make(map[int64]budget.Schedule)
	out := make([]budgetLineForExpand, 0, len(lines))
	for _, ln := range lines {
		sched, ok := schedCache[ln.ScheduleID]
		if !ok {
			sched, err = tk.loadSchedule(ctx, ln.ScheduleID)
			if err != nil {
				return nil, err
			}
			schedCache[ln.ScheduleID] = sched
		}
		fund := FundID(0)
		if ln.FundID.Valid {
			fund = ln.FundID.Int64
		}
		// Sign the stored positive magnitude by account type into net-debit space:
		// expense budgets a positive net-debit, revenue a negative one.
		amount := ln.Amount
		if acctType[ln.AccountID] == "revenue" {
			amount = -amount
		}
		out = append(out, budgetLineForExpand{
			key: BudgetKey{
				Subsidiary: ln.SubsidiaryID, Account: ln.AccountID, Fund: fund,
				Program: ln.ProgramID, Currency: ln.Currency,
			},
			amount:   amount,
			schedule: sched,
		})
	}
	return out, nil
}

// loadSchedule maps a stored budget schedule (+ its custom date list) to the pure
// budget.Schedule the expansion engine consumes.
func (tk *Toolkit) loadSchedule(ctx context.Context, scheduleID int64) (budget.Schedule, error) {
	row, err := tk.store.GetSchedule(ctx, scheduleID)
	if err != nil {
		return budget.Schedule{}, fmt.Errorf("load schedule %d: %w", scheduleID, err)
	}
	sched := budget.Schedule{
		Kind:          row.Kind,
		DayOfMonth:    nullInt(row.DayOfMonth),
		DayOfMonth2:   nullInt(row.DayOfMonth2),
		Ordinal:       nullInt(row.Ordinal),
		Weekday:       nullInt(row.Weekday),
		AnchorDate:    nullStr(row.AnchorDate),
		WeekendAdjust: row.WeekendAdjust,
	}
	if row.Kind == budget.KindCustom {
		dates, err := tk.store.ScheduleDates(ctx, scheduleID)
		if err != nil {
			return budget.Schedule{}, fmt.Errorf("load schedule %d dates: %w", scheduleID, err)
		}
		sched.CustomDates = dates
	}
	return sched, nil
}

// nullInt maps a nullable schedule int column to the engine's int (invalid -> 0,
// the engine's "unset" sentinel). Mirrors store.intOrZero over the sqlc null type.
func nullInt(n sql.NullInt64) int {
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}

// nullStr maps a nullable schedule string column to the engine's string (invalid ->
// "", the engine's "unset" sentinel).
func nullStr(n sql.NullString) string {
	if !n.Valid {
		return ""
	}
	return n.String
}

// --- bucketing (deterministic, no clock) -----------------------------------

// bucketKey maps an occurrence/split date to its period bucket key at granularity g,
// as a "YYYY-MM-DD" string:
//   - GranWeek: the ISO week start (the MONDAY on-or-before the date). Documented
//     week definition (DECISIONS p19.2): weeks start MONDAY.
//   - GranMonth: the first of the date's month (YYYY-MM-01).
//   - GranQuarter: the first of the date's quarter (Jan/Apr/Jul/Oct-01).
//   - GranYear: the first of the date's year (YYYY-01-01).
//   - GranNone: a single all-period bucket keyed by the date's year start (a
//     degenerate single column; callers wanting real buckets pass a real g).
//
// The mapping is by the occurrence's OWN date -- the no-pro-rata rule: one
// occurrence lands wholly in exactly one bucket.
func bucketKey(date string, g Granularity) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		return "", fmt.Errorf("bucket key: parse date %q: %w", date, err)
	}
	switch g {
	case GranWeek:
		// ISO week start: roll back to Monday. Go's Weekday is Sun=0..Sat=6; the
		// Monday-based offset is (weekday+6)%7.
		back := (int(t.Weekday()) + 6) % 7
		return t.AddDate(0, 0, -back).Format("2006-01-02"), nil
	case GranQuarter:
		q := (int(t.Month()) - 1) / 3     // 0..3
		firstMonth := time.Month(q*3 + 1) // Jan/Apr/Jul/Oct
		return fmt.Sprintf("%04d-%02d-01", t.Year(), int(firstMonth)), nil
	case GranYear, GranNone:
		return fmt.Sprintf("%04d-01-01", t.Year()), nil
	default: // GranMonth
		return fmt.Sprintf("%04d-%02d-01", t.Year(), int(t.Month())), nil
	}
}

// bucketEnd maps a bucket START key (as produced by bucketKey) to that bucket's
// inclusive END date at granularity g, as a "YYYY-MM-DD" string:
//   - GranWeek: start + 6 days (the Sunday closing a Monday-start week).
//   - GranMonth: the last day of the start's month.
//   - GranQuarter: the last day of the quarter (start month + 2, its last day).
//   - GranYear / GranNone: December 31 of the start's year.
//
// A report drilling an actuals cell clamps [bucketStart, bucketEnd] to the report
// window [from,to] so the drilled split set matches the cell (a Monday-start week
// bucket can begin before `from`; the clamp restores the reconciliation invariant).
func bucketEnd(bucketStart string, g Granularity) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", bucketStart, time.UTC)
	if err != nil {
		return "", fmt.Errorf("bucket end: parse date %q: %w", bucketStart, err)
	}
	switch g {
	case GranWeek:
		return t.AddDate(0, 0, 6).Format("2006-01-02"), nil
	case GranQuarter:
		// Start is the first of the quarter's first month; end = last day of +2 months.
		return endOfMonth(t.AddDate(0, 2, 0)), nil
	case GranYear, GranNone:
		return fmt.Sprintf("%04d-12-31", t.Year()), nil
	default: // GranMonth
		return endOfMonth(t), nil
	}
}

// endOfMonth returns the last calendar day of t's month as a "YYYY-MM-DD" string
// (the day before the first of the next month), handling month/short-month lengths.
func endOfMonth(t time.Time) string {
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, 0).AddDate(0, 0, -1)
	return last.Format("2006-01-02")
}

// nextDay returns the calendar day after d ("YYYY-MM-DD"). Used to step from one
// bucket's inclusive end to the next bucket's first day when enumerating projection
// buckets over a period.
func nextDay(d string) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", d, time.UTC)
	if err != nil {
		return "", fmt.Errorf("next day: parse date %q: %w", d, err)
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02"), nil
}

// sortCells orders AVB cells deterministically: by bucket, then the key's fields.
func sortCells(cells []BudgetVsActualCell) {
	sort.Slice(cells, func(i, j int) bool {
		a, b := cells[i], cells[j]
		if a.Bucket != b.Bucket {
			return a.Bucket < b.Bucket
		}
		if a.Key.Subsidiary != b.Key.Subsidiary {
			return a.Key.Subsidiary < b.Key.Subsidiary
		}
		if a.Key.Account != b.Key.Account {
			return a.Key.Account < b.Key.Account
		}
		if a.Key.Fund != b.Key.Fund {
			return a.Key.Fund < b.Key.Fund
		}
		if a.Key.Program != b.Key.Program {
			return a.Key.Program < b.Key.Program
		}
		return a.Key.Currency < b.Key.Currency
	})
}

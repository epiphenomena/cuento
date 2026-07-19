package reports

// budget_plan.go is the p27.3 BUDGET-PLAN toolkit: the two computation methods the
// redesigned budget reports layer over the p27.2 split-derived model (budget_plans /
// budget_splits) -- replacing the retired schedule-expansion toolkit (budget.go).
// Like the rest of the toolkit every method is a pure read (rule 2): it composes
// store reads and never writes. Money stays int64 minor units (rule 3), native
// currency (no conversion -- these methods default native, like the other activity
// toolkits).
//
// The redesign's data source is a set of PROJECTED, dated single-legged splits (NOT
// the real ledger), each carrying account/fund/program/amount/currency/date. A
// budget-split stores a POSITIVE magnitude (p27.2c); the reports apply DIRECTION:
//
//   - Cash-flow projection classifies each split as an INFLOW or OUTFLOW by the
//     categorized leg's account TYPE (DECISIONS "Budget redesign"): revenue and an
//     open_item receivable (asset) are inflows (+cash); expense and an open_item
//     payable (liability) are outflows (-cash). Opening cash is PER FUND from ACTUALS
//     (CurrentCashFundBalancesAsOf), not from a budget leg.
//   - Budget variance signs each split into NET-DEBIT space (expense +, revenue -) so
//     the projected figure lines up with BudgetKeyActivity's actuals (which are R/E
//     net-debit). Variance = actual - budgeted, mirroring the retired toolkit's sign.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// --- shared budget-report types (grain + series) ---------------------------

// BudgetKey is the (subsidiary, account, fund, program, currency) tuple a budget
// figure and its actual share -- the grain BudgetVariancePlan compares. Fund 0 is
// the unrestricted group (D20), matching BudgetKeyActivity's COALESCE.
type BudgetKey struct {
	Subsidiary SubsidiaryID
	Account    AccountID
	Fund       FundID // 0 = unrestricted
	Program    ProgramID
	Currency   string
}

// BudgetVsActualCell is one (bucket, key) row of BudgetVariancePlan: the budgeted and
// actual net-debit amounts (int64 minor) and their variance. Bucket is the period
// bucket key. A cell exists when EITHER budgeted or actual is present.
type BudgetVsActualCell struct {
	Bucket   string
	Key      BudgetKey
	Budgeted int64 // signed net-debit (expense +, revenue -)
	Actual   int64 // signed net-debit (p15.2 Activity grain)
	Variance int64 // actual - budgeted
}

// FundCurrency identifies one projection series: a fund (0 = unrestricted) and a
// currency. CashflowProjectionPlan keys its result by this so restricted and
// unrestricted, and each currency, are tracked SEPARATELY (D20).
type FundCurrency struct {
	Fund     FundID
	Currency string
}

// ProjectionSeries is one fund/currency's cash projection: the Start balance (the
// current actual current-cash balance at the period start), the End balance (Start
// rolled forward through every projected flow to period end), and the running
// balance At each flow DATE. FlowDates are the sorted flow dates (ascending) at which
// a projected flow moves this fund/currency; At[d] is the running balance immediately
// AFTER that date's flow(s). Between two listed dates the balance is constant, so the
// running balance on any day is At of the latest FlowDate on-or-before it (or Start
// before the first).
type ProjectionSeries struct {
	Start     int64            // current actual current-cash balance at period start (minor)
	End       int64            // Start + all projected flows to period end (minor)
	FlowDates []string         // sorted projected-flow dates (ascending)
	At        map[string]int64 // flow date -> running balance just after its flow(s)
}

// budgetSplitResolved is a budget-split with its account type/open_item resolved --
// the shape both toolkit methods iterate. Fund 0 = unrestricted (D20). Program 0 =
// none (only A/L legs, which carry no program).
type budgetSplitResolved struct {
	key      BudgetKey
	amount   int64  // stored POSITIVE magnitude (direction applied by the caller)
	acctType string // "revenue" | "expense" | "asset" | "liability"
	openItem bool
	date     string
}

// planSplitsResolved reads a plan's splits (confirming the plan exists for a clean
// error on a bad id) and resolves each split's account type + open_item flag from
// the account tree, once per run. It is the shared front-half of both budget-plan
// report methods.
func (tk *Toolkit) planSplitsResolved(ctx context.Context, planID BudgetPlanID) ([]budgetSplitResolved, error) {
	if _, err := tk.store.GetBudgetPlan(ctx, planID); err != nil {
		return nil, fmt.Errorf("budget report: load plan %d: %w", planID, err)
	}
	splits, err := tk.store.BudgetSplits(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("budget report: load plan %d splits: %w", planID, err)
	}
	tree, err := tk.store.Tree(ctx, "en", nil)
	if err != nil {
		return nil, fmt.Errorf("budget report: load account tree: %w", err)
	}
	type acctInfo struct {
		typ      string
		openItem bool
	}
	info := make(map[int64]acctInfo, len(tree))
	for _, r := range tree {
		info[r.ID] = acctInfo{typ: r.Type, openItem: r.OpenItem}
	}
	out := make([]budgetSplitResolved, 0, len(splits))
	for _, sp := range splits {
		ai := info[sp.AccountID]
		out = append(out, budgetSplitResolved{
			key: BudgetKey{
				Account:  AccountID(sp.AccountID),
				Fund:     fundOrZero(sp.FundID),
				Program:  progOrZero(sp.ProgramID),
				Currency: sp.Currency,
			},
			amount:   sp.Amount,
			acctType: ai.typ,
			openItem: ai.openItem,
			date:     sp.Date,
		})
	}
	return out, nil
}

// fundOrZero maps a nullable fund_id to the fund-0 unrestricted convention (D20).
func fundOrZero(n sql.NullInt64) FundID {
	if n.Valid {
		return FundID(n.Int64)
	}
	return 0
}

// progOrZero maps a nullable program_id to 0 (no program -- only A/L legs).
func progOrZero(n sql.NullInt64) ProgramID {
	if n.Valid {
		return ProgramID(n.Int64)
	}
	return 0
}

// cashDirection classifies a budget-split's cash direction: +1 for an INFLOW
// (revenue, or an open_item receivable asset), -1 for an OUTFLOW (expense, or an
// open_item payable liability). It returns 0 for a split whose account is neither
// (which the store rejects, so this is a defensive skip, not a live path).
func cashDirection(acctType string, openItem bool) int64 {
	switch acctType {
	case "revenue":
		return +1
	case "expense":
		return -1
	case "asset":
		if openItem {
			return +1 // receivable: an expected collection is an inflow
		}
	case "liability":
		if openItem {
			return -1 // payable: an expected settlement is an outflow
		}
	}
	return 0
}

// netDebitSign maps an R/E account type to the sign that puts a POSITIVE-magnitude
// budget-split into net-debit space (expense +, revenue -), so a projected figure
// is directly comparable to BudgetKeyActivity's actuals. Returns 0 for a non-R/E
// account (an A/L open-item leg has no actuals counterpart in BudgetKeyActivity).
func netDebitSign(acctType string) int64 {
	switch acctType {
	case "expense":
		return +1
	case "revenue":
		return -1
	default:
		return 0
	}
}

// CashflowProjectionPlan projects each fund/currency's SPENDABLE cash position over
// [from,to] from the plan's budget-splits (p27.3). Start = each fund's ACTUAL
// current-cash balance as of the period start (CurrentCashFundBalancesAsOf), rolled
// FORWARD through the projected inflows/outflows (classified by account type) to
// period end. Per fund and per currency (restricted vs unrestricted tracked
// separately, D20). Native currency (no conversion).
func (tk *Toolkit) CashflowProjectionPlan(ctx context.Context, s Scope, planID BudgetPlanID, from, to string) (map[FundCurrency]ProjectionSeries, error) {
	splits, err := tk.planSplitsResolved(ctx, planID)
	if err != nil {
		return nil, err
	}

	// Start balances: each fund's ACTUAL current-cash balance at the period start
	// (as-of `from`). This is the resolved per-fund opening (DECISIONS tension 1).
	startFB, err := tk.store.CurrentCashFundBalancesAsOf(ctx, from, int64(s.Sub))
	if err != nil {
		return nil, fmt.Errorf("cashflow projection: start balances: %w", err)
	}
	series := make(map[FundCurrency]ProjectionSeries)
	for _, fb := range startFB {
		fc := FundCurrency{Fund: FundID(fb.FundID), Currency: fb.Currency}
		series[fc] = ProjectionSeries{Start: fb.Amount, End: fb.Amount, At: map[string]int64{}}
	}

	// flow[fc][date] = the day's signed CASH delta from the projected splits in
	// [from,to] (inflow +magnitude, outflow -magnitude).
	type fcDate struct {
		fc   FundCurrency
		date string
	}
	flows := make(map[fcDate]int64)
	dateSet := make(map[string]bool)
	for _, sp := range splits {
		if from != "" && sp.date < from {
			continue
		}
		if to != "" && sp.date > to {
			continue
		}
		dir := cashDirection(sp.acctType, sp.openItem)
		if dir == 0 {
			continue
		}
		fc := FundCurrency{Fund: sp.key.Fund, Currency: sp.key.Currency}
		flows[fcDate{fc, sp.date}] += dir * sp.amount
		dateSet[sp.date] = true
		if _, ok := series[fc]; !ok {
			series[fc] = ProjectionSeries{At: map[string]int64{}}
		}
	}

	// Sorted flow dates for the chronological roll-forward.
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Strings(dates)

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

// BudgetVariancePlan computes, per (bucket, key), the budgeted vs actual net-debit
// activity over [from,to] at granularity g (p27.3). BUDGETED = the plan's R/E
// budget-splits signed into net-debit space (expense +, revenue -), each bucketed by
// its OWN date (no pro-rata). ACTUAL = the p15.2 net-debit activity for the same
// (sub,account,fund,program,currency) key. Variance = actual - budgeted. A cell
// exists wherever either side is nonzero.
//
// Only R/E splits participate: BudgetKeyActivity returns only program-carrying R/E
// actuals, so an open_item A/L budget-split has no comparable actual (DECISIONS
// tension 2 -- A/R-A/P variance is a period-net concern out of this report's scope).
func (tk *Toolkit) BudgetVariancePlan(ctx context.Context, s Scope, planID BudgetPlanID, from, to string, g Granularity) ([]BudgetVsActualCell, error) {
	splits, err := tk.planSplitsResolved(ctx, planID)
	if err != nil {
		return nil, err
	}

	type bk struct {
		bucket string
		key    BudgetKey
	}
	budgeted := make(map[bk]int64)
	actual := make(map[bk]int64)

	// BUDGETED: sign each R/E split into net-debit space, bucket by its own date. The
	// budget-split key carries no subsidiary, so we key budgeted rows by the PLAN's
	// subsidiary; the actual side (BudgetKeyActivity) keys by each split's POSTING
	// subsidiary. These align (budgeted + actual merge into one row) as long as the
	// plan's activity posts IN the plan's subsidiary. A plan whose subsidiary has
	// DESCENDANTS that post the same account's activity would land budgeted and actual
	// in separate rows -- an accepted limitation (DECISIONS p27.3a): budget plans are
	// authored at the subsidiary the activity posts to.
	plan, err := tk.store.GetBudgetPlan(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("budget variance: load plan %d: %w", planID, err)
	}
	for _, sp := range splits {
		// p27.4: a program-scoped grant filters BUDGETED rows to the granted subtree
		// (empty ProgramScope => no filter, unscoped/admin unchanged). A budget-split's
		// program is on its key; an out-of-subtree split contributes to no cell.
		if !tk.Params.InProgramScope(sp.key.Program) {
			continue
		}
		sign := netDebitSign(sp.acctType)
		if sign == 0 {
			continue // A/L open-item leg: no actuals counterpart
		}
		b, err := bucketKey(sp.date, g)
		if err != nil {
			return nil, err
		}
		key := sp.key
		key.Subsidiary = SubsidiaryID(plan.SubsidiaryID)
		budgeted[bk{b, key}] += sign * sp.amount
	}

	// ACTUAL: p15.2 net-debit activity per key, bucketed by each split's date.
	cells, err := tk.store.BudgetKeyActivity(ctx, from, to, int64(s.Sub))
	if err != nil {
		return nil, fmt.Errorf("budget variance: actuals: %w", err)
	}
	for _, c := range cells {
		// p27.4: filter ACTUAL rows to the granted subtree too, so a scoped variance
		// report never surfaces a sibling subtree's actuals (BudgetKeyActivity is raw
		// per-program R/E activity). Empty ProgramScope => no filter.
		if !tk.Params.InProgramScope(ProgramID(c.ProgramID)) {
			continue
		}
		b, err := bucketKey(c.Date, g)
		if err != nil {
			return nil, err
		}
		key := BudgetKey{
			Subsidiary: SubsidiaryID(c.SubsidiaryID), Account: AccountID(c.AccountID), Fund: FundID(c.FundID),
			Program: ProgramID(c.ProgramID), Currency: c.Currency,
		}
		actual[bk{b, key}] += c.Amount
	}

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

// --- bucketing (deterministic, no clock) -----------------------------------

// bucketKey maps a split date to its period bucket key at granularity g, as a
// "YYYY-MM-DD" string:
//   - GranWeek: the ISO week start (the MONDAY on-or-before the date; weeks start
//     MONDAY).
//   - GranMonth: the first of the date's month (YYYY-MM-01).
//   - GranQuarter: the first of the date's quarter (Jan/Apr/Jul/Oct-01).
//   - GranYear / GranNone: the first of the date's year (YYYY-01-01).
//
// The mapping is by the split's OWN date -- the no-pro-rata rule: one split lands
// wholly in exactly one bucket.
func bucketKey(date string, g Granularity) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		return "", fmt.Errorf("bucket key: parse date %q: %w", date, err)
	}
	switch g {
	case GranWeek:
		back := (int(t.Weekday()) + 6) % 7
		return t.AddDate(0, 0, -back).Format("2006-01-02"), nil
	case GranQuarter:
		q := (int(t.Month()) - 1) / 3
		firstMonth := time.Month(q*3 + 1)
		return fmt.Sprintf("%04d-%02d-01", t.Year(), int(firstMonth)), nil
	case GranYear, GranNone:
		return fmt.Sprintf("%04d-01-01", t.Year()), nil
	default: // GranMonth
		return fmt.Sprintf("%04d-%02d-01", t.Year(), int(t.Month())), nil
	}
}

// bucketEnd maps a bucket START key (as produced by bucketKey) to that bucket's
// inclusive END date at granularity g, as a "YYYY-MM-DD" string. A report drilling
// an actuals cell clamps [bucketStart, bucketEnd] to the report window [from,to] so
// the drilled split set matches the cell.
func bucketEnd(bucketStart string, g Granularity) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", bucketStart, time.UTC)
	if err != nil {
		return "", fmt.Errorf("bucket end: parse date %q: %w", bucketStart, err)
	}
	switch g {
	case GranWeek:
		return t.AddDate(0, 0, 6).Format("2006-01-02"), nil
	case GranQuarter:
		return endOfMonth(t.AddDate(0, 2, 0)), nil
	case GranYear, GranNone:
		return fmt.Sprintf("%04d-12-31", t.Year()), nil
	default: // GranMonth
		return endOfMonth(t), nil
	}
}

// endOfMonth returns the last calendar day of t's month as a "YYYY-MM-DD" string.
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

// sortCells orders variance cells deterministically: by bucket, then the key's
// fields.
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

package reports

// compute.go is the p15.2 report toolkit: the Appendix-E computation methods
// layered over the p08.4 store balance queries. It is the ONE place the reporting
// concerns live — CONSOLIDATION (a scope = a subsidiary + all descendants, D18),
// CONVERSION (D12: closing rate for balances, per-transaction-date rate for
// activity, half-even rounding at the final aggregate), INTERCOMPANY collapse
// (D19), the EFFECTIVE-990-code rollup (D25), and placeholder tree rollups — so
// p15.3–p15.11 call these methods rather than re-deriving them. Every method is a
// pure read (rule 2): it composes store queries and never writes.
//
// Deviation from the Appendix-E sketch (recorded in DECISIONS): dates are plain
// YYYY-MM-DD STRINGS, not a `ledger.Date` type (no such type exists — the store's
// balance queries and RateOn already take string dates), and the money/rate return
// types are the small value types below (CurAmt / Rate) rather than sketch names.
// Consolidation needs no scope expansion here: the p08.4 queries ALREADY close the
// descendant set with a recursive CTE (balances.go), so a method passes Scope.Sub
// straight through and the store consolidates.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"cuento/internal/money"
)

// AccountID / FundID / ProgramID / SubsidiaryID are report-layer id aliases so the
// Appendix-E signatures read as intended without importing store id types (the
// store uses bare int64). They are plain aliases: a store int64 flows in and out
// freely.
type (
	AccountID    = int64
	FundID       = int64
	ProgramID    = int64
	SubsidiaryID = int64
)

// Scope is a report scope: a subsidiary consolidated with ALL its descendants
// (D18). The root subsidiary = full org consolidation; a leaf = just that sub. The
// p08.4 queries take the sub id and close the descendant set themselves, so Scope
// is a thin typed wrapper the toolkit forwards.
type Scope struct{ Sub SubsidiaryID }

// RateMode selects how a toolkit method converts (D12). RateNone leaves amounts in
// their native currency; RateClosing converts each currency's balance at the AsOf
// closing (on-or-before) rate (balance-sheet treatment); RateTxnDate converts
// activity at each transaction's date rate (P&L treatment), realized here as a
// per-calendar-month decomposition (the fixture's monthly rate schedule makes
// month-granularity exactly per-transaction-date under on-or-before lookup).
type RateMode int

const (
	// RateNone performs no conversion: amounts stay in native currency.
	RateNone RateMode = iota
	// RateClosing converts at the as-of on-or-before rate (balance sheet, D12).
	RateClosing
	// RateTxnDate converts activity month-by-month at each month's rate (P&L, D12).
	RateTxnDate
)

// ConvertOpts is a conversion request (D12): the target currency To and the Mode.
// When Mode is RateNone, To is ignored and results are native-currency.
type ConvertOpts struct {
	To   string
	Mode RateMode
}

// Class is a functional-expense class (D21): program | management | fundraising.
type Class string

// CurAmt is one (currency, minor-unit) amount — the toolkit's money value. Minor
// is exact int64 (rule 3); the only float touch is the single D12 conversion step,
// after which the result is back to int64 minor units.
type CurAmt struct {
	Currency string
	Minor    int64
}

// Rate is a resolved exchange rate plus the ACTUAL date of the row it came from
// (which may predate the query date — staleness a report footnotes, p14.1) and
// whether it was derived reciprocally. It mirrors store.RateResult so reports can
// footnote gaps without importing the store type.
type Rate struct {
	Rate       float64
	RateDate   string
	Reciprocal bool
}

// LineRow is one row of a Group990 effective-code rollup (D25): the effective 990
// code, its amount, and an Unmapped flag for the explicit "" bucket (rendered
// last). A report turns these into table rows under the part's line labels.
type LineRow struct {
	Code     string
	Amount   CurAmt
	Unmapped bool
}

// TreeRow is one row of a placeholder rollup (Rollup): an account (leaf or
// placeholder), its amount, its tree Indent, and whether it is a placeholder
// Subtotal row (a parent's subtree total) versus a leaf data row.
type TreeRow struct {
	AccountID AccountID
	Name      string
	Amount    CurAmt
	Indent    int
	Subtotal  bool
}

// RoundHalfEven rounds a float to the nearest int64, ties to even (D12). Exported
// so tests pin the rule and reports/goldens can reuse the exact primitive.
func RoundHalfEven(x float64) int64 { return int64(math.RoundToEven(x)) }

// BalancesAsOf returns, per account, the per-currency cumulative balance as of d in
// the scope's descendant closure (D18). With o.Mode == RateNone the amounts are
// native (one CurAmt per currency present); with RateClosing each currency is
// converted to o.To at the on-or-before closing rate and collapsed into a single
// o.To CurAmt per account (D12).
func (tk *Toolkit) BalancesAsOf(ctx context.Context, s Scope, d string, o ConvertOpts) (map[AccountID][]CurAmt, error) {
	rows, err := tk.store.SubtreeBalancesAsOf(ctx, d, s.Sub)
	if err != nil {
		return nil, err
	}
	native := make(map[AccountID][]CurAmt, len(rows))
	for _, r := range rows {
		native[r.AccountID] = append(native[r.AccountID], CurAmt{Currency: r.Currency, Minor: r.Amount})
	}
	if o.Mode == RateNone {
		return native, nil
	}
	// Closing conversion: every currency at the as-of rate, collapsed to o.To.
	out := make(map[AccountID][]CurAmt, len(native))
	for acct, amts := range native {
		conv, err := tk.convertClosing(ctx, amts, o.To, d)
		if err != nil {
			return nil, err
		}
		out[acct] = []CurAmt{conv}
	}
	return out, nil
}

// Activity returns, per account, the per-currency signed activity over [from,to] in
// the scope's descendant closure. RateNone yields native amounts; RateTxnDate
// converts month-by-month at each month's rate and rounds the per-account total
// HALF-EVEN once (D12 "at final aggregates"); RateClosing converts the whole-period
// total at the on-or-before as-of rate.
func (tk *Toolkit) Activity(ctx context.Context, s Scope, from, to string, o ConvertOpts) (map[AccountID][]CurAmt, error) {
	excl, err := tk.consolidatedICExclusions(ctx, s.Sub)
	if err != nil {
		return nil, err
	}
	if o.Mode != RateTxnDate {
		rows, err := tk.store.PeriodActivity(ctx, from, to, s.Sub)
		if err != nil {
			return nil, err
		}
		native := make(map[AccountID][]CurAmt, len(rows))
		for _, r := range rows {
			if excl[r.AccountID] {
				continue // intra-group R/E, excluded at consolidation (D19)
			}
			native[r.AccountID] = append(native[r.AccountID], CurAmt{Currency: r.Currency, Minor: r.Amount})
		}
		if o.Mode == RateNone {
			return native, nil
		}
		out := make(map[AccountID][]CurAmt, len(native))
		for acct, amts := range native {
			conv, err := tk.convertClosing(ctx, amts, o.To, to)
			if err != nil {
				return nil, err
			}
			out[acct] = []CurAmt{conv}
		}
		return out, nil
	}

	// TxnDate: decompose the period into calendar months, convert each month's
	// native activity at that month's rate, accumulate the UNROUNDED product per
	// account, and round once. Under on-or-before lookup with the monthly rate
	// schedule, a month resolves to its own (or the latest prior) point — exactly
	// the per-transaction-date rate for every transaction in that month.
	unrounded := make(map[AccountID]float64)
	present := make(map[AccountID]bool)
	err = tk.ByPeriod(from, to, GranMonth, func(pFrom, pTo string) error {
		rows, err := tk.store.PeriodActivity(ctx, pFrom, pTo, s.Sub)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if excl[r.AccountID] {
				continue // intra-group R/E, excluded at consolidation (D19)
			}
			rr, err := tk.RateOn(ctx, r.Currency, o.To, pTo)
			if err != nil {
				return err
			}
			exFrom, exTo, err := tk.exponents(ctx, r.Currency, o.To)
			if err != nil {
				return err
			}
			unrounded[r.AccountID] += float64(r.Amount) * rr.Rate * math.Pow(10, float64(exTo-exFrom))
			present[r.AccountID] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[AccountID][]CurAmt, len(present))
	for acct := range present {
		out[acct] = []CurAmt{{Currency: o.To, Minor: RoundHalfEven(unrounded[acct])}}
	}
	return out, nil
}

// FundBalancesAsOf returns, per fund, the per-currency asset-side (unexpended)
// balance as of d in the scope's descendant closure, INCLUDING the unrestricted
// group as fund id 0 (D20). RateNone yields native per-currency; RateClosing
// converts each fund's currencies to o.To at the as-of rate and collapses to one
// o.To CurAmt.
func (tk *Toolkit) FundBalancesAsOf(ctx context.Context, s Scope, d string, o ConvertOpts) (map[FundID][]CurAmt, error) {
	rows, err := tk.store.FundBalancesAsOf(ctx, d, s.Sub)
	if err != nil {
		return nil, err
	}
	native := make(map[FundID][]CurAmt, len(rows))
	for _, r := range rows {
		native[r.FundID] = append(native[r.FundID], CurAmt{Currency: r.Currency, Minor: r.Amount})
	}
	if o.Mode == RateNone {
		return native, nil
	}
	out := make(map[FundID][]CurAmt, len(native))
	for fund, amts := range native {
		conv, err := tk.convertClosing(ctx, amts, o.To, d)
		if err != nil {
			return nil, err
		}
		out[fund] = []CurAmt{conv}
	}
	return out, nil
}

// FundStatement is one fund's period statement (p15.8): the opening and closing
// SPENDABLE (cash) balances that frame the period, and the RECEIVED / APPLIED flows
// that move between them, per currency. It is the per-grant funder view (Q3) and the
// D20 "released from restrictions" derivation feeding p15.9.
//
// The identity that holds per currency BY CONSTRUCTION (asserted by the golden and
// verified by cuento's fund conservation, D20/Z10):
//
//	Opening + Received − AppliedExpense − AppliedNonExpense == Closing
//
// where every figure is a per-currency map keyed by ISO code. Opening/Closing are the
// fund's SPENDABLE (cash) position — its asset accounts EXCLUDING those it capitalized
// via a non-expense application in-period (a Building purchase, a loan advance). A fund
// that holds only cash (Beca Agua) has spendable == FundBalancesAsOf; a fund that
// capitalized a fixed asset (Building Fund) has spendable == FundBalancesAsOf − the
// capitalized non-expense applications (Capitalized below), so the two reconcile.
type FundStatement struct {
	// Opening is the spendable balance the day before From, per currency (minor).
	Opening map[string]int64
	// Received is the period's inflows (contributions/revenue INTO the fund), per
	// currency, as a POSITIVE magnitude (−Σ revenue splits, since revenue is a credit
	// / negative net-debit).
	Received map[string]int64
	// AppliedExpense is the period's EXPENSE applications (Σ expense splits tagged the
	// fund — positive net-debit), per currency.
	AppliedExpense map[string]int64
	// AppliedNonExpense is the period's NON-EXPENSE applications (asset purchases, loan
	// principal — positive net-debit debits to asset/liability accounts, EXCLUDING the
	// cash accounts that merely sourced a receipt/spend), per currency. The fixture's
	// Building purchase (Building +40,000.00) lands here, NOT in AppliedExpense.
	AppliedNonExpense map[string]int64
	// Closing is the spendable balance as of To, per currency (minor). Equals
	// Opening + Received − AppliedExpense − AppliedNonExpense by construction.
	Closing map[string]int64
	// Capitalized is the running total of NON-EXPENSE applications still held as fund
	// assets (the Building), per currency, so Closing + Capitalized reconciles to the
	// all-asset FundBalancesAsOf(To). For a cash-only fund it is empty.
	Capitalized map[string]int64
	// CapitalAccounts is the set of asset accounts the fund capitalized into (received
	// a non-expense application debit) over the period — the accounts EXCLUDED from the
	// spendable position. Used to drill / reconcile the spendable figure.
	CapitalAccounts map[AccountID]bool
	// Currencies is the sorted union of every currency any figure above uses (a stable
	// section order for the report).
	Currencies []string
}

// FundPeriodStatement derives fund f's period statement over [from,to] in the scope's
// descendant closure (native currency; a fund's splits already live only in its
// subsidiaries so scope narrows nothing but is honored via the store's FundLedger).
// It reads the fund's splits to To (FundLedger), classifies each by its account TYPE
// (revenue / expense / asset / liability), and folds them into the Received/Applied
// buckets, with the day-before-From cut giving the opening balance. NON-EXPENSE
// applications are debits to a NON-CASH asset (or a liability) — the fund CAPITALIZING
// its cash into a held asset; the cash accounts that source those debits stay in the
// spendable balance. The identity Opening+Received−AppliedExpense−AppliedNonExpense ==
// Closing holds per currency because the fund nets to zero within itself (D20/Z10).
func (tk *Toolkit) FundPeriodStatement(ctx context.Context, s Scope, f FundID, from, to string) (FundStatement, error) {
	// Account type per id (revenue/expense/asset/liability/equity), read once.
	tree, err := tk.store.Tree(ctx, "en", nil)
	if err != nil {
		return FundStatement{}, err
	}
	acctType := make(map[AccountID]string, len(tree))
	for _, r := range tree {
		acctType[r.ID] = r.Type
	}

	// All the fund's splits up to To, ordered (date, split_id), with IsAsset and the
	// per-currency asset-side running balance already computed by the store.
	rows, err := tk.store.FundLedger(ctx, f, to)
	if err != nil {
		return FundStatement{}, err
	}

	// PASS 1: find the CAPITAL asset accounts — asset accounts that received a
	// non-expense application DEBIT (amount > 0) on a DISBURSEMENT transaction (a txn
	// with no revenue split for this fund). These are excluded from the spendable
	// position; every OTHER asset account is "cash" (spendable).
	//
	// The classification scans the fund's CUMULATIVE position up to the window end
	// (every row here is already <= to via FundLedger), NOT just the in-window rows:
	// a fund that capitalized an asset BEFORE the window start is still holding that
	// capital asset during the window, so its opening/closing debit balance must be
	// excluded from the spendable figure. Restricting PASS 1 to [from,to] would leave
	// a pre-window capitalized asset classified as "cash", overstating spendable
	// Opening (and thus Closing) for any window that starts after the capitalization.
	revenueTxn := make(map[int64]bool) // txn id -> has a revenue split for this fund
	for _, r := range rows {
		if acctType[r.AccountID] == "revenue" {
			revenueTxn[r.TxnID] = true
		}
	}
	capital := make(map[AccountID]bool)
	for _, r := range rows {
		if r.Date > to {
			continue // defensive; FundLedger already bounds rows at <= to
		}
		if acctType[r.AccountID] == "asset" && r.Amount > 0 && !revenueTxn[r.TxnID] {
			// A positive (debit) asset movement on a disbursement txn = capitalizing
			// cash into a held asset (the Building purchase). That target account is a
			// capital account for this fund.
			capital[r.AccountID] = true
		}
	}

	st := FundStatement{
		Opening:           map[string]int64{},
		Received:          map[string]int64{},
		AppliedExpense:    map[string]int64{},
		AppliedNonExpense: map[string]int64{},
		Closing:           map[string]int64{},
		Capitalized:       map[string]int64{},
		CapitalAccounts:   capital,
	}
	seen := map[string]bool{}

	// PASS 2: fold every split. Opening (< from) accumulates only the SPENDABLE (cash)
	// asset movements. In-period splits fold into the flow buckets by account type.
	for _, r := range rows {
		ccy := r.Currency
		seen[ccy] = true
		spendableAsset := acctType[r.AccountID] == "asset" && !capital[r.AccountID]

		if r.Date < from {
			if spendableAsset {
				st.Opening[ccy] += r.Amount
			}
			continue
		}
		// In period.
		switch acctType[r.AccountID] {
		case "revenue":
			st.Received[ccy] += -r.Amount // credit → positive inflow
		case "expense":
			st.AppliedExpense[ccy] += r.Amount
		case "asset":
			if capital[r.AccountID] {
				if r.Amount > 0 {
					st.AppliedNonExpense[ccy] += r.Amount
					st.Capitalized[ccy] += r.Amount
				}
				// A credit on a capital account (a disposal) would reduce Capitalized;
				// the fixture has none, but handle it for correctness.
				if r.Amount < 0 {
					st.Capitalized[ccy] += r.Amount
					st.AppliedNonExpense[ccy] += r.Amount
				}
			}
			// Non-capital (cash) asset movements do not enter Received/Applied — they
			// are the SOURCE/DESTINATION side, already reflected via the counterpart.
		case "liability":
			// A liability DEBIT in a disbursement (principal paydown) is a non-expense
			// application; a liability CREDIT (a loan draw) is a receipt of resources.
			if r.Amount > 0 {
				st.AppliedNonExpense[ccy] += r.Amount
			} else {
				st.Received[ccy] += -r.Amount
			}
		}
	}

	// Closing (spendable) = Opening + Received − AppliedExpense − AppliedNonExpense,
	// per currency — the identity, computed from the folded flows.
	for ccy := range seen {
		st.Closing[ccy] = st.Opening[ccy] + st.Received[ccy] - st.AppliedExpense[ccy] - st.AppliedNonExpense[ccy]
	}

	// Currency section order: the sorted union of every currency any figure uses.
	for ccy := range seen {
		st.Currencies = append(st.Currencies, ccy)
	}
	sort.Strings(st.Currencies)
	return st, nil
}

// FunctionalMatrix returns, per (expense account, class), the per-currency activity
// over [from,to] in the scope (D21). Only expense splits carry a class, so the
// result is exactly the functional expense matrix cells. RateNone leaves each cell
// native; RateClosing collapses each cell's currencies to o.To at the as-of (to)
// rate; RateTxnDate converts each cell's activity month-by-month at each month's rate
// and rounds the per-cell total HALF-EVEN once (the income-statement / P&L treatment,
// D12) — an expense FLOW is measured at the rate in force when it occurred, so the
// functional-expense total (a period expense flow, NOT a year-end balance) matches the
// income statement's total expenses exactly rather than the closing-rate figure.
func (tk *Toolkit) FunctionalMatrix(ctx context.Context, s Scope, from, to string, o ConvertOpts) (map[AccountID]map[Class][]CurAmt, error) {
	excl, err := tk.consolidatedICExclusions(ctx, s.Sub)
	if err != nil {
		return nil, err
	}

	if o.Mode == RateTxnDate {
		// TxnDate: decompose the period into calendar months (like Activity's TxnDate
		// path), convert each month's native (account,class) activity at that month's
		// on-or-before rate, accumulate the UNROUNDED product per (account,class), and
		// round once. Under the monthly rate schedule this is the per-transaction-date
		// rate for every split in the month — so each expense flow lands at its own rate.
		unrounded := make(map[AccountID]map[Class]float64)
		err := tk.ByPeriod(from, to, GranMonth, func(pFrom, pTo string) error {
			rows, err := tk.store.FunctionalActivity(ctx, pFrom, pTo, s.Sub)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if excl[r.AccountID] {
					continue // intra-group expense, excluded at consolidation (D19)
				}
				rr, err := tk.RateOn(ctx, r.Currency, o.To, pTo)
				if err != nil {
					return err
				}
				exFrom, exTo, err := tk.exponents(ctx, r.Currency, o.To)
				if err != nil {
					return err
				}
				cl := Class(r.FunctionalClass)
				if unrounded[r.AccountID] == nil {
					unrounded[r.AccountID] = make(map[Class]float64)
				}
				unrounded[r.AccountID][cl] += float64(r.Amount) * rr.Rate * math.Pow(10, float64(exTo-exFrom))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		out := make(map[AccountID]map[Class][]CurAmt, len(unrounded))
		for acct, byClass := range unrounded {
			out[acct] = make(map[Class][]CurAmt, len(byClass))
			for cl, v := range byClass {
				out[acct][cl] = []CurAmt{{Currency: o.To, Minor: RoundHalfEven(v)}}
			}
		}
		return out, nil
	}

	rows, err := tk.store.FunctionalActivity(ctx, from, to, s.Sub)
	if err != nil {
		return nil, err
	}
	native := make(map[AccountID]map[Class][]CurAmt)
	for _, r := range rows {
		if excl[r.AccountID] {
			continue // intra-group expense, excluded at consolidation (D19)
		}
		if native[r.AccountID] == nil {
			native[r.AccountID] = make(map[Class][]CurAmt)
		}
		cl := Class(r.FunctionalClass)
		native[r.AccountID][cl] = append(native[r.AccountID][cl], CurAmt{Currency: r.Currency, Minor: r.Amount})
	}
	if o.Mode == RateNone {
		return native, nil
	}
	out := make(map[AccountID]map[Class][]CurAmt, len(native))
	for acct, byClass := range native {
		out[acct] = make(map[Class][]CurAmt, len(byClass))
		for cl, amts := range byClass {
			conv, err := tk.convertClosing(ctx, amts, o.To, to)
			if err != nil {
				return nil, err
			}
			out[acct][cl] = []CurAmt{conv}
		}
	}
	return out, nil
}

// ProgramActivity returns, per (program, account), the per-currency activity over
// [from,to] in the scope (D24). It ADDITIONALLY rolls each program's activity UP
// the program tree: a parent program's cells include its descendant programs'
// activity (so General, the seeded root, carries the whole org's program activity).
// RateClosing converts each cell to o.To at the as-of (to) rate.
func (tk *Toolkit) ProgramActivity(ctx context.Context, s Scope, from, to string, o ConvertOpts) (map[ProgramID]map[AccountID][]CurAmt, error) {
	rows, err := tk.store.ProgramActivity(ctx, from, to, s.Sub)
	if err != nil {
		return nil, err
	}
	excl, err := tk.consolidatedICExclusions(ctx, s.Sub)
	if err != nil {
		return nil, err
	}
	tree, err := tk.store.ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	// ancestorsOf[p] = p and all its ancestors (for the tree rollup).
	parent := make(map[ProgramID]ProgramID, len(tree))
	for _, n := range tree {
		if n.ParentID.Valid {
			parent[n.ID] = n.ParentID.Int64
		}
	}
	// native[program][account] summed per currency, rolled up to ancestors.
	type cell = map[string]int64 // currency -> minor
	acc := make(map[ProgramID]map[AccountID]cell)
	add := func(p ProgramID, a AccountID, ccy string, minor int64) {
		if acc[p] == nil {
			acc[p] = make(map[AccountID]cell)
		}
		if acc[p][a] == nil {
			acc[p][a] = make(cell)
		}
		acc[p][a][ccy] += minor
	}
	for _, r := range rows {
		if excl[r.AccountID] {
			continue // intra-group R/E, excluded at consolidation (D19)
		}
		// Add to the program and every ancestor (tree rollup).
		for p := r.ProgramID; ; {
			add(p, r.AccountID, r.Currency, r.Amount)
			up, ok := parent[p]
			if !ok {
				break
			}
			p = up
		}
	}

	out := make(map[ProgramID]map[AccountID][]CurAmt, len(acc))
	for p, byAcct := range acc {
		out[p] = make(map[AccountID][]CurAmt, len(byAcct))
		for a, byCcy := range byAcct {
			amts := sortedCurAmts(byCcy)
			if o.Mode == RateNone {
				out[p][a] = amts
				continue
			}
			conv, err := tk.convertClosing(ctx, amts, o.To, to)
			if err != nil {
				return nil, err
			}
			out[p][a] = []CurAmt{conv}
		}
	}
	return out, nil
}

// Group990 rolls a leaf (account -> minor) map to EFFECTIVE 990 codes for a part
// (D25): each account's amount lands under its effective code (own, else nearest
// ancestor's — a leaf override lands on its OWN line); accounts with no effective
// code fall into an explicit Unmapped bucket (code "", Unmapped=true) rendered
// LAST. Rows are ordered by the part's line sort order (form990_lines), Unmapped
// last. The amounts are already in the report currency currency (the caller
// converts before rolling up), so Group990 does no conversion.
func (tk *Toolkit) Group990(ctx context.Context, part, currency string, leaf map[AccountID]int64) ([]LineRow, error) {
	eff, err := tk.store.Effective990Codes(ctx)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]int64)
	for acct, minor := range leaf {
		byCode[eff[acct]] += minor // absent -> "" (Unmapped)
	}
	// Order codes by the part's line sort order (form990_lines sort,code). Codes
	// outside the part still render under their own code (sorted after known lines)
	// — but a well-formed leaf map for a part only carries that part's accounts.
	// Unmapped ("") is always last.
	order, err := tk.codeOrder(ctx)
	if err != nil {
		return nil, err
	}
	var codes []string
	for c := range byCode {
		codes = append(codes, c)
	}
	sort.SliceStable(codes, func(i, j int) bool {
		ci, cj := codes[i], codes[j]
		if ci == "" { // Unmapped last
			return false
		}
		if cj == "" {
			return true
		}
		oi, iok := order[ci]
		oj, jok := order[cj]
		switch {
		case iok && jok:
			return oi < oj
		case iok:
			return true
		case jok:
			return false
		default:
			return ci < cj
		}
	})
	rows := make([]LineRow, 0, len(codes))
	for _, c := range codes {
		rows = append(rows, LineRow{
			Code:     c,
			Amount:   CurAmt{Currency: currency, Minor: byCode[c]},
			Unmapped: c == "",
		})
	}
	return rows, nil
}

// PartLine is one 990 line of a part: its code, the line number, and the IRS-seeded
// label (form990_lines reference data, D25). The functional-expenses report (p15.7)
// renders one grouping row per PartLine (code + label) with the contributing expense
// accounts indented beneath it, in the part's report order. The label is STORED
// reference data (like a currency name or an account name), not a catalog key, so a
// report renders it as a TEXT cell verbatim (rule 9's stored-data carve-out).
type PartLine struct {
	Code  string
	Line  string
	Label string
}

// Part990Lines returns the 990 lines of a part (e.g. "IX" — Statement of Functional
// Expenses) in report order (form990_lines sort), each with its line number and
// IRS-seeded label. It reuses Form990LinesForType (the same reference read Group990's
// ordering uses) filtered to the part's account type: Part IX lines are all
// "expense" lines, so the accountType argument selects the part. The result lets the
// p15.7 report render a grouping/subtotal row per effective line with the seeded
// label, WITHOUT the report re-reading the reference table or naming a store type.
func (tk *Toolkit) Part990Lines(ctx context.Context, part, accountType string) ([]PartLine, error) {
	opts, err := tk.store.Form990LinesForType(ctx, accountType)
	if err != nil {
		return nil, err
	}
	out := make([]PartLine, 0, len(opts))
	for _, o := range opts {
		if o.Part != part {
			continue
		}
		out = append(out, PartLine{Code: o.Code, Line: o.Line, Label: o.Label})
	}
	return out, nil
}

// EffectiveCodes returns the accountID -> effective 990 code map (D25 inheritance:
// own code, else nearest ancestor's; absent => unmapped). A thin pass-through to the
// store so a report (p15.7) groups its accounts by effective line without importing
// the store, mirroring how Group990 resolves the same map internally.
func (tk *Toolkit) EffectiveCodes(ctx context.Context) (map[AccountID]string, error) {
	return tk.store.Effective990Codes(ctx)
}

// IntercompanyNet computes the residual of the intercompany-flagged accounts (D19)
// per currency across the consolidated scope as of d. On a balanced ledger a
// consolidated scope that covers both sides nets to zero per currency; a NONZERO
// residual is returned so the caller renders a warning row. Currencies that net to
// exactly zero are still returned (so the report can show the checked-zero line).
func (tk *Toolkit) IntercompanyNet(ctx context.Context, s Scope, d string) ([]CurAmt, error) {
	icIDs, err := tk.store.IntercompanyAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	isIC := make(map[AccountID]bool, len(icIDs))
	for _, id := range icIDs {
		isIC[id] = true
	}
	rows, err := tk.store.SubtreeBalancesAsOf(ctx, d, s.Sub)
	if err != nil {
		return nil, err
	}
	byCcy := make(map[string]int64)
	for _, r := range rows {
		if isIC[r.AccountID] {
			byCcy[r.Currency] += r.Amount
		}
	}
	return sortedCurAmts(byCcy), nil
}

// ICResidualSplit is the intercompany residual (D19) decomposed into its two
// components for the CTA presentation (p26.70), each already converted to a single
// target currency (minor): Closing is the residual valued at the as-of CLOSING rate
// (== ConvertMinorAt of IntercompanyNet); Historical is the SAME residual position
// valued FLOW-wise at each contributing transaction's-date rate (the amount actually
// funded, before FX retranslation). Their difference (Closing − Historical) is the
// Cumulative Translation Adjustment — the FX gain/loss from retranslating accumulated
// intercompany balances at the closing rate (ASC 830); Historical is the genuine
// residual (real imbalance) that should approach zero once cutoff timing is fixed. On
// a single-base-currency residual the two are equal (no FX component → all Historical).
type ICResidualSplit struct {
	Closing    int64 // residual @ closing rate, target minor
	Historical int64 // residual @ transaction-date rate, target minor
}

// IntercompanyResidualSplit computes the ICResidualSplit for the consolidated scope as
// of d, converted to `target`. The CLOSING leg converts IntercompanyNet's native
// residual at the as-of rate (the balance-sheet treatment). The HISTORICAL leg values
// the intercompany accounts' cumulative activity from inception to d FLOW-wise — month
// by month at each month's rate, half-even once per currency — exactly the RateTxnDate
// treatment, but over the intercompany-FLAGGED accounts (which Activity EXCLUDES on a
// consolidated scope, so this walks them directly like IntercompanyNet). A same-
// currency-as-target residual has closing == historical (no retranslation), so its CTA
// is zero and it stays entirely in the Historical (real-imbalance) component.
func (tk *Toolkit) IntercompanyResidualSplit(ctx context.Context, s Scope, d, target string) (ICResidualSplit, error) {
	icIDs, err := tk.store.IntercompanyAccountIDs(ctx)
	if err != nil {
		return ICResidualSplit{}, err
	}
	isIC := make(map[AccountID]bool, len(icIDs))
	for _, id := range icIDs {
		isIC[id] = true
	}

	// CLOSING: native residual per currency, each converted at the as-of rate.
	net, err := tk.IntercompanyNet(ctx, s, d)
	if err != nil {
		return ICResidualSplit{}, err
	}
	var closing int64
	for _, a := range net {
		conv, err := tk.ConvertMinorAt(ctx, a.Minor, a.Currency, target, d)
		if err != nil {
			return ICResidualSplit{}, err
		}
		closing += conv
	}

	// HISTORICAL: the intercompany accounts' cumulative activity to d, converted
	// month-by-month at each month's rate (the amount actually funded, D12 flow rate).
	// Accumulate the UNROUNDED product per currency, then round half-even once per
	// currency and sum — mirroring the Activity TxnDate grain. Start the month walk at
	// the earliest intercompany-touching transaction date (not the 1900 inception) so a
	// balanced org with a recent first entry does not decompose a century of empty
	// months every run; "" (no IC activity) short-circuits to zero historical.
	from, err := tk.earliestICYear(ctx, s.Sub, isIC, d)
	if err != nil {
		return ICResidualSplit{}, err
	}
	if from == "" {
		return ICResidualSplit{Closing: closing, Historical: 0}, nil // no IC activity
	}
	unrounded := make(map[string]float64)
	err = tk.ByPeriod(from, d, GranMonth, func(pFrom, pTo string) error {
		rows, err := tk.store.PeriodActivity(ctx, pFrom, pTo, s.Sub)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if !isIC[r.AccountID] {
				continue
			}
			rr, err := tk.RateOn(ctx, r.Currency, target, pTo)
			if err != nil {
				return err
			}
			exFrom, exTo, err := tk.exponents(ctx, r.Currency, target)
			if err != nil {
				return err
			}
			unrounded[r.Currency] += float64(r.Amount) * rr.Rate * math.Pow(10, float64(exTo-exFrom))
		}
		return nil
	})
	if err != nil {
		return ICResidualSplit{}, err
	}
	var historical int64
	for _, v := range unrounded {
		historical += RoundHalfEven(v)
	}

	return ICResidualSplit{Closing: closing, Historical: historical}, nil
}

// earliestICYear returns "YYYY-01-01" of the earliest calendar YEAR (from the fixed
// inception bound to d) in which any intercompany-flagged account has activity, or ""
// when there is none. It exists so the historical-rate month decomposition
// (IntercompanyResidualSplit) starts at real intercompany activity rather than the 1900
// inception — a coarse yearly scan (one PeriodActivity per year) finds the active span
// cheaply, then the caller month-decomposes only [that year, d]. No store query is added
// (PeriodActivity carries no min-date); the yearly probe reuses the same read.
func (tk *Toolkit) earliestICYear(ctx context.Context, scope int64, isIC map[AccountID]bool, d string) (string, error) {
	var earliest string
	err := tk.ByPeriod(inceptionDate, d, GranYear, func(pFrom, pTo string) error {
		if earliest != "" {
			return nil // already found the first active year
		}
		rows, err := tk.store.PeriodActivity(ctx, pFrom, pTo, scope)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if isIC[r.AccountID] {
				y, _, err := yearMonth(pFrom)
				if err != nil {
					return err
				}
				earliest = firstOfMonth(y, 1)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return earliest, nil
}

// NetIncome returns the net revenue+expense activity over [from,to] in the scope,
// converted to the target (D12). It is the sum of every revenue/expense account's
// activity (net-debit: a net CREDIT — a surplus — is negative). RateClosing
// converts each native currency subtotal to o.To at the as-of (to) rate, rounding
// each currency's contribution HALF-EVEN once before summing (the final aggregate
// is the per-currency converted subtotal, D12); RateTxnDate converts month-by-month
// and rounds the grand total once; RateNone requires a single currency (else it is
// ambiguous) and returns it raw.
func (tk *Toolkit) NetIncome(ctx context.Context, s Scope, from, to string, o ConvertOpts) (CurAmt, error) {
	act, err := tk.Activity(ctx, s, from, to, ConvertOpts{Mode: RateNone})
	if err != nil {
		return CurAmt{}, err
	}
	reReport, err := tk.reAccounts(ctx)
	if err != nil {
		return CurAmt{}, err
	}
	// Sum native activity per currency over revenue+expense accounts only.
	subtotal := make(map[string]int64)
	for acct, amts := range act {
		if !reReport[acct] {
			continue
		}
		for _, a := range amts {
			subtotal[a.Currency] += a.Minor
		}
	}
	if o.Mode == RateNone {
		// Native only makes sense single-currency; return the sole currency (or zero).
		amts := sortedCurAmts(subtotal)
		if len(amts) == 1 {
			return amts[0], nil
		}
		return CurAmt{}, nil
	}
	// TxnDate: reuse the Activity TxnDate path (per-account unrounded sums) then add.
	if o.Mode == RateTxnDate {
		conv, err := tk.Activity(ctx, s, from, to, o)
		if err != nil {
			return CurAmt{}, err
		}
		var total int64
		for acct, amts := range conv {
			if !reReport[acct] {
				continue
			}
			for _, a := range amts {
				total += a.Minor
			}
		}
		return CurAmt{Currency: o.To, Minor: total}, nil
	}
	// Closing: convert each native currency subtotal once at the as-of rate.
	var total int64
	for ccy, minor := range subtotal {
		conv, err := tk.ConvertMinorAt(ctx, minor, ccy, o.To, to)
		if err != nil {
			return CurAmt{}, err
		}
		total += conv
	}
	return CurAmt{Currency: o.To, Minor: total}, nil
}

// Rollup returns placeholder subtotal rows in TREE ORDER over a leaf (account ->
// minor) map: it walks the account tree pre-order, emitting a Subtotal row for each
// placeholder account whose amount is the sum of its subtree's leaf amounts, and a
// data row for each leaf that carries a value. Amounts are single-currency (the
// caller converts before rolling up). This is the shared spine of the tree reports.
func (tk *Toolkit) Rollup(ctx context.Context, currency string, leaf map[AccountID]int64) ([]TreeRow, error) {
	tree, err := tk.store.Tree(ctx, "en", nil)
	if err != nil {
		return nil, err
	}
	// children[parent] = ordered child ids; roots = accounts with no parent.
	children := make(map[AccountID][]AccountID)
	depth := make(map[AccountID]int)
	name := make(map[AccountID]string)
	var roots []AccountID
	isPlaceholder := make(map[AccountID]bool)
	for _, r := range tree {
		name[r.ID] = r.Name
		if r.ParentID.Valid {
			children[r.ParentID.Int64] = append(children[r.ParentID.Int64], r.ID)
		} else {
			roots = append(roots, r.ID)
		}
	}
	for p := range children {
		isPlaceholder[p] = true
	}
	// Tree is already pre-order; compute depth from the parent chain.
	parentOf := make(map[AccountID]AccountID)
	for _, r := range tree {
		if r.ParentID.Valid {
			parentOf[r.ID] = r.ParentID.Int64
		}
	}
	for _, r := range tree {
		d := 0
		for n := r.ID; ; {
			p, ok := parentOf[n]
			if !ok {
				break
			}
			d++
			n = p
		}
		depth[r.ID] = d
	}

	// subtreeSum[id] = sum of leaf amounts in id's subtree (post-order fold).
	subtreeSum := make(map[AccountID]int64)
	var fold func(id AccountID) int64
	fold = func(id AccountID) int64 {
		if !isPlaceholder[id] {
			s := leaf[id]
			subtreeSum[id] = s
			return s
		}
		var s int64
		for _, c := range children[id] {
			s += fold(c)
		}
		subtreeSum[id] = s
		return s
	}
	for _, r := range roots {
		fold(r)
	}

	// Emit in pre-order (tree order), placeholders as Subtotal rows, leaves as data.
	var rows []TreeRow
	var walk func(id AccountID)
	walk = func(id AccountID) {
		if isPlaceholder[id] {
			rows = append(rows, TreeRow{
				AccountID: id,
				Name:      name[id],
				Amount:    CurAmt{Currency: currency, Minor: subtreeSum[id]},
				Indent:    depth[id],
				Subtotal:  true,
			})
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		// Leaf: emit only if it carries a value (keeps the rollup to the relevant set).
		if v, ok := leaf[id]; ok {
			rows = append(rows, TreeRow{
				AccountID: id,
				Name:      name[id],
				Amount:    CurAmt{Currency: currency, Minor: v},
				Indent:    depth[id],
			})
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return rows, nil
}

// ByPeriod invokes f once per sub-period of [from,to] at granularity g, passing the
// sub-period's inclusive [pFrom,pTo] bounds. GranNone calls f once with the whole
// range; GranMonth/GranQuarter/GranYear split it into calendar months/quarters/
// years; GranWeek splits it into ISO (Monday-start) weeks. It drives the comparative
// report columns (p15.5) and the TxnDate month decomposition.
func (tk *Toolkit) ByPeriod(from, to string, g Granularity, f func(pFrom, pTo string) error) error {
	if g == GranNone {
		return f(from, to)
	}
	if g == GranWeek {
		return tk.byWeek(from, to, f)
	}
	// Month-anchored granularities: the step is the number of calendar months per
	// sub-period (month 1, quarter 3, year 12).
	step := 1
	switch g {
	case GranQuarter:
		step = 3
	case GranYear:
		step = 12
	}
	y, m, err := yearMonth(from)
	if err != nil {
		return err
	}
	for {
		pFrom := firstOfMonth(y, m)
		if pFrom > to {
			break
		}
		ey, em := y, m+step-1
		for em > 12 {
			em -= 12
			ey++
		}
		pTo := lastOfMonth(ey, em)
		if pTo > to {
			pTo = to
		}
		if pFrom < from {
			pFrom = from
		}
		if err := f(pFrom, pTo); err != nil {
			return err
		}
		m += step
		for m > 12 {
			m -= 12
			y++
		}
	}
	return nil
}

// byWeek splits [from,to] into ISO (Monday-start) weeks, invoking f once per week
// with the week's inclusive bounds clamped to [from,to]. The first week starts at
// the Monday on-or-before `from` (clamped up to `from`); the last week ends at `to`.
// The Monday-start definition matches the budget toolkit's GranWeek bucket key.
func (tk *Toolkit) byWeek(from, to string, f func(pFrom, pTo string) error) error {
	fromT, err := time.Parse("2006-01-02", from)
	if err != nil {
		return fmt.Errorf("reports: byWeek parse from %q: %w", from, err)
	}
	toT, err := time.Parse("2006-01-02", to)
	if err != nil {
		return fmt.Errorf("reports: byWeek parse to %q: %w", to, err)
	}
	// Start of the week containing `from`: roll back to Monday.
	back := (int(fromT.Weekday()) + 6) % 7
	weekStart := fromT.AddDate(0, 0, -back)
	for !weekStart.After(toT) {
		pFrom := weekStart
		if pFrom.Before(fromT) {
			pFrom = fromT
		}
		pTo := weekStart.AddDate(0, 0, 6) // Sunday end of this week
		if pTo.After(toT) {
			pTo = toT
		}
		if err := f(pFrom.Format("2006-01-02"), pTo.Format("2006-01-02")); err != nil {
			return err
		}
		weekStart = weekStart.AddDate(0, 0, 7)
	}
	return nil
}

// RateOn returns the on-or-before rate for base->quote at d, plus its actual date
// (staleness) and whether it was reciprocal (p14.1). It surfaces store.ErrRateMissing
// unwrapped-in-chain so a report can footnote a gap.
func (tk *Toolkit) RateOn(ctx context.Context, base, quote, d string) (Rate, error) {
	rr, err := tk.store.RateOn(ctx, base, quote, d)
	if err != nil {
		return Rate{}, err
	}
	return Rate{Rate: rr.Rate, RateDate: rr.RateDate, Reciprocal: rr.Reciprocal}, nil
}

// ConvertMinorAt converts minor units in currency `from` to currency `to` at the
// on-or-before rate at date d, rounding HALF-EVEN once (D12). Exponent-aware via
// money.ConvertMinor. base==quote is identity. Exported so a report (and the tests)
// can convert a single cell with the exact toolkit rule.
func (tk *Toolkit) ConvertMinorAt(ctx context.Context, minor int64, from, to, d string) (int64, error) {
	if from == to {
		return minor, nil
	}
	rr, err := tk.store.RateOn(ctx, from, to, d)
	if err != nil {
		return 0, err
	}
	exFrom, exTo, err := tk.exponents(ctx, from, to)
	if err != nil {
		return 0, err
	}
	return money.ConvertMinor(minor, rr.Rate, exFrom, exTo), nil
}

// --- unexported helpers ----------------------------------------------------

// convertClosing converts a slice of native per-currency amounts to `to` at the
// on-or-before rate at date d and sums them into a single `to` CurAmt. Each
// currency is converted and rounded once (D12 final-aggregate grain), then the
// int64 results are summed.
func (tk *Toolkit) convertClosing(ctx context.Context, amts []CurAmt, to, d string) (CurAmt, error) {
	var total int64
	for _, a := range amts {
		conv, err := tk.ConvertMinorAt(ctx, a.Minor, a.Currency, to, d)
		if err != nil {
			return CurAmt{}, err
		}
		total += conv
	}
	return CurAmt{Currency: to, Minor: total}, nil
}

// exponents returns the minor-unit exponents of two currencies (money.ConvertMinor
// scales between them). Cached per toolkit run via expCache.
func (tk *Toolkit) exponents(ctx context.Context, a, b string) (int, int, error) {
	ea, err := tk.exponent(ctx, a)
	if err != nil {
		return 0, 0, err
	}
	eb, err := tk.exponent(ctx, b)
	if err != nil {
		return 0, 0, err
	}
	return ea, eb, nil
}

func (tk *Toolkit) exponent(ctx context.Context, code string) (int, error) {
	if tk.expCache == nil {
		tk.expCache = make(map[string]int)
	}
	if e, ok := tk.expCache[code]; ok {
		return e, nil
	}
	c, err := tk.store.Currency(ctx, code)
	if err != nil {
		return 0, err
	}
	tk.expCache[code] = int(c.Exponent)
	return int(c.Exponent), nil
}

// codeOrder maps a 990 code -> its report sort position (form990_lines sort,code).
func (tk *Toolkit) codeOrder(ctx context.Context) (map[string]int, error) {
	// Fetch all lines via the type-filtered read with every account type unioned:
	// simpler to read the full set through the store's currency-free path. There is
	// no "all lines" store method, so union the four account types the lines cover.
	order := make(map[string]int)
	pos := 0
	seen := make(map[string]bool)
	for _, t := range []string{"revenue", "expense", "asset", "liability"} {
		lines, err := tk.store.Form990LinesForType(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			if seen[l.Code] {
				continue
			}
			seen[l.Code] = true
			order[l.Code] = pos
			pos++
		}
	}
	return order, nil
}

// consolidatedICExclusions returns the intercompany account ids to DROP from a
// consolidated-scope activity/functional/program result (D19): across a
// consolidated (multi-sub) scope an intra-group revenue/expense transfer is
// internal and must not inflate the group's income statement / 990 (mirroring the
// balance sheet's intercompany collapse). At a leaf/single-sub scope the set is
// EMPTY — there the intercompany account is that entity's real external-facing
// line and stays. The two legs of a transfer are flagged independently, so both
// drop; no cross-currency netting is needed (they are simply excluded).
func (tk *Toolkit) consolidatedICExclusions(ctx context.Context, scope int64) (map[AccountID]bool, error) {
	consolidated, err := tk.isConsolidated(ctx, scope)
	if err != nil {
		return nil, err
	}
	if !consolidated {
		return nil, nil
	}
	ids, err := tk.store.IntercompanyAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[AccountID]bool, len(ids))
	for _, id := range ids {
		set[AccountID(id)] = true
	}
	return set, nil
}

// reAccounts returns the set of revenue/expense account ids (for NetIncome). It
// reads the account tree types once.
func (tk *Toolkit) reAccounts(ctx context.Context) (map[AccountID]bool, error) {
	tree, err := tk.store.Tree(ctx, "en", nil)
	if err != nil {
		return nil, err
	}
	re := make(map[AccountID]bool)
	for _, r := range tree {
		if r.Type == "revenue" || r.Type == "expense" {
			re[r.ID] = true
		}
	}
	return re, nil
}

// sortedCurAmts turns a currency->minor map into a currency-sorted CurAmt slice
// (deterministic output order).
func sortedCurAmts(m map[string]int64) []CurAmt {
	out := make([]CurAmt, 0, len(m))
	for ccy, minor := range m {
		out = append(out, CurAmt{Currency: ccy, Minor: minor})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// --- ISO date arithmetic (internal compute; not a user-render path, rule 10) ---
// These operate on YYYY-MM-DD strings — the same convention the store's balance
// queries and RateOn use. time.Parse with the ISO layout is the established
// in-code pattern (store.transactions validates dates the same way).

// yearMonth parses a YYYY-MM-DD date into its year and month.
func yearMonth(d string) (int, int, error) {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return 0, 0, fmt.Errorf("reports: parse date %q: %w", d, err)
	}
	return t.Year(), int(t.Month()), nil
}

// firstOfMonth returns YYYY-MM-01 for the given year/month.
func firstOfMonth(y, m int) string {
	return fmt.Sprintf("%04d-%02d-01", y, m)
}

// lastOfMonth returns the last calendar day of year/month as YYYY-MM-DD (via the
// "day 0 of next month" trick, which time normalizes to the last day of this one).
func lastOfMonth(y, m int) string {
	t := time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC)
	return t.Format("2006-01-02")
}

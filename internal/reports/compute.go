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
	if o.Mode != RateTxnDate {
		rows, err := tk.store.PeriodActivity(ctx, from, to, s.Sub)
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
	err := tk.ByPeriod(from, to, GranMonth, func(pFrom, pTo string) error {
		rows, err := tk.store.PeriodActivity(ctx, pFrom, pTo, s.Sub)
		if err != nil {
			return err
		}
		for _, r := range rows {
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

// FunctionalMatrix returns, per (expense account, class), the per-currency activity
// over [from,to] in the scope (D21). Only expense splits carry a class, so the
// result is exactly the functional expense matrix cells. Conversion (RateClosing)
// collapses each cell's currencies to o.To at the as-of (to) rate; RateNone leaves
// them native.
func (tk *Toolkit) FunctionalMatrix(ctx context.Context, s Scope, from, to string, o ConvertOpts) (map[AccountID]map[Class][]CurAmt, error) {
	rows, err := tk.store.FunctionalActivity(ctx, from, to, s.Sub)
	if err != nil {
		return nil, err
	}
	native := make(map[AccountID]map[Class][]CurAmt)
	for _, r := range rows {
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
// range; GranMonth/GranQuarter split it into calendar months/quarters. It drives
// the comparative report columns (p15.5) and the TxnDate month decomposition.
func (tk *Toolkit) ByPeriod(from, to string, g Granularity, f func(pFrom, pTo string) error) error {
	if g == GranNone {
		return f(from, to)
	}
	step := 1
	if g == GranQuarter {
		step = 3
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

package reports

import (
	"context"
	"sort"
)

// Form990ReportID is the id (URL slug + registry key) of the 990 PACKAGE report
// (p15.11): the year-end IRS Form 990 filing package, one labeled SECTION per Part in
// a single report Table. It COMPOSES the four sibling reports' toolkit paths so its
// numbers reconcile to them exactly (the cross-checks are the point):
//
//   - Part III  — Program service summary: revenue + expense per PROGRAM (the same
//     program set p15.10's comparative view emits — General, then its children — driven
//     by the identical ProgramActivity(RateNone) call, so each program group equals
//     p15.10's column). Native, per-currency (a program is a mission dimension read in
//     the money it occurred in — matching p15.10, which converting would blur). No
//     cross-program grand total: General is a rollup that already folds in its children,
//     so summing the groups would double-count (like p15.10's un-summed columns).
//   - Part VIII — Revenue by effective line: the REVENUE accounts' activity converted at
//     the TXN-DATE rate (matching p15.5, the income statement's revenue flow) and rolled
//     to their effective Part VIII 990 codes via Group990 (D25 inheritance/override,
//     explicit Unmapped bucket). The line total == p15.5's total revenue.
//   - Part IX  — Functional-expenses totals: the p15.7 FunctionalMatrix(RateTxnDate) path
//     rolled to effective Part IX lines; each line total (Σ the three functional classes)
//     equals p15.7's line total exactly, and the Part IX grand total ties Part VIII's
//     basis — revenue and expenses both at the txn-date flow rate (p26.71), so Part IX ==
//     the income statement's total expenses.
//   - Part X   — Balance-sheet lines at year-end: the p15.4 balance-sheet path
//     (BalancesAsOf at fiscal-year-end + the by-restriction net-asset split via fund
//     tagging + the intercompany elimination on a consolidated scope), converted at the
//     year-end CLOSING rate. Net assets with/without split == p15.4's; A == L + NA.
//
// EVERY section renders an explicit UNMAPPED bucket (accounts with no effective 990 code
// for that Part appear on an Unmapped line, never dropped) even when empty on the fixture
// — the mechanism is the whole point (Z19). On this fixture Part VIII's Unmapped bucket
// is non-empty (Event Income, no effective code); Part IX / Part III are structurally
// empty (every expense inherits IX.24e; every R/E split carries a program, D24); Part X's
// Unmapped assets/liabilities bucket is empty (every A/L account is mapped or, being
// listed as a natural account, needs no 990-line rollup here — see below).
//
// FISCAL YEAR (Params.Period From/To): the "fiscal year" is expressed as the period
// param (From..To), the org's fiscal year resolved to a from–to. Part X's as-of date is
// the fiscal-year-END (To): a 990 balance sheet is the position at year-end, and using
// To makes Part X reconcile to p15.4 run as-of the same date. The report converts to the
// scope's base (USD) — a 990 is a single-currency form — for Parts VIII/IX/X; Part III is
// native (per p15.10). Documented in DECISIONS.
//
// COLUMNS: one shared 3-column shape [Line/Account, Currency, Amount] serves every Part
// (a converted Part emits one USD row per line; native Part III emits per-currency rows),
// so a single Table renders all four Parts and every row is single-currency — hence every
// amount line DRILLS cleanly to its accounts' splits (DrillPeriod for III/VIII/IX,
// DrillAsOf(To) for X), reusing the p15.3d drill patterns.
const Form990ReportID = "form_990"

// registerForm990 registers the 990 package report (p15.11) into reg under the "tax"
// (IRS-990) group. It offers the period (From/To = the fiscal year) and the target-
// currency control (defaulting to the scope base, USD); Part X's as-of is derived from
// the period end internally (no separate as-of control).
func registerForm990(reg *Registry) {
	reg.Register(Report{
		ID:                 Form990ReportID,
		TitleKey:           "reports.form_990.title",
		Group:              "tax",
		ParamsSpec:         ParamsSpec{Period: true, Currency: true},
		Run:                runForm990,
		ProgramDimensioned: true, // p27.4: R/E activity carries a program (grant-subtree filterable).
	})
}

// runForm990 computes the 990-package Table (p15.11): the four Parts as labeled sections
// in one Table, each built from its sibling report's toolkit path so the numbers
// reconcile, each with an explicit Unmapped bucket.
func runForm990(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	b := &f990Builder{tk: tk, p: p, target: p.TargetCurrency}
	b.columns()

	if err := b.partIII(ctx); err != nil {
		return Table{}, err
	}
	if err := b.partVIII(ctx); err != nil {
		return Table{}, err
	}
	if err := b.partIX(ctx); err != nil {
		return Table{}, err
	}
	// p27.4: Parts III/VIII/IX are R/E activity (program-dimensioned) and are filtered to
	// the granted subtree by the toolkit (ProgramActivity / Activity / FunctionalMatrix).
	// Part X is the BALANCE SHEET (assets/liabilities/net-assets) -- NO split carries a
	// program (D24), so it CANNOT be program-filtered. Under a program-scoped grant we do
	// not COMPUTE it at all (rather than compute org-wide balances and hide the rows): the
	// "non-program content isn't shown to a purely program-scoped user" rule. Empty
	// ProgramScope (admin / unscoped grant) renders Part X unchanged, so the goldens do
	// not move.
	if len(p.ProgramScope) == 0 {
		if err := b.partX(ctx); err != nil {
			return Table{}, err
		}
	}
	return b.table(), nil
}

// f990Builder accumulates the package rows over the shared 3-column shape.
type f990Builder struct {
	tk     *Toolkit
	p      Params
	target string
	rows   []Row
}

// columns is the shared shape every Part renders into: a line/account label, the row's
// currency, and the amount. (No struct field — the shape is fixed; table() emits it.)
func (b *f990Builder) columns() {}

func (b *f990Builder) table() Table {
	return Table{
		Columns: []Column{
			{HeaderKey: "reports.form_990.col.line", Align: AlignLeft},
			{HeaderKey: "reports.form_990.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.form_990.col.amount", Align: AlignRight},
		},
		Rows: b.rows,
	}
}

// --- shared row helpers -----------------------------------------------------

// sectionRow appends a Part section-header row (a label + blank currency/amount cells).
func (b *f990Builder) sectionRow(key string) {
	b.rows = append(b.rows, Row{
		Cells: []Cell{LabelCell(key), TextCell(""), BlankMoneyCell()},
		Kind:  RowData,
	})
}

// lineRowText appends a data line whose first cell is stored TEXT (a 990 line label or
// a program/account proper noun), with the row's currency + amount and an optional drill.
func (b *f990Builder) lineRowText(text, ccy string, minor int64, d *Drill, indent int) {
	amt := MoneyCell(minor, ccy)
	if d != nil {
		amt = amt.WithDrill(d)
	}
	b.rows = append(b.rows, Row{
		Cells:  []Cell{TextCell(text), TextCell(ccy), amt},
		Indent: indent,
		Kind:   RowData,
	})
}

// unmappedRow appends the explicit Unmapped bucket line (a localized "(Unmapped)" LABEL
// + currency + amount). Rendered even when empty (minor 0, no currency) so the mechanism
// is always present (Z19 — never drop rows).
func (b *f990Builder) unmappedRow(ccy string, minor int64, d *Drill) {
	amt := MoneyCell(minor, ccy)
	if d != nil {
		amt = amt.WithDrill(d)
	}
	b.rows = append(b.rows, Row{
		Cells:  []Cell{LabelCell("reports.form_990.unmapped"), TextCell(ccy), amt},
		Indent: 1,
		Kind:   RowData,
	})
}

// subtotalRow appends an emphasized subtotal/total row (a localized LABEL + currency +
// amount). Not drillable (a rollup over many accounts/currencies).
func (b *f990Builder) subtotalRow(key, ccy string, minor int64, kind RowKind) {
	b.rows = append(b.rows, Row{
		Cells:  []Cell{LabelCell(key), TextCell(ccy), MoneyCell(minor, ccy)},
		Indent: 1,
		Kind:   kind,
	})
}

// periodLineDrill builds the DrillPeriod filter for a Part VIII/IX effective-line amount
// over the fiscal year, gated on a SINGLE native currency (the p15.7 rule): a line whose
// accounts span more than one native currency has a converted figure summed across
// currencies, which no single currency-filtered drill reconciles — so it is left non-
// drillable. accts is the line's contributing account ids; ccys is the set of native
// currencies those accounts posted in. The drill carries the accounts + the ONE native
// currency; the drilled native splits' signed sum equals the pre-conversion native figure.
func (b *f990Builder) periodLineDrill(accts []int64, ccys map[string]bool) *Drill {
	if len(accts) == 0 || len(ccys) != 1 {
		return nil
	}
	var ccy string
	for c := range ccys {
		ccy = c
	}
	return &Drill{
		Scope:      int64(b.p.Scope),
		AccountIDs: dedupSortInts(accts),
		Currency:   ccy,
		Mode:       DrillPeriod,
		From:       b.p.From,
		To:         b.p.To,
	}
}

// --- Part III: program service summary (reuses p15.10 ProgramActivity) ------

// partIII renders, per program (the same set p15.10's comparative view emits — General
// then its descendants, tree pre-order), a Revenue then an Expense line per currency,
// native (per p15.10). Each program is a rolled-up group; General folds in its children,
// so the groups are NOT summed into a cross-program total (that would double-count).
// The Unmapped bucket is structurally empty (D24: every R/E split carries a program) but
// rendered anyway.
func (b *f990Builder) partIII(ctx context.Context) error {
	b.sectionRow("reports.form_990.part.iii")

	// Same native rollup p15.10 reads: a parent program's cells fold in its descendants.
	act, err := b.tk.ProgramActivity(ctx, Scope{Sub: b.p.Scope}, b.p.From, b.p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return err
	}
	// The program set + descendant sets, exactly p15.10's comparative columns.
	cols, err := programColumns(ctx, b.tk, Params{Scope: b.p.Scope}) // Program==0 => comparative
	if err != nil {
		return err
	}
	types, err := b.tk.accountTypes(ctx)
	if err != nil {
		return err
	}

	for _, c := range cols {
		b.lineRowText(c.name, "", 0, nil, 1) // program group header (proper-noun label)
		// Revenue then Expense, per currency, native. Sum this program's accounts of the
		// type per currency. Revenue is net-debit NEGATIVE (a credit) shown +inflow (−1);
		// expense net-debit POSITIVE shown as-is (+1).
		b.programTypeLines(act[c.id], types, c, "revenue", "reports.form_990.iii.revenue", -1)
		b.programTypeLines(act[c.id], types, c, "expense", "reports.form_990.iii.expenses", +1)
	}

	// Unmapped bucket: a program with an activity account of NO recognized R/E type — none
	// can exist (D24), so this is structurally empty. Rendered anyway (the mechanism).
	b.unmappedRow("", 0, nil)
	return nil
}

// programTypeLines emits one subtotal line per currency for a program's accounts of the
// given type, signed for display, each drillable across the program's subtree (the p15.10
// rollup-cell drill: ProgramIDs for a program WITH descendants, ProgramID for a leaf).
func (b *f990Builder) programTypeLines(
	byAcct map[AccountID][]CurAmt, types map[AccountID]string, c progCol,
	typ, labelKey string, sign int64,
) {
	byCcy := map[string]int64{}
	acctsByCcy := map[string][]int64{}
	for acct, amts := range byAcct {
		if types[acct] != typ {
			continue
		}
		for _, a := range amts {
			byCcy[a.Currency] += a.Minor
			acctsByCcy[a.Currency] = append(acctsByCcy[a.Currency], int64(acct))
		}
	}
	for _, ccy := range sortedKeys(byCcy) {
		accts := dedupSortInts(acctsByCcy[ccy])
		d := &Drill{
			Scope:      int64(b.p.Scope),
			AccountIDs: accts,
			Currency:   ccy,
			Mode:       DrillPeriod,
			From:       b.p.From,
			To:         b.p.To,
		}
		if len(c.descendants) > 1 {
			d.ProgramIDs = c.descendants
		} else {
			id := int64(c.id)
			d.ProgramID = &id
		}
		b.rows = append(b.rows, Row{
			Cells:  []Cell{LabelCell(labelKey), TextCell(ccy), MoneyCell(sign*byCcy[ccy], ccy).WithDrill(d)},
			Indent: 2,
			Kind:   RowSubtotal,
		})
	}
}

// --- Part VIII: revenue by effective line (reuses p15.5 flow + Group990) ----

// partVIII renders the REVENUE accounts' activity converted at the TXN-DATE rate (p15.5's
// income-statement revenue flow) rolled to their effective Part VIII 990 codes (D25) via
// Group990, one line per effective code in the part's report order with the Unmapped
// bucket last. Amounts are displayed as positive inflows (revenue is net-debit negative).
// The line total == p15.5's total revenue.
func (b *f990Builder) partVIII(ctx context.Context) error {
	b.sectionRow("reports.form_990.part.viii")
	target := b.target

	// Per-account revenue activity converted to the target at the txn-date rate — the
	// SAME grain p15.5 sums (convert per account BEFORE rolling up, since Group990 does no
	// conversion). One converted USD CurAmt per revenue account.
	act, err := b.tk.Activity(ctx, Scope{Sub: b.p.Scope}, b.p.From, b.p.To, ConvertOpts{To: target, Mode: RateTxnDate})
	if err != nil {
		return err
	}
	// Native per-account activity (RateNone) — for the drill's NATIVE currency and to
	// detect single- vs multi-currency lines (only a single-native-currency line drills,
	// the p15.7 rule: one currency-filtered link cannot reconcile a summed-across-
	// currencies converted figure like VIII.1e's USD+MXN).
	native, err := b.tk.Activity(ctx, Scope{Sub: b.p.Scope}, b.p.From, b.p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return err
	}
	types, err := b.tk.accountTypes(ctx)
	if err != nil {
		return err
	}
	// Revenue leaf map (net-debit minor, target currency) for Group990. type=="revenue"
	// only (NOT reAccounts, which is R+E).
	leaf := map[AccountID]int64{}
	acctsByCode := map[string][]int64{}
	ccysByCode := map[string]map[string]bool{}
	eff, err := b.tk.EffectiveCodes(ctx)
	if err != nil {
		return err
	}
	for acct, amts := range act {
		if types[acct] != "revenue" {
			continue
		}
		for _, a := range amts { // exactly one target CurAmt per account
			leaf[acct] += a.Minor
		}
		code := eff[acct]
		acctsByCode[code] = append(acctsByCode[code], int64(acct))
		if ccysByCode[code] == nil {
			ccysByCode[code] = map[string]bool{}
		}
		for _, na := range native[acct] {
			ccysByCode[code][na.Currency] = true
		}
	}

	rows, err := b.tk.Group990(ctx, "VIII", target, leaf)
	if err != nil {
		return err
	}
	lines, err := b.tk.Part990Lines(ctx, "VIII", "revenue")
	if err != nil {
		return err
	}
	labelOf := map[string]string{}
	for _, pl := range lines {
		labelOf[pl.Code] = lineLabel(pl)
	}

	var total int64
	seenUnmapped := false
	for _, lr := range rows {
		// Display revenue as a positive inflow (net-debit negative -> negate).
		amt := -lr.Amount.Minor
		total += amt
		d := b.periodLineDrill(acctsByCode[lr.Code], ccysByCode[lr.Code])
		if lr.Unmapped {
			seenUnmapped = true
			b.unmappedRow(target, amt, d)
		} else {
			b.lineRowText(labelOf[lr.Code], target, amt, d, 1)
		}
	}
	if !seenUnmapped {
		b.unmappedRow(target, 0, nil) // always present, even when empty
	}
	b.subtotalRow("reports.form_990.viii.total", target, total, RowTotal)
	return nil
}

// --- Part IX: functional-expenses totals (reuses p15.7 FunctionalMatrix) ----

// partIX renders the p15.7 functional-expenses path: each expense (account,class) cell
// converted at the TRANSACTION-DATE rate (p26.71 — an expense flow, matching the income
// statement), grouped to its effective Part IX line (D25), and the LINE TOTAL (Σ the
// three functional classes) emitted per effective code — so each line
// total equals p15.7's line total exactly. Accounts with no effective code fall into the
// Unmapped bucket (empty on this fixture: every expense inherits IX.24e). One converted
// (USD) figure per line + a grand total.
func (b *f990Builder) partIX(ctx context.Context) error {
	b.sectionRow("reports.form_990.part.ix")
	target := b.target

	conv, err := b.tk.FunctionalMatrix(ctx, Scope{Sub: b.p.Scope}, b.p.From, b.p.To, ConvertOpts{To: target, Mode: RateTxnDate})
	if err != nil {
		return err
	}
	// Native per-(account,class) matrix (RateNone) — for the drill's NATIVE currency and
	// single- vs multi-currency detection (only a single-native-currency line drills; the
	// p15.7 rule — IX.24e spans USD+MXN, so its converted total is not drillable).
	native, err := b.tk.FunctionalMatrix(ctx, Scope{Sub: b.p.Scope}, b.p.From, b.p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return err
	}
	eff, err := b.tk.EffectiveCodes(ctx)
	if err != nil {
		return err
	}
	lines, err := b.tk.Part990Lines(ctx, "IX", "expense")
	if err != nil {
		return err
	}

	// Per effective code: the line total (Σ classes, all converted to target) + the
	// contributing accounts + their native currency set (for the drill). "" == Unmapped.
	byCode := map[string]int64{}
	acctsByCode := map[string][]int64{}
	ccysByCode := map[string]map[string]bool{}
	for acct, byClass := range conv {
		code := eff[acct]
		for _, amts := range byClass {
			byCode[code] += classMinor(amts, target)
		}
		acctsByCode[code] = append(acctsByCode[code], int64(acct))
		if ccysByCode[code] == nil {
			ccysByCode[code] = map[string]bool{}
		}
		for _, amts := range native[acct] {
			for _, na := range amts {
				ccysByCode[code][na.Currency] = true
			}
		}
	}

	var grand int64
	emit := func(code, label string, unmapped bool) {
		total, ok := byCode[code]
		if !ok {
			return
		}
		grand += total
		d := b.periodLineDrill(acctsByCode[code], ccysByCode[code])
		if unmapped {
			b.unmappedRow(target, total, d)
		} else {
			b.lineRowText(label, target, total, d, 1)
		}
	}
	for _, pl := range lines {
		emit(pl.Code, lineLabel(pl), false)
	}
	if _, ok := byCode[""]; ok {
		emit("", "", true)
	} else {
		b.unmappedRow(target, 0, nil) // always present, even when empty
	}
	b.subtotalRow("reports.form_990.ix.total", target, grand, RowTotal)
	return nil
}

// --- Part X: balance sheet at year-end (reuses p15.4 balance-sheet path) -----

// partX renders the balance-sheet lines at fiscal-year-END (To), converted at the closing
// rate, reusing the p15.4 path: the asset/liability accounts (intercompany eliminated on a
// consolidated scope, D19), the by-restriction net-asset split (with/without donor
// restrictions from fund tagging + the section plug), and the A = L + NA identity. Each
// A/L account line drills (DrillAsOf(To)); the synthetic net-asset lines do not. The
// Unmapped assets/liabilities bucket is rendered (empty on this fixture).
func (b *f990Builder) partX(ctx context.Context) error {
	b.sectionRow("reports.form_990.part.x")
	target := b.target
	asOf := b.p.To

	balances, err := b.tk.BalancesAsOf(ctx, Scope{Sub: b.p.Scope}, asOf, ConvertOpts{Mode: RateNone})
	if err != nil {
		return err
	}
	tree, err := b.tk.Store().Tree(ctx, b.p.LangOr(), nil)
	if err != nil {
		return err
	}

	// Intercompany elimination on a CONSOLIDATED scope (D19), exactly as p15.4 does.
	consolidated, err := b.tk.isConsolidated(ctx, b.p.Scope)
	if err != nil {
		return err
	}
	icAccts := map[int64]bool{}
	if consolidated {
		ids, err := b.tk.store.IntercompanyAccountIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			icAccts[id] = true
		}
	}

	// Classify accounts into Assets / Liabilities; net-debit signs (assets positive,
	// liabilities stored credit -> negate to a positive balance). Equity/R/E roll into the
	// net-asset plug. Convert each (account,currency) at the closing rate to the target.
	var assets, liabilities []bsLine
	for _, node := range tree {
		amts, ok := balances[AccountID(node.ID)]
		if !ok || icAccts[node.ID] {
			continue
		}
		switch node.Type {
		case "asset":
			l := bsLine{name: node.Name, acctID: node.ID}
			for _, a := range amts {
				l.add(a.Currency, a.Minor)
			}
			assets = append(assets, l)
		case "liability":
			l := bsLine{name: node.Name, acctID: node.ID}
			for _, a := range amts {
				l.add(a.Currency, -a.Minor)
			}
			liabilities = append(liabilities, l)
		}
	}
	assetTotal := sumLines(assets)
	liabilityTotal := sumLines(liabilities)

	// Net assets: total = plug (A − L) per currency; with = restricted funds' asset-side
	// balances; without = total − with. Exactly p15.4's split.
	netAssetsTotal := map[string]int64{}
	for ccy, v := range assetTotal {
		netAssetsTotal[ccy] += v
	}
	for ccy, v := range liabilityTotal {
		netAssetsTotal[ccy] -= v
	}
	withRestriction, err := b.tk.restrictedNetAssets(ctx, b.p.Scope, asOf)
	if err != nil {
		return err
	}
	withoutRestriction := map[string]int64{}
	for ccy, v := range netAssetsTotal {
		withoutRestriction[ccy] = v - withRestriction[ccy]
	}
	for ccy, v := range withRestriction {
		if _, ok := netAssetsTotal[ccy]; !ok {
			withoutRestriction[ccy] = -v
		}
	}

	// --- Assets.
	for _, l := range assets {
		b.balanceAccountLine(ctx, l, asOf, target)
	}
	b.convertedSubtotal(ctx, "reports.form_990.x.total_assets", assetTotal, asOf, target, RowSubtotal)

	// --- Liabilities.
	for _, l := range liabilities {
		b.balanceAccountLine(ctx, l, asOf, target)
	}
	b.convertedSubtotal(ctx, "reports.form_990.x.total_liabilities", liabilityTotal, asOf, target, RowSubtotal)

	// --- Explicit Unmapped balance-sheet bucket (A/L accounts with NO effective 990
	// code): ONE bucket for the whole Part (empty on this fixture), rendered anyway so no
	// row is ever dropped.
	b.unmappedBalanceBucket(ctx, append(append([]bsLine{}, assets...), liabilities...))

	// --- Net assets (by-restriction split; synthetic, not drillable).
	b.convertedSubtotal(ctx, "reports.form_990.x.na_without", withoutRestriction, asOf, target, RowData)
	b.convertedSubtotal(ctx, "reports.form_990.x.na_with", withRestriction, asOf, target, RowData)
	b.convertedSubtotal(ctx, "reports.form_990.x.total_net_assets", netAssetsTotal, asOf, target, RowSubtotal)

	// --- Total liabilities + net assets (the identity's RHS; == total assets balanced).
	lPlusNA := map[string]int64{}
	for ccy, v := range liabilityTotal {
		lPlusNA[ccy] += v
	}
	for ccy, v := range netAssetsTotal {
		lPlusNA[ccy] += v
	}
	b.convertedSubtotal(ctx, "reports.form_990.x.total_liabilities_net_assets", lPlusNA, asOf, target, RowTotal)
	return nil
}

// balanceAccountLine emits one asset/liability account line converted to the target at the
// closing rate on asOf. A single-native-currency account drills (DrillAsOf); a multi-
// currency account's converted figure sums across currencies and is left non-drillable
// (matching p15.4's converted-only account cell).
func (b *f990Builder) balanceAccountLine(ctx context.Context, l bsLine, asOf, target string) {
	var conv int64
	for _, ccy := range sortedKeys(l.byCcy) {
		c := l.byCcy[ccy]
		if target != "" {
			cc, err := b.tk.ConvertMinorAt(ctx, l.byCcy[ccy], ccy, target, asOf)
			if err == nil {
				c = cc
			}
		}
		conv += c
	}
	var d *Drill
	if l.acctID != 0 && len(l.byCcy) == 1 {
		for ccy := range l.byCcy {
			d = &Drill{Scope: int64(b.p.Scope), AccountIDs: []int64{l.acctID}, Currency: ccy, Mode: DrillAsOf, AsOf: asOf}
		}
	}
	b.lineRowText(l.name, target, conv, d, 2)
}

// unmappedBalanceBucket renders Part X's explicit Unmapped assets/liabilities bucket: the
// A/L accounts with NO effective 990 code. Part X is a BY-ACCOUNT section — every A/L
// account is already listed by name and summed into the totals above — so this bucket is
// a non-double-counting MEMO (it must NOT re-add those balances to any total; the accounts
// are not dropped, they are shown by name). It exists to make the "never drop rows" rule
// explicit and uniform across all four Parts. On this fixture the fixture's A/L accounts
// carry no 990 code but span USD+MXN, so the bucket renders 0 (a multi-currency memo has
// no single-currency drill and is not summed); a single-native-currency unmapped A/L set
// would render its (drillable) converted memo — still not added to the section totals.
func (b *f990Builder) unmappedBalanceBucket(ctx context.Context, lines []bsLine) {
	eff, err := b.tk.EffectiveCodes(ctx)
	if err != nil {
		b.unmappedRow(b.target, 0, nil)
		return
	}
	var total int64
	var accts []int64
	single := true
	var ccy string
	for _, l := range lines {
		if eff[AccountID(l.acctID)] != "" {
			continue
		}
		accts = append(accts, l.acctID)
		for c, v := range l.byCcy {
			total += v
			if ccy == "" {
				ccy = c
			} else if ccy != c {
				single = false
			}
		}
	}
	if len(accts) == 0 || !single {
		b.unmappedRow(b.target, 0, nil)
		return
	}
	// Non-empty single-currency unmapped bucket: convert + drill (defensive; empty here).
	conv := total
	if b.target != "" && ccy != "" {
		if cc, err := b.tk.ConvertMinorAt(ctx, total, ccy, b.target, b.p.To); err == nil {
			conv = cc
		}
	}
	d := &Drill{Scope: int64(b.p.Scope), AccountIDs: dedupSortInts(accts), Currency: ccy, Mode: DrillAsOf, AsOf: b.p.To}
	b.unmappedRow(b.target, conv, d)
}

// convertedSubtotal emits a per-currency-summed line converted to the target at the
// closing rate on asOf (one target row), for a section total / net-asset line.
func (b *f990Builder) convertedSubtotal(
	ctx context.Context, key string, byCcy map[string]int64, asOf, target string, kind RowKind,
) {
	var conv int64
	for _, ccy := range sortedKeys(byCcy) {
		c := byCcy[ccy]
		if target != "" {
			if cc, err := b.tk.ConvertMinorAt(ctx, byCcy[ccy], ccy, target, asOf); err == nil {
				c = cc
			}
		}
		conv += c
	}
	b.subtotalRow(key, target, conv, kind)
}

// --- toolkit helper (990 package) ------------------------------------------

// accountTypes returns accountID -> account type (asset/liability/equity/revenue/expense)
// for every account, so the 990 package classifies revenue vs expense accounts (Part VIII
// vs the program R/E split) without a per-account store call. One tree read per run.
func (tk *Toolkit) accountTypes(ctx context.Context) (map[AccountID]string, error) {
	tree, err := tk.store.Tree(ctx, "en", nil)
	if err != nil {
		return nil, err
	}
	m := make(map[AccountID]string, len(tree))
	for _, r := range tree {
		m[AccountID(r.ID)] = r.Type
	}
	return m, nil
}

// dedupSortInts returns a sorted, de-duplicated copy of ids (a drill's account set is
// built by appending per-currency, so a multi-currency account can appear twice).
func dedupSortInts(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

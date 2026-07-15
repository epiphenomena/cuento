package reports

import (
	"context"
	"sort"
)

// BalanceSheetReportID is the id (URL slug + registry key) of the balance-sheet
// report (p15.4): the statement of financial position at an as-of date. It presents
// three sections -- ASSETS, LIABILITIES, and NET ASSETS (nonprofit equity) -- such
// that the balance-sheet identity holds: Assets = Liabilities + Net Assets.
//
// The NET-ASSETS section is the nonprofit classification (Q3, D20): net assets are
// split by DONOR RESTRICTION into "without donor restrictions" and "with donor
// restrictions", NOT by equity source. "With donor restrictions" is the sum of the
// RESTRICTED funds' unexpended (asset-side) balances via fund tagging; the TOTAL net
// assets is the balancing plug (Assets - Liabilities) taken from the already-
// collapsed A/L sections; "without donor restrictions" is total - with. This keeps
// the identity STRUCTURAL (it cannot drift with rounding or intercompany collapse)
// and is why the equity-source accounts (Opening Balances) are NOT emitted as rows:
// they are absorbed into the plug (emitting them would double-count against the
// synthetic net-asset lines and break the identity). "Net surplus to date" -- the
// accumulated revenue-minus-expense from inception to the as-of date -- is disclosed
// as an "of which" component of the without-restriction figure, NOT summed into the
// total (it is one source of the without-restriction balance, not a peer of it).
//
// INTERCOMPANY (D19): across a CONSOLIDATED (multi-sub) scope the intercompany
// due-to/due-from accounts are INTERNAL and are ELIMINATED -- dropped from the
// Assets/Liabilities listings and their totals. On the balanced fixture they net to
// zero, so the eliminated asset and liability cancel and Net Assets (the plug) is
// unchanged; a NONZERO residual is surfaced as a WARNING row (never hidden). A
// LEAF/single-sub scope is NOT a consolidation: its intercompany accounts are that
// subsidiary's genuine due-to-parent / due-from-child balances, shown as ordinary
// account rows, never collapsed and never warned.
//
// PER-CURRENCY DETAIL (Params.Detail == "currency"): the default view shows only the
// converted total per section line (target currency at the AsOf closing rate, D12);
// the detail toggle expands each line into one row per native currency (native +
// converted columns) so a reviewer sees the underlying currencies before conversion.
//
// DRILL-DOWN (p15.3d): each asset/liability ACCOUNT balance cell is drillable to the
// transactions behind it (native, per-currency, as-of) -- the trial-balance retrofit
// pattern. The synthetic net-asset lines span many funds/accounts and are not
// drillable (they do not map to a single DrillFilter).
const BalanceSheetReportID = "balance_sheet"

// registerBalanceSheet registers the balance-sheet report (p15.4) into reg under the
// "financial" group. It offers the as-of, target-currency, and per-currency detail
// controls.
func registerBalanceSheet(reg *Registry) {
	reg.Register(Report{
		ID:         BalanceSheetReportID,
		TitleKey:   "reports.balance_sheet.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{AsOf: true, Currency: true, Detail: true},
		Run:        runBalanceSheet,
		Tree:       true, // p26.26: A/L/net-asset sections nest their account lines.
	})
}

// bsLine is one section line accumulated during the walk: a display name, the owning
// account id (for the drill; 0 for a synthetic net-asset line), and per-currency
// native minor amounts. The net-debit sign is normalized per section so every
// displayed figure is POSITIVE the way a balance sheet reads (assets positive,
// liabilities and net assets shown as positive balances).
type bsLine struct {
	name   string
	acctID int64
	byCcy  map[string]int64
}

func (l *bsLine) add(ccy string, minor int64) {
	if l.byCcy == nil {
		l.byCcy = map[string]int64{}
	}
	l.byCcy[ccy] += minor
}

// runBalanceSheet computes the balance-sheet Table (p15.4). It reads the scope's
// per-account per-currency as-of balances (native), classifies each account by TYPE
// into the Assets/Liabilities sections (equity/revenue/expense accounts are NOT
// listed -- they roll into the net-asset plug), builds the by-restriction net-asset
// split from fund tagging + the section plug, and emits the sections with converted
// totals (and, under the detail toggle, per-currency lines).
func runBalanceSheet(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	balances, err := tk.BalancesAsOf(ctx, Scope{Sub: p.Scope}, p.AsOf, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}
	tree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}

	target := p.TargetCurrency

	// --- Intercompany COLLAPSE (D19) applies only across a CONSOLIDATED (multi-sub)
	// scope: there the intercompany due-to/due-from balances are INTERNAL and are
	// eliminated (dropped from the Assets/Liabilities listings and their totals), and
	// the residual (which is zero when the scope covers both sides) is surfaced as a
	// warning row when nonzero. At a LEAF/single-sub scope the intercompany accounts
	// are that subsidiary's genuine due-to-parent / due-from-child balances -- shown as
	// ordinary account rows, NOT collapsed and NOT warned (a leaf legitimately holds
	// only its own side).
	consolidated, err := tk.isConsolidated(ctx, p.Scope)
	if err != nil {
		return Table{}, err
	}
	icAccts := map[int64]bool{}
	if consolidated {
		ids, err := tk.store.IntercompanyAccountIDs(ctx)
		if err != nil {
			return Table{}, err
		}
		for _, id := range ids {
			icAccts[id] = true
		}
	}

	// --- classify accounts into the Assets and Liabilities sections. Walk the tree
	// pre-order (stable order + resolved names). Net-debit signs (D2): assets are
	// positive as stored; liabilities are stored NEGATIVE (credit), so negate to show
	// a positive liability balance. Equity/revenue/expense accounts are skipped --
	// they are absorbed into the net-asset plug below. Intercompany-flagged accounts
	// are ELIMINATED at a consolidated scope (icAccts is empty at a leaf scope).
	var assets, liabilities []bsLine
	for _, node := range tree {
		amts, ok := balances[node.ID]
		if !ok {
			continue
		}
		if icAccts[node.ID] {
			continue // eliminated on consolidation (D19)
		}
		switch node.Type {
		case "asset":
			line := bsLine{name: node.Name, acctID: node.ID}
			for _, a := range amts {
				line.add(a.Currency, a.Minor)
			}
			assets = append(assets, line)
		case "liability":
			line := bsLine{name: node.Name, acctID: node.ID}
			for _, a := range amts {
				line.add(a.Currency, -a.Minor) // stored credit -> positive liability
			}
			liabilities = append(liabilities, line)
		}
	}

	// --- section totals per native currency (positive as displayed).
	assetTotal := sumLines(assets)
	liabilityTotal := sumLines(liabilities)

	// --- Net assets. total NA = plug (Assets - Liabilities) per currency; this keeps
	// the identity exact and consistent with the collapsed sections (intercompany
	// due-to/due-from already net inside those sections). "with donor restrictions" =
	// the restricted funds' asset-side balances (fund tagging); "without" = total -
	// with. Net surplus to date is a disclosure component of "without".
	netAssetsTotal := map[string]int64{}
	for ccy, v := range assetTotal {
		netAssetsTotal[ccy] += v
	}
	for ccy, v := range liabilityTotal {
		netAssetsTotal[ccy] -= v
	}

	withRestriction, err := tk.restrictedNetAssets(ctx, p.Scope, p.AsOf)
	if err != nil {
		return Table{}, err
	}
	withoutRestriction := map[string]int64{}
	for ccy, v := range netAssetsTotal {
		withoutRestriction[ccy] = v - withRestriction[ccy]
	}
	for ccy, v := range withRestriction {
		if _, ok := netAssetsTotal[ccy]; !ok {
			// A restricted currency with no net-asset total: still show the negative.
			withoutRestriction[ccy] = -v
		}
	}

	// Net surplus to date: cumulative R/E activity from inception to the as-of date.
	// NetIncome is net-debit (a surplus is a net CREDIT, negative); present it as a
	// positive surplus.
	surplus, err := tk.netSurplusByCurrency(ctx, p.Scope, p.AsOf)
	if err != nil {
		return Table{}, err
	}

	// Intercompany residual (D19): the flagged accounts, collapsed across the
	// CONSOLIDATED scope, must net to zero per currency; a nonzero residual => a
	// warning row. Only computed at a consolidated scope (a leaf scope holds one side
	// legitimately and is not a consolidation, so it never warns).
	var icNet []CurAmt
	if consolidated {
		icNet, err = tk.IntercompanyNet(ctx, Scope{Sub: p.Scope}, p.AsOf)
		if err != nil {
			return Table{}, err
		}
	}

	b := &bsBuilder{tk: tk, ctx: ctx, p: p, target: target, detail: p.DetailCurrency()}

	// --- Assets section.
	b.sectionHeader("reports.balance_sheet.section.assets")
	for _, l := range assets {
		b.accountLine(l)
	}
	b.totalLine("reports.balance_sheet.total.assets", assetTotal)

	// --- Liabilities section.
	b.sectionHeader("reports.balance_sheet.section.liabilities")
	for _, l := range liabilities {
		b.accountLine(l)
	}
	b.totalLine("reports.balance_sheet.total.liabilities", liabilityTotal)

	// --- Net Assets section (by-restriction split; synthetic lines only).
	b.sectionHeader("reports.balance_sheet.section.net_assets")
	b.syntheticLine("reports.balance_sheet.na.without", withoutRestriction, false)
	b.syntheticLine("reports.balance_sheet.na.surplus_of_which", surplus, true) // "of which" memo, indented
	b.syntheticLine("reports.balance_sheet.na.with", withRestriction, false)
	b.totalLine("reports.balance_sheet.total.net_assets", netAssetsTotal)

	// --- Total liabilities + net assets (the identity's right-hand side; equals
	// total assets on a balanced statement).
	lPlusNA := map[string]int64{}
	for ccy, v := range liabilityTotal {
		lPlusNA[ccy] += v
	}
	for ccy, v := range netAssetsTotal {
		lPlusNA[ccy] += v
	}
	b.grandTotalLine("reports.balance_sheet.total.liabilities_net_assets", lPlusNA)

	// --- Intercompany warning row (only when nonzero, D19). Surfaced, never hidden.
	if hasNonzero(icNet) {
		b.warningLine("reports.balance_sheet.intercompany_warning", icNet)
	}

	return b.table(), nil
}

// bsBuilder accumulates the Table rows with the right column shape for the current
// detail mode. In converted-only mode the columns are [Line, Converted]; in
// per-currency detail mode they are [Line, Currency, Native, Converted].
type bsBuilder struct {
	tk     *Toolkit
	ctx    context.Context
	p      Params
	target string
	detail bool
	rows   []Row
}

func (b *bsBuilder) columns() []Column {
	if b.detail {
		return []Column{
			{HeaderKey: "reports.balance_sheet.col.line", Align: AlignLeft},
			{HeaderKey: "reports.balance_sheet.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.balance_sheet.col.native", Align: AlignRight},
			{HeaderKey: "reports.balance_sheet.col.converted", Align: AlignRight},
		}
	}
	return []Column{
		{HeaderKey: "reports.balance_sheet.col.line", Align: AlignLeft},
		{HeaderKey: "reports.balance_sheet.col.amount", Align: AlignRight},
	}
}

// convert converts a native per-currency map to the target's minor total at the AsOf
// closing rate (D12), summing each currency's converted contribution.
func (b *bsBuilder) convert(byCcy map[string]int64) (int64, error) {
	var total int64
	for _, ccy := range sortedKeys(byCcy) {
		conv := byCcy[ccy]
		if b.target != "" {
			c, err := b.tk.ConvertMinorAt(b.ctx, byCcy[ccy], ccy, b.target, b.p.AsOf)
			if err != nil {
				return 0, err
			}
			conv = c
		}
		total += conv
	}
	return total, nil
}

// convCcy is the converted column's currency (target, or -- with no target -- blank
// so the cell mirrors native; in that degenerate case the converted total is only
// meaningful single-currency).
func (b *bsBuilder) convCcy() string { return b.target }

// sectionHeader appends a section heading row (a label + blank amount cells).
func (b *bsBuilder) sectionHeader(key string) {
	b.rows = append(b.rows, Row{Cells: b.labelRow(LabelCell(key)), Kind: RowData})
}

// accountLine appends an asset/liability account line: its converted total (and, in
// detail mode, one row per native currency with a drill on the native cell). The
// account name is a proper noun (TextCell). In detail mode the FIRST currency row
// carries the name and subsequent rows are blank-named continuations.
func (b *bsBuilder) accountLine(l bsLine) {
	if !b.detail {
		conv, _ := b.convert(l.byCcy)
		b.rows = append(b.rows, Row{
			Cells:  b.amountRow(TextCell(l.name), conv, b.convCcy(), b.accountDrillAll(l)),
			Indent: 1,
			Kind:   RowData,
		})
		return
	}
	first := true
	for _, ccy := range sortedKeys(l.byCcy) {
		native := l.byCcy[ccy]
		conv := native
		if b.target != "" {
			conv, _ = b.tk.ConvertMinorAt(b.ctx, native, ccy, b.target, b.p.AsOf)
		}
		nameCell := TextCell("")
		if first {
			nameCell = TextCell(l.name)
			first = false
		}
		nativeCell := MoneyCell(native, ccy)
		if d := b.accountDrill(l, ccy); d != nil {
			nativeCell = nativeCell.WithDrill(d)
		}
		b.rows = append(b.rows, Row{
			Cells:  []Cell{nameCell, TextCell(ccy), nativeCell, MoneyCell(conv, b.convCcyOr(ccy))},
			Indent: 1,
			Kind:   RowData,
		})
	}
}

// syntheticLine appends a net-asset line (no account; not drillable). ofWhich renders
// it as an indented "of which" disclosure memo (net surplus to date).
func (b *bsBuilder) syntheticLine(key string, byCcy map[string]int64, ofWhich bool) {
	indent := 1
	if ofWhich {
		indent = 2
	}
	if !b.detail {
		conv, _ := b.convert(byCcy)
		b.rows = append(b.rows, Row{
			Cells:  b.amountRow(LabelCell(key), conv, b.convCcy(), nil),
			Indent: indent,
			Kind:   RowData,
		})
		return
	}
	first := true
	for _, ccy := range sortedKeys(byCcy) {
		native := byCcy[ccy]
		conv := native
		if b.target != "" {
			conv, _ = b.tk.ConvertMinorAt(b.ctx, native, ccy, b.target, b.p.AsOf)
		}
		nameCell := LabelCell(key)
		if !first {
			nameCell = TextCell("")
		}
		first = false
		b.rows = append(b.rows, Row{
			Cells:  []Cell{nameCell, TextCell(ccy), MoneyCell(native, ccy), MoneyCell(conv, b.convCcyOr(ccy))},
			Indent: indent,
			Kind:   RowData,
		})
	}
}

// totalLine appends a section subtotal row (converted total; per-currency in detail).
func (b *bsBuilder) totalLine(key string, byCcy map[string]int64) {
	b.emphasized(key, byCcy, RowSubtotal, 0)
}

// grandTotalLine appends the identity's right-hand grand total (L + NA).
func (b *bsBuilder) grandTotalLine(key string, byCcy map[string]int64) {
	b.emphasized(key, byCcy, RowTotal, 0)
}

func (b *bsBuilder) emphasized(key string, byCcy map[string]int64, kind RowKind, indent int) {
	if !b.detail {
		conv, _ := b.convert(byCcy)
		b.rows = append(b.rows, Row{
			Cells:  b.amountRow(LabelCell(key), conv, b.convCcy(), nil),
			Indent: indent,
			Kind:   kind,
		})
		return
	}
	first := true
	for _, ccy := range sortedKeys(byCcy) {
		native := byCcy[ccy]
		conv := native
		if b.target != "" {
			conv, _ = b.tk.ConvertMinorAt(b.ctx, native, ccy, b.target, b.p.AsOf)
		}
		nameCell := LabelCell(key)
		if !first {
			nameCell = TextCell("")
		}
		first = false
		b.rows = append(b.rows, Row{
			Cells:  []Cell{nameCell, TextCell(ccy), MoneyCell(native, ccy), MoneyCell(conv, b.convCcyOr(ccy))},
			Indent: indent,
			Kind:   kind,
		})
	}
}

// warningLine appends the D19 intercompany warning row (nonzero residual). In both
// modes it shows the residual converted; in detail mode, per currency.
func (b *bsBuilder) warningLine(key string, amts []CurAmt) {
	byCcy := map[string]int64{}
	for _, a := range amts {
		byCcy[a.Currency] += a.Minor
	}
	b.emphasized(key, byCcy, RowWarning, 0)
}

// labelRow builds a row's cells for a pure label heading (blank amount columns).
func (b *bsBuilder) labelRow(label Cell) []Cell {
	if b.detail {
		return []Cell{label, TextCell(""), BlankMoneyCell(), BlankMoneyCell()}
	}
	return []Cell{label, BlankMoneyCell()}
}

// amountRow builds a converted-only row's cells: the name/label cell + the converted
// amount cell (drillable when d != nil, though only account cells drill).
func (b *bsBuilder) amountRow(nameCell Cell, conv int64, ccy string, d *Drill) []Cell {
	amt := MoneyCell(conv, ccy)
	if d != nil {
		amt = amt.WithDrill(d)
	}
	return []Cell{nameCell, amt}
}

// convCcyOr returns the converted-column currency: the target, or ccy when no target
// is set (so a native-mode run mirrors the native value honestly).
func (b *bsBuilder) convCcyOr(ccy string) string {
	if b.target == "" {
		return ccy
	}
	return b.target
}

// accountDrill builds the p15.3d drill for one (account, currency) as-of balance --
// the trial-balance retrofit pattern: same scope (descendant closure), this account,
// this native currency, cumulative to AsOf, so the drilled splits' signed native sum
// reconciles to the pre-sign-normalization native figure.
func (b *bsBuilder) accountDrill(l bsLine, ccy string) *Drill {
	if l.acctID == 0 {
		return nil
	}
	return &Drill{
		Scope:      b.p.Scope,
		AccountIDs: []int64{l.acctID},
		Currency:   ccy,
		Mode:       DrillAsOf,
		AsOf:       b.p.AsOf,
	}
}

// accountDrillAll builds a drill for the converted-only account cell. A single-
// currency account drills to that currency; a multi-currency account is left non-
// drillable in the converted-only view (one link cannot reconcile a summed-across-
// currencies converted figure) -- the per-currency detail view is the drill path for
// those. Assets/liabilities in this fixture are single-currency except FX Clearing.
func (b *bsBuilder) accountDrillAll(l bsLine) *Drill {
	if l.acctID == 0 || len(l.byCcy) != 1 {
		return nil
	}
	for ccy := range l.byCcy {
		return b.accountDrill(l, ccy)
	}
	return nil
}

func (b *bsBuilder) table() Table {
	return Table{Columns: b.columns(), Rows: b.rows}
}

// --- toolkit helpers (p15.4) -----------------------------------------------

// isConsolidated reports whether the scope covers MORE THAN ONE subsidiary (its
// descendant closure has >1 sub) -- i.e. it is a consolidation where intercompany
// balances are internal and eliminated (D19). A leaf (single-sub) scope is not a
// consolidation: its intercompany accounts are genuine due-to/due-from balances.
func (tk *Toolkit) isConsolidated(ctx context.Context, scope int64) (bool, error) {
	desc, err := tk.store.Descendants(ctx, scope)
	if err != nil {
		return false, err
	}
	return len(desc) > 1, nil
}

// restrictedNetAssets returns, per currency, the sum of the RESTRICTED funds'
// asset-side (unexpended) balances as of d in the scope -- "net assets with donor
// restrictions" (Q3, D20). A fund is restricted when its Restriction field is
// non-empty (purpose/time/perpetual); fund id 0 (unrestricted) is excluded. The
// asset-side balance is exactly what FundBalancesAsOf returns (a whole-fund sum is
// zero by conservation, so the asset position is the unexpended restricted resource).
func (tk *Toolkit) restrictedNetAssets(ctx context.Context, scope int64, d string) (map[string]int64, error) {
	funds, err := tk.store.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	restricted := map[int64]bool{}
	for _, f := range funds {
		if f.Restriction != "" {
			restricted[f.ID] = true
		}
	}
	fb, err := tk.store.FundBalancesAsOf(ctx, d, scope)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range fb {
		if r.FundID != 0 && restricted[r.FundID] {
			out[r.Currency] += r.Amount
		}
	}
	return out, nil
}

// netSurplusByCurrency returns the accumulated net surplus (revenue - expense) from
// inception to d in the scope, per NATIVE currency, presented POSITIVE (a surplus).
// NetIncome is net-debit signed (a surplus is a net credit, negative), so each
// currency subtotal is negated. Inception is the earliest possible date ("" lower
// bound via PeriodActivity is inclusive of everything on-or-before d); the fixture's
// earliest txn is 2025-01-01, so a fixed early bound is cumulative-from-inception.
func (tk *Toolkit) netSurplusByCurrency(ctx context.Context, scope int64, d string) (map[string]int64, error) {
	act, err := tk.Activity(ctx, Scope{Sub: scope}, inceptionDate, d, ConvertOpts{Mode: RateNone})
	if err != nil {
		return nil, err
	}
	reReport, err := tk.reAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for acct, amts := range act {
		if !reReport[acct] {
			continue
		}
		for _, a := range amts {
			out[a.Currency] -= a.Minor // net-debit -> positive surplus
		}
	}
	return out, nil
}

// inceptionDate is the "from" bound for the cumulative net-surplus-to-date figure: a
// date on-or-before every possible transaction, so the accumulated surplus is truly
// from inception. The ledger has no transactions before this (the synthetic fixture
// starts 2025-01; a real org's first entries postdate it), and PeriodActivity is
// inclusive, so it captures the full accumulated surplus without a per-org config.
const inceptionDate = "1900-01-01"

// --- small pure helpers ----------------------------------------------------

// sumLines sums a section's lines into a per-currency total map.
func sumLines(lines []bsLine) map[string]int64 {
	out := map[string]int64{}
	for _, l := range lines {
		for ccy, v := range l.byCcy {
			out[ccy] += v
		}
	}
	return out
}

// sortedKeys returns a currency-code-sorted key slice for deterministic output.
func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hasNonzero reports whether any currency's intercompany residual is nonzero (=> a
// warning row is emitted, D19).
func hasNonzero(amts []CurAmt) bool {
	for _, a := range amts {
		if a.Minor != 0 {
			return true
		}
	}
	return false
}

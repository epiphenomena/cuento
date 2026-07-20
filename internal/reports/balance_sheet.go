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
// unchanged. A NONZERO residual is NOT hidden and NOT presented as an unexplained error
// (p26.70): most of it is a legitimate FX TRANSLATION ADJUSTMENT (ASC 830 --
// retranslating accumulated foreign intercompany balances at the closing rate) plus a
// smaller genuine-imbalance core. In the CONVERTED (single-target) view it is
// reclassified into a Cumulative Translation Adjustment line (closing − historical value)
// and a reconciling-difference line (historical value), both carved OUT of the
// without-restriction figure so the net-assets total and the balance-sheet identity are
// unchanged (assets == L + NA still holds; the added net-asset deltas sum to zero). In
// the per-currency NATIVE view there is no single rate, so the residual is shown as the
// reconciling-difference line only (no translation component). A LEAF/single-sub scope is
// NOT a consolidation: its intercompany accounts are that subsidiary's genuine
// due-to-parent / due-from-child balances, shown as ordinary account rows, never
// collapsed and never reclassified.
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
		ID:       BalanceSheetReportID,
		TitleKey: "reports.balance_sheet.title",
		Group:    "financial",
		// p15.4 filter: Fund narrows the statement to one fund's OWN position (the
		// "— all funds —" default leaves the org-wide statement byte-identical). A single
		// fund is not a consolidation, so it carries no intercompany elimination/CTA.
		ParamsSpec: ParamsSpec{AsOf: true, Currency: true, Detail: true, Fund: true},
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
	acctID AccountID
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
	// "Net assets with donor restrictions" needs a per-FUND scan (FundBalancesAsOf),
	// independent of the per-account BalancesAsOf below. Run it CONCURRENTLY so its
	// full-ledger scan overlaps BalancesAsOf's rather than running after it (p29.6 perf).
	// Safe: the store is WAL with a pooled connection set and the toolkit holds no mutable
	// state, so two toolkit reads run on two connections without contention. The channel
	// is buffered (cap 1) so an early error return below never leaks the goroutine.
	type rnaResult struct {
		m   map[string]int64
		err error
	}
	// FUND selector (p15.4): a fund-scoped Statement of Position presents that single
	// fund's OWN assets, liabilities, and net assets. Its per-account balances come from
	// the fund-filtered ledger (FundBalancesAsOfByAccount), NOT the whole-scope
	// SubtreeBalancesAsOf. A single fund is NOT a consolidation, so there is no
	// intercompany elimination or CTA reclassification (icAccts stays empty, consolidated
	// false below). "With donor restrictions" for a single fund is the fund's OWN
	// asset-side balance when the fund is restricted, else zero -- computed from the same
	// fund balances rather than the org-wide restricted-fund scan. When no fund is chosen
	// this whole branch is skipped, so the org-wide statement is byte-identical (goldens
	// do not move).
	fundFilter := p.Fund != 0

	rnaCh := make(chan rnaResult, 1)
	if !fundFilter {
		go func() {
			m, err := tk.restrictedNetAssets(ctx, p.Scope, p.AsOf)
			rnaCh <- rnaResult{m, err}
		}()
	}

	var balances map[AccountID][]CurAmt
	var err error
	if fundFilter {
		balances, err = tk.FundBalancesAsOfByAccount(ctx, p.Fund, Scope{Sub: p.Scope}, p.AsOf)
	} else {
		balances, err = tk.BalancesAsOf(ctx, Scope{Sub: p.Scope}, p.AsOf, ConvertOpts{Mode: RateNone})
	}
	if err != nil {
		return Table{}, err
	}
	tree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}

	// R/E account ids, straight from the tree we already fetched (no extra query) — used
	// to derive the net surplus from `balances` below instead of re-scanning the ledger.
	reReport := map[AccountID]bool{}
	for _, node := range tree {
		if node.Type == "revenue" || node.Type == "expense" {
			reReport[node.ID] = true
		}
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
	// A single-fund view is never a consolidation: the fund's intercompany legs (if any)
	// are its genuine due-to/due-from balances, shown as ordinary rows, never eliminated
	// or reclassified into a CTA line (p15.4 fund filter).
	if fundFilter {
		consolidated = false
	}
	icAccts := map[AccountID]bool{}
	if consolidated {
		ids, err := tk.store.IntercompanyAccountIDs(ctx)
		if err != nil {
			return Table{}, err
		}
		for _, id := range ids {
			icAccts[id] = true
		}
	}

	// --- classify LEAF accounts into the Assets and Liabilities sections. Walk the
	// tree pre-order (stable order + resolved names). Net-debit signs (D2): assets are
	// positive as stored; liabilities are stored NEGATIVE (credit), so negate to show
	// a positive liability balance. Equity/revenue/expense accounts are skipped --
	// they are absorbed into the net-asset plug below. Intercompany-flagged accounts
	// are ELIMINATED at a consolidated scope (icAccts is empty at a leaf scope).
	//
	// assetLeaf / liabLeaf index each in-section leaf by account id (for the p26.53
	// tree walk, scoped per section); assets/liabilities keep the flat ordered lists
	// the section TOTALS sum over.
	assetLeaf := map[AccountID]bsLine{}
	liabLeaf := map[AccountID]bsLine{}
	var assets, liabilities []bsLine
	for _, node := range tree {
		amts, ok := balances[AccountID(node.ID)]
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
			assetLeaf[node.ID] = line
		case "liability":
			line := bsLine{name: node.Name, acctID: node.ID}
			for _, a := range amts {
				line.add(a.Currency, -a.Minor) // stored credit -> positive liability
			}
			liabilities = append(liabilities, line)
			liabLeaf[node.ID] = line
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

	var withRestriction map[string]int64
	if fundFilter {
		// Single-fund "with donor restrictions": the fund's OWN still-restricted MONETARY
		// balance (current_cash + receivable_payable assets net of liabilities) when the
		// fund is RESTRICTED (purpose/time/perpetual), else empty. The fund's DEPLOYED
		// non-monetary assets (a capitalized building) are released, so without = plug
		// (A - L) - monetary = the deployed amount. This mirrors the org-wide
		// restrictedNetAssets narrowed to the one chosen fund.
		wr, err := tk.fundRestrictedNetAssets(ctx, p.Scope, p.AsOf, p.Fund)
		if err != nil {
			return Table{}, err
		}
		withRestriction = wr
	} else {
		rna := <-rnaCh
		if rna.err != nil {
			return Table{}, rna.err
		}
		withRestriction = rna.m
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
	// R/E accounts carry no opening balance, so an R/E account's balance AS OF the date
	// IS its inception-to-date activity — already present in `balances`. Deriving it from
	// there avoids a redundant full-ledger scan (the old netSurplusByCurrency re-ran
	// Activity(inception, asof); p29.6 perf). NetIncome is net-debit (a surplus is a net
	// CREDIT, negative); present it as a positive surplus.
	surplus := map[string]int64{}
	for acct, amts := range balances {
		if !reReport[acct] {
			continue
		}
		for _, a := range amts {
			surplus[a.Currency] -= a.Minor
		}
	}

	// Intercompany residual (D19): the flagged accounts, collapsed across the
	// CONSOLIDATED scope, ideally net to zero per currency. A nonzero residual is NOT
	// an unexplained error — most of it is a legitimate FX TRANSLATION ADJUSTMENT (ASC
	// 830: retranslating accumulated foreign intercompany balances at the closing rate),
	// with a smaller genuine-imbalance core. p26.70 reclassifies it: in the CONVERTED
	// (single-target) view the residual is split into a Cumulative Translation Adjustment
	// line (closing − historical value) and a reconciling-difference line (historical
	// value), both carved OUT of the without-restriction figure so the net-assets total —
	// and the balance-sheet identity — are unchanged. In the per-currency NATIVE detail
	// view there is no single rate, so the native residual is shown as the reconciling
	// difference only (no translation component). Only at a consolidated scope.
	var icNet []CurAmt
	var icSplit ICResidualSplit
	if consolidated {
		// IntercompanyNet is the intercompany-flagged accounts' balances summed per
		// currency — the SAME SubtreeBalancesAsOf already in `balances`. Derive it here
		// rather than re-running that scan (p29.6 perf). icAccts was built above for the
		// consolidated elimination, so it is exactly the intercompany set.
		icByCcy := map[string]int64{}
		for acct, amts := range balances {
			if icAccts[acct] {
				for _, a := range amts {
					icByCcy[a.Currency] += a.Minor
				}
			}
		}
		icNet = sortedCurAmts(icByCcy)
		if hasNonzero(icNet) && target != "" {
			icSplit, err = tk.IntercompanyResidualSplit(ctx, Scope{Sub: p.Scope}, p.AsOf, target)
			if err != nil {
				return Table{}, err
			}
		}
	}

	b := &bsBuilder{tk: tk, ctx: ctx, p: p, target: target, detail: p.DetailCurrency()}

	// p26.53: index the account tree so each section renders the NESTED account
	// hierarchy (placeholder parents as rolled-up subtotal rows, leaves indented under
	// them) -- consistent with the trial balance / income statement / chart. In the
	// synthetic base fixture every asset/liability is a top-level leaf (no grouping
	// parent), so this adds no rows there; on a real chart with parent asset/liability
	// accounts (e.g. Fixed Assets > Building) the parents now surface with their
	// rolled subtotals instead of being dropped.
	tn := toTreeNodes(tree)
	children, roots, isPlaceholder, name, depth, _ := indexTree(tn)

	// --- Assets section (nested tree; section header at Indent 0, accounts at
	// treeDepth+1 so a top-level leaf stays at Indent 1 exactly as before).
	b.sectionHeader("reports.balance_sheet.section.assets")
	b.emitSectionTree(children, roots, isPlaceholder, name, depth, assetLeaf)
	b.totalLine("reports.balance_sheet.total.assets", assetTotal)

	// --- Liabilities section.
	b.sectionHeader("reports.balance_sheet.section.liabilities")
	b.emitSectionTree(children, roots, isPlaceholder, name, depth, liabLeaf)
	b.totalLine("reports.balance_sheet.total.liabilities", liabilityTotal)

	// --- Net Assets section (by-restriction split; synthetic lines only).
	//
	// p26.70 CTA reclassification (converted view only — see the showCTA gate). The
	// intercompany residual is currently absorbed into the plug: eliminating the IC legs
	// leaves consolidated assets short by the residual, so the plug (net assets = assets −
	// liabilities) and thus the without-restriction figure are distorted by −closing. We
	// carve that distortion out into two labeled lines WITHOUT changing the net-assets
	// total, so the balance-sheet identity is untouched:
	//   - restore the without-restriction line to its undistorted value: + closing;
	//   - CTA line          = historical − closing  (the FX translation component);
	//   - reconciling line  = − historical          (the genuine imbalance, → 0 as the
	//                                                 import cutoff fix lands).
	// The three added deltas sum to zero (+closing) + (historical − closing) + (−historical)
	// == 0, so without + CTA + reconciling + with still foots to the plug and A == L + NA
	// holds exactly. The signs are FORCED by the identity: with `without` restored to its
	// clean value, CTA + reconciling MUST equal −closing (the amount the elimination pulled
	// out of the plug) — a "+closing" reading would drive without ~2×closing away from clean
	// and break balance. So the lines are the negation of the raw split components; "no
	// value lost" holds in magnitude (|CTA + reconciling| == the old closing residual). The
	// residual is converted (target minor) — injecting {target: v} into a native map
	// converts identity, so the closing restoration lands exactly on the converted total.
	// The CTA reclassification is a CONVERTED-view (single-target) presentation: a
	// translation adjustment only exists under conversion, and the carve injects a
	// target-currency amount into the converted total. The per-currency NATIVE DETAIL view
	// (Detail=="currency") has no single rate, so it must NOT run the split — it falls
	// through to the native reconciling line below (the residual shown per native
	// currency). Hence the gate excludes detail mode as well as the no-target case.
	withoutShown := withoutRestriction
	showCTA := consolidated && hasNonzero(icNet) && target != "" && !b.detail
	if showCTA {
		withoutShown = map[string]int64{}
		for ccy, v := range withoutRestriction {
			withoutShown[ccy] = v
		}
		withoutShown[target] += icSplit.Closing // restore the undistorted figure
	}
	b.sectionHeader("reports.balance_sheet.section.net_assets")
	b.syntheticLine("reports.balance_sheet.na.without", withoutShown, false)
	b.syntheticLine("reports.balance_sheet.na.surplus_of_which", surplus, true) // "of which" memo, indented
	if showCTA {
		cta := map[string]int64{target: icSplit.Historical - icSplit.Closing}
		reconciling := map[string]int64{target: -icSplit.Historical}
		b.syntheticLine("reports.balance_sheet.na.cta", cta, false)
		b.syntheticLine("reports.balance_sheet.na.ic_reconciling", reconciling, false)
	}
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

	// --- Intercompany residual, native view (no target): there is no single rate, so it
	// cannot be split into a translation adjustment. Show it as the reconciling-difference
	// line per native currency (p26.70) — the residual is never hidden, just labeled
	// honestly rather than flagged as an unexplained error. When the CTA split ran
	// (converted view) the residual is already presented as the CTA + reconciling lines
	// above, so no extra row here.
	if hasNonzero(icNet) && !showCTA {
		b.warningLine("reports.balance_sheet.na.ic_reconciling", icNet)
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

// emitSectionTree walks the account tree pre-order (p26.53) and emits the section's
// NESTED hierarchy: a PLACEHOLDER parent that has any in-section leaf beneath it becomes
// a rolled-up SUBTOTAL row; each in-section LEAF becomes an account row. Every account
// row sits at Indent = treeDepth+1 (the section header is Indent 0), so a TOP-LEVEL leaf
// stays at Indent 1 exactly as the pre-p26.53 flat layout -- adding a parent above a leaf
// pushes the leaf to Indent 2, mirroring the trial balance. leaf indexes the in-section
// leaves (already sign-normalized + intercompany-eliminated); a placeholder with no
// in-section leaf beneath it is skipped (empty chart branch).
func (b *bsBuilder) emitSectionTree(
	children map[AccountID][]AccountID, roots []AccountID, isPlaceholder map[AccountID]bool,
	name map[AccountID]string, depth map[AccountID]int, leaf map[AccountID]bsLine,
) {
	// hasLeaf marks a node whose subtree carries an in-section leaf (so empty
	// placeholder branches drop out). A leaf qualifies iff it is in `leaf`.
	hasLeaf := map[AccountID]bool{}
	var mark func(id AccountID) bool
	mark = func(id AccountID) bool {
		if !isPlaceholder[id] {
			_, ok := leaf[id]
			hasLeaf[id] = ok
			return ok
		}
		any := false
		for _, c := range children[id] {
			if mark(c) {
				any = true
			}
		}
		hasLeaf[id] = any
		return any
	}
	for _, r := range roots {
		mark(r)
	}

	var walk func(id AccountID)
	walk = func(id AccountID) {
		if !hasLeaf[id] {
			return
		}
		if isPlaceholder[id] {
			b.parentSubtotal(name[id], b.subtreeByCcy(id, children, isPlaceholder, leaf), depth[id]+1)
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		b.accountLine(leaf[id], depth[id]+1)
	}
	for _, r := range roots {
		walk(r)
	}
}

// subtreeByCcy sums the per-currency native balances of every in-section LEAF beneath id
// (id inclusive when it is itself a leaf) -- a placeholder parent's rolled figure. The
// intercompany elimination and sign normalization already live in the leaf bsLines, so a
// parent's rollup inherits them (an eliminated child is simply absent from `leaf`, D19).
func (b *bsBuilder) subtreeByCcy(
	id AccountID, children map[AccountID][]AccountID, isPlaceholder map[AccountID]bool, leaf map[AccountID]bsLine,
) map[string]int64 {
	out := map[string]int64{}
	var add func(n AccountID)
	add = func(n AccountID) {
		if !isPlaceholder[n] {
			if l, ok := leaf[n]; ok {
				for ccy, v := range l.byCcy {
					out[ccy] += v
				}
			}
			return
		}
		for _, c := range children[n] {
			add(c)
		}
	}
	add(id)
	return out
}

// parentSubtotal appends a placeholder parent's rolled-up subtotal row at the given
// indent: its converted rollup (blank native in detail mode -- a mixed-currency subtree
// has no single native figure, mirroring the trial balance). Not drillable (a rollup
// spans many leaves).
func (b *bsBuilder) parentSubtotal(nm string, byCcy map[string]int64, indent int) {
	if !b.detail {
		conv, _ := b.convert(byCcy)
		b.rows = append(b.rows, Row{
			Cells:  b.amountRow(TextCell(nm), conv, b.convCcy(), nil),
			Indent: indent,
			Kind:   RowSubtotal,
		})
		return
	}
	conv, _ := b.convert(byCcy)
	b.rows = append(b.rows, Row{
		Cells:  []Cell{TextCell(nm), TextCell(""), BlankMoneyCell(), MoneyCell(conv, b.convCcy())},
		Indent: indent,
		Kind:   RowSubtotal,
	})
}

// accountLine appends an asset/liability account leaf line at the given indent: its
// converted total (and, in detail mode, one row per native currency with a drill on the
// native cell). The account name is a proper noun (TextCell). In detail mode the FIRST
// currency row carries the name and subsequent rows are blank-named continuations.
func (b *bsBuilder) accountLine(l bsLine, indent int) {
	if !b.detail {
		conv, _ := b.convert(l.byCcy)
		b.rows = append(b.rows, Row{
			Cells:  b.amountRow(TextCell(l.name), conv, b.convCcy(), b.accountDrillAll(l)),
			Indent: indent,
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
			Indent: indent,
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

// totalLine appends a SECTION total row (converted total; per-currency in detail):
// "Total assets"/"Total liabilities"/"Total net assets". A RowSectionTotal (p30.10) —
// the definitive section figure, ranked ABOVE the placeholder-parent RowSubtotal rollups
// and BELOW the grand-total "Total liabilities and net assets" (RowTotal), so the three
// total tiers read distinctly (matching the statement of activities).
func (b *bsBuilder) totalLine(key string, byCcy map[string]int64) {
	b.emphasized(key, byCcy, RowSectionTotal, 0)
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
		AccountIDs: []AccountID{l.acctID},
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
func (tk *Toolkit) isConsolidated(ctx context.Context, scope SubsidiaryID) (bool, error) {
	desc, err := tk.store.Descendants(ctx, scope)
	if err != nil {
		return false, err
	}
	return len(desc) > 1, nil
}

// restrictedNetAssets returns, per currency, the sum of the RESTRICTED funds'
// still-restricted MONETARY net balances as of d in the scope -- "net assets with
// donor restrictions" (Q3, D20, p-golive). A fund is restricted when its Restriction
// field is non-empty (purpose/time/perpetual); fund id 0 (unrestricted) is excluded.
//
// The restricted figure is the fund's MONETARY position (MonetaryFundBalancesAsOf:
// current_cash + receivable_payable assets, net of liabilities), NOT the whole asset
// side. A restricted grant DEPLOYED into a non-monetary asset (land, a building) has
// satisfied its purpose and is RELEASED from restriction; only the spendable cash /
// receivables still owed to the purpose (net of liabilities) remain restricted. The
// released amount = full asset side - monetary, which the balance sheet surfaces in
// "without restrictions" (without = total NA - with), keeping with + without == total
// NA exactly. This makes the point-in-time line articulate with the flow report's
// "net assets released from restrictions" (which already releases on acquisition via
// AppliedNonExpense).
func (tk *Toolkit) restrictedNetAssets(ctx context.Context, scope SubsidiaryID, d string) (map[string]int64, error) {
	funds, err := tk.store.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	restricted := map[FundID]bool{}
	for _, f := range funds {
		if f.Restriction != "" {
			restricted[f.ID] = true
		}
	}
	fb, err := tk.store.MonetaryFundBalancesAsOf(ctx, d, scope)
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

// FundBalancesAsOfByAccount returns ONE fund's per-(account, currency) native cumulative
// balances as of d in the scope (p15.4 fund selector). It mirrors BalancesAsOf(RateNone)
// but reads the fund-filtered store query, so the balance sheet's classification/plug/
// identity logic runs unchanged over a single fund's ledger. Native only (the balance
// sheet converts downstream); a fund's whole-fund sum is zero by conservation (D20/Z10),
// so its Assets - Liabilities equals its net-asset balance and A = L + NA holds.
func (tk *Toolkit) FundBalancesAsOfByAccount(ctx context.Context, f FundID, s Scope, d string) (map[AccountID][]CurAmt, error) {
	rows, err := tk.store.FundSubtreeBalancesAsOf(ctx, f, d, s.Sub)
	if err != nil {
		return nil, err
	}
	out := make(map[AccountID][]CurAmt, len(rows))
	for _, r := range rows {
		acct := AccountID(r.AccountID)
		out[acct] = append(out[acct], CurAmt{Currency: r.Currency, Minor: r.Amount})
	}
	return out, nil
}

// fundRestrictedNetAssets returns the single fund's "net assets with donor restrictions"
// per currency: the fund's still-restricted MONETARY net balance (current_cash +
// receivable_payable assets, net of liabilities -- MonetaryFundBalancesAsOf) when the
// fund is RESTRICTED (non-empty Restriction), else an empty map (an unrestricted fund
// contributes nothing to the restricted line, so its whole plug reads as
// without-restriction). It mirrors the org-wide restrictedNetAssets narrowed to one
// fund: the fund's DEPLOYED non-monetary assets (a capitalized building) are released,
// so with = monetary and the released remainder falls into without (= plug - with).
func (tk *Toolkit) fundRestrictedNetAssets(ctx context.Context, scope SubsidiaryID, d string, f FundID) (map[string]int64, error) {
	funds, err := tk.store.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	restricted := false
	for _, fd := range funds {
		if fd.ID == f {
			restricted = fd.Restriction != ""
			break
		}
	}
	out := map[string]int64{}
	if !restricted {
		return out, nil
	}
	fb, err := tk.store.MonetaryFundBalancesAsOf(ctx, d, scope)
	if err != nil {
		return nil, err
	}
	for _, r := range fb {
		if r.FundID == f {
			out[r.Currency] += r.Amount
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

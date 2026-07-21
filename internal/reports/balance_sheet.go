package reports

import (
	"context"
	"sort"
	"strconv"

	"cuento/internal/store"
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
// MULTI-PERIOD COLUMNS (p18): the statement is presented as a SERIES of as-of value
// columns, left to right: column 1 is the selected as-of date, then the most recent
// December 31 STRICTLY BEFORE it, then the December 31 before that, and so on back to
// the earliest posting date in the ledger (bsAsOfDates). Every column is the SAME
// snapshot computation as of that column's date (computeSnapshot), so a reviewer reads
// the year-end progression of the position at a glance. The statement STRUCTURE (which
// account/net-asset/total rows exist, their drills and indents) is driven by column 1
// (the selected as-of); an older column with no balance for a row shows a zero cell.
// The multi-period fan-out is the CONVERTED-only view; the per-currency DETAIL toggle
// stays a single as-of column (N periods x per-currency native/converted pairs would be
// a combinatorial column set out of proportion to the goal).
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
		// p18: the multi-period fan-out emits one column per year-end back to inception,
		// so render full-viewport-width (like the comparative income statement) rather
		// than truncating the older columns.
		WideMatrix: true,
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

// bsSnapshot is one as-of column's fully computed position (p18): the per-account
// sign-normalized native leaf balances (for account rows + placeholder-parent rollups
// + drills), the per-currency section totals and net-asset split, and the intercompany
// residual/CTA state -- everything the builder needs to emit that column's value cell
// for any row. It is the SAME computation the single-column statement always did,
// captured per date so the builder iterates it over the date list. `asOf` is the
// column's closing date (the rate seam converts each column at ITS own closing rate).
type bsSnapshot struct {
	asOf string

	// assetLeaf/liabLeaf: in-section leaves by account id (sign-normalized, IC-eliminated),
	// keyed for the p26.53 nested-tree walk and drills.
	assetLeaf map[AccountID]bsLine
	liabLeaf  map[AccountID]bsLine

	// per-currency native section totals and net-asset split (all positive as displayed).
	assetTotal      map[string]int64
	liabilityTotal  map[string]int64
	netAssetsTotal  map[string]int64
	withoutShown    map[string]int64 // without-restriction, post CTA restoration
	withRestriction map[string]int64
	surplus         map[string]int64

	// CTA reclassification (converted view only; nil when not shown for this column).
	cta         map[string]int64
	reconciling map[string]int64
	showCTA     bool

	// icNet is the native intercompany residual (for the native reconciling warning line).
	icNet []CurAmt
}

// runBalanceSheet computes the balance-sheet Table (p15.4, p18). It resolves the
// left-to-right as-of date series (the selected as-of, then each prior Dec 31 back to
// the earliest posting date), computes the SAME position snapshot as of each date, and
// emits the sections with one converted value column per date -- the statement structure
// (rows, drills, indents) driven by the primary (selected) as-of column. The
// per-currency detail toggle stays a single as-of column.
func runBalanceSheet(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	detail := p.DetailCurrency()

	// The as-of date series (p18): column 1 is the selected as-of, then each Dec 31
	// strictly before it back to the earliest posting date. The detail (per-currency)
	// view stays single-column (see the report doc-comment).
	var dates []string
	if detail {
		dates = []string{p.AsOf}
	} else {
		ds, err := bsAsOfDates(ctx, tk, p.AsOf)
		if err != nil {
			return Table{}, err
		}
		dates = ds
	}

	// The account tree is date-independent (the chart of accounts), so fetch it once.
	tree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}

	// Hoist all the DATE-INDEPENDENT context out of the per-column loop (p29.6 perf): the
	// consolidation flag, the intercompany account set, the restricted-fund set, and the
	// R/E account ids come from the chart / subsidiary tree, not the as-of date, so
	// recomputing them once per column (as the old N-pass loop did) was pure waste.
	sc, err := newSnapshotContext(ctx, tk, p, tree)
	if err != nil {
		return Table{}, err
	}

	// Pre-fetch every column's per-account balances and restricted figure. On the org-wide
	// (non-fund, non-detail) path this is a SINGLE ordered scan of dated postings for each
	// of the two grains -- accumulated per cutoff date in Go -- instead of N independent
	// as-of recomputations (the p18 perf fix). The fund / detail paths keep their per-date
	// as-of queries (narrower, single-column for detail; no dated fund-scan variant).
	balancesByDate, restrictedByDate, err := sc.fetchColumns(ctx, tk, p, dates)
	if err != nil {
		return Table{}, err
	}

	snaps := make([]bsSnapshot, len(dates))
	for i, d := range dates {
		snap, err := computeSnapshot(sc, d, balancesByDate[i], restrictedByDate[i])
		if err != nil {
			return Table{}, err
		}
		snaps[i] = snap
	}

	b := &bsBuilder{tk: tk, ctx: ctx, p: p, target: p.TargetCurrency, detail: detail, snaps: snaps}

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
	// treeDepth+1 so a top-level leaf stays at Indent 1 exactly as before). The primary
	// column's leaf set drives which account rows exist.
	b.sectionHeader("reports.balance_sheet.section.assets")
	b.emitSectionTree(children, roots, isPlaceholder, name, depth, sectionKind{asset: true})
	b.totalLine("reports.balance_sheet.total.assets", func(s bsSnapshot) map[string]int64 { return s.assetTotal })

	// --- Liabilities section.
	b.sectionHeader("reports.balance_sheet.section.liabilities")
	b.emitSectionTree(children, roots, isPlaceholder, name, depth, sectionKind{asset: false})
	b.totalLine("reports.balance_sheet.total.liabilities", func(s bsSnapshot) map[string]int64 { return s.liabilityTotal })

	// --- Net Assets section (by-restriction split; synthetic lines only). The CTA lines
	// appear iff ANY column reclassifies a residual (p26.70): a residual nonzero at an
	// older year-end but zero at the selected as-of must still surface its CTA/reconciling
	// disclosure (else that column's net-asset section would not foot). Each column fills
	// its OWN value; a column with no residual shows a zero cta/reconciling cell.
	anyCTA := false
	for _, s := range snaps {
		if s.showCTA {
			anyCTA = true
			break
		}
	}
	b.sectionHeader("reports.balance_sheet.section.net_assets")
	// The converted "without" line is derived as a RESIDUAL (Total NA - with - CTA -
	// reconciling) per column, NOT by converting the native without-map, so the net-asset
	// section FOOTS EXACTLY in converted space (each of NA/with/without would otherwise
	// round half-even independently and drift a minor unit -- the p15.5 "footing wins"
	// rule). On column 1 the residual reproduces the old figure byte-for-byte.
	b.withoutLine("reports.balance_sheet.na.without", anyCTA)
	b.syntheticLine("reports.balance_sheet.na.surplus_of_which", func(s bsSnapshot) map[string]int64 { return s.surplus }, true)
	if anyCTA {
		b.syntheticLine("reports.balance_sheet.na.cta", func(s bsSnapshot) map[string]int64 { return s.cta }, false)
		b.syntheticLine("reports.balance_sheet.na.ic_reconciling", func(s bsSnapshot) map[string]int64 { return s.reconciling }, false)
	}
	b.syntheticLine("reports.balance_sheet.na.with", func(s bsSnapshot) map[string]int64 { return s.withRestriction }, false)
	b.totalLine("reports.balance_sheet.total.net_assets", func(s bsSnapshot) map[string]int64 { return s.netAssetsTotal })

	// --- Total liabilities + net assets (the identity's right-hand side; equals total
	// assets on a balanced statement), per column.
	b.grandTotalLine("reports.balance_sheet.total.liabilities_net_assets", func(s bsSnapshot) map[string]int64 {
		lPlusNA := map[string]int64{}
		for ccy, v := range s.liabilityTotal {
			lPlusNA[ccy] += v
		}
		for ccy, v := range s.netAssetsTotal {
			lPlusNA[ccy] += v
		}
		return lPlusNA
	})

	// --- Intercompany residual, native view (no target): there is no single rate, so it
	// cannot be split into a translation adjustment. Show it as the reconciling-difference
	// line per native currency (p26.70). Emitted iff ANY column has a native residual not
	// already reclassified into a CTA line (each column fills its own per-currency value).
	anyNativeResidual := false
	for _, s := range snaps {
		if hasNonzero(s.icNet) && !s.showCTA {
			anyNativeResidual = true
			break
		}
	}
	if anyNativeResidual {
		b.warningLine("reports.balance_sheet.na.ic_reconciling", func(s bsSnapshot) map[string]int64 {
			if s.showCTA {
				return nil // this column reclassified into CTA above; no native line for it.
			}
			by := map[string]int64{}
			for _, a := range s.icNet {
				by[a.Currency] += a.Minor
			}
			return by
		})
	}

	return b.table(dates), nil
}

// bsAsOfDates builds the left-to-right as-of date series (p18): the selected as-of
// first, then the most recent December 31 STRICTLY BEFORE it, then each earlier
// December 31 back to (and including, when it is itself a Dec 31) the earliest posting
// date in the ledger. The strict "< asOf" is the on-Dec-31 dedup the task calls out: a
// selected as-of that lands ON a Dec 31 is NOT repeated as the "prior year-end" column.
// On an EMPTY ledger (LedgerDateRange ok=false) or a malformed as-of the series is just
// the single selected column, so the loop can never run away or go negative.
func bsAsOfDates(ctx context.Context, tk *Toolkit, asOf string) ([]string, error) {
	dates := []string{asOf}
	minDate, _, ok, err := tk.Store().LedgerDateRange(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return dates, nil // empty ledger: single column.
	}
	asOfYear, okY := isoYear(asOf)
	if !okY {
		return dates, nil // malformed as-of: single column, never loop.
	}
	// The first candidate year-end is Dec 31 of the selected as-of's year when that is
	// strictly before the as-of, else the prior year's Dec 31 (so an as-of ON Dec 31
	// dedups to the PRIOR year-end).
	year := asOfYear
	if ye := yearEnd(year); ye >= asOf {
		year--
	}
	for ; ; year-- {
		ye := yearEnd(year)
		if ye < minDate {
			break // earlier than the earliest posting: stop (guards the loop).
		}
		dates = append(dates, ye)
	}
	return dates, nil
}

// yearEnd returns the December 31 ISO date for the given year.
func yearEnd(year int) string { return strconv.Itoa(year) + "-12-31" }

// isoYear parses the leading YYYY of an ISO date (YYYY-MM-DD). ok=false on a malformed
// value so the caller falls back to a single column rather than looping on garbage.
func isoYear(iso string) (int, bool) {
	if len(iso) < 4 {
		return 0, false
	}
	y, err := strconv.Atoi(iso[:4])
	if err != nil || y <= 0 {
		return 0, false
	}
	// Reject an obviously-malformed date shape (must be YYYY-... or exactly YYYY).
	if len(iso) > 4 && iso[4] != '-' {
		return 0, false
	}
	return y, true
}

// snapshotContext holds the DATE-INDEPENDENT context shared by every as-of column of a
// single balance-sheet run (p29.6 perf): the account tree, the consolidation flag, the
// intercompany-eliminated account set, the R/E account ids (for the net-surplus derivation),
// and the restricted-fund set (for the org-wide restricted accumulation). The old code
// recomputed all of these once PER COLUMN (N passes); hoisting them here computes each once.
// `ctx`/`tk`/`p` are retained so the ONE remaining per-column IO -- the rare CTA residual
// split (converted view, nonzero intercompany residual) -- can still query per date.
type snapshotContext struct {
	ctx        context.Context
	tk         *Toolkit
	p          Params
	tree       []store.TreeRow
	fundFilter bool

	consolidated bool
	icAccts      map[AccountID]bool
	reReport     map[AccountID]bool // revenue/expense account ids (net-surplus source)
	restricted   map[FundID]bool    // restricted funds (org-wide "with restrictions" source)
}

// newSnapshotContext builds the date-independent snapshot context ONCE for a run: it
// resolves the consolidation flag + intercompany account set (skipped for a single fund,
// which is never a consolidation), the R/E account ids from the already-fetched tree, and
// the restricted-fund set (org-wide path only).
func newSnapshotContext(ctx context.Context, tk *Toolkit, p Params, tree []store.TreeRow) (*snapshotContext, error) {
	fundFilter := p.Fund != 0

	// --- Intercompany COLLAPSE (D19) applies only across a CONSOLIDATED (multi-sub) scope.
	// A single-fund view is never a consolidation: the fund's intercompany legs (if any) are
	// its genuine due-to/due-from balances, shown as ordinary rows, never eliminated.
	consolidated := false
	if !fundFilter {
		c, err := tk.isConsolidated(ctx, p.Scope)
		if err != nil {
			return nil, err
		}
		consolidated = c
	}
	icAccts := map[AccountID]bool{}
	if consolidated {
		ids, err := tk.store.IntercompanyAccountIDs(ctx)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			icAccts[id] = true
		}
	}

	// R/E account ids, straight from the tree (no extra query) -- used to derive the net
	// surplus from the per-column balances instead of re-scanning the ledger.
	reReport := map[AccountID]bool{}
	for _, node := range tree {
		if node.Type == "revenue" || node.Type == "expense" {
			reReport[node.ID] = true
		}
	}

	// The RESTRICTED-fund set (org-wide path): the funds whose monetary balance feeds "net
	// assets with donor restrictions". Fetched once, then applied per column over the dated
	// monetary scan. The fund-filtered path resolves restriction per its single fund instead.
	restricted := map[FundID]bool{}
	if !fundFilter {
		funds, err := tk.store.ListFunds(ctx)
		if err != nil {
			return nil, err
		}
		for _, f := range funds {
			if f.Restriction != "" {
				restricted[FundID(f.ID)] = true
			}
		}
	}

	return &snapshotContext{
		ctx: ctx, tk: tk, p: p, tree: tree, fundFilter: fundFilter,
		consolidated: consolidated, icAccts: icAccts, reReport: reReport, restricted: restricted,
	}, nil
}

// fetchColumns returns, for each as-of date in `dates`, the per-account native balances and
// the "with donor restrictions" per-currency figure. On the ORG-WIDE (non-fund, non-detail)
// path it does this from a SINGLE ordered scan of dated postings per grain (accounts +
// monetary funds), accumulating running balances and snapshotting at each cutoff date -- the
// p18 perf fix that replaces N independent as-of recomputations. Because column 1 is the max
// (selected) as-of, one scan to that date covers every earlier year-end column, and summing
// the dated cells for date <= cutoff reproduces each cutoff's as-of balance byte-for-byte
// (integer sums are associative). The FUND / DETAIL paths keep their per-date as-of queries
// (detail is single-column; there is no dated fund-scoped scan variant), still benefiting
// from the hoisted date-independent context.
func (sc *snapshotContext) fetchColumns(ctx context.Context, tk *Toolkit, p Params, dates []string) ([]map[AccountID][]CurAmt, []map[string]int64, error) {
	balancesByDate := make([]map[AccountID][]CurAmt, len(dates))
	restrictedByDate := make([]map[string]int64, len(dates))

	if sc.fundFilter || p.DetailCurrency() {
		// Narrow paths: per-date as-of queries (unchanged figures, hoisted context reused).
		for i, d := range dates {
			var bals map[AccountID][]CurAmt
			var err error
			if sc.fundFilter {
				bals, err = tk.FundBalancesAsOfByAccount(ctx, p.Fund, Scope{Sub: p.Scope}, d)
			} else {
				bals, err = tk.BalancesAsOf(ctx, Scope{Sub: p.Scope}, d, ConvertOpts{Mode: RateNone})
			}
			if err != nil {
				return nil, nil, err
			}
			balancesByDate[i] = bals

			var wr map[string]int64
			if sc.fundFilter {
				wr, err = tk.fundRestrictedNetAssets(ctx, p.Scope, d, p.Fund)
			} else {
				wr, err = tk.restrictedNetAssets(ctx, p.Scope, d)
			}
			if err != nil {
				return nil, nil, err
			}
			restrictedByDate[i] = wr
		}
		return balancesByDate, restrictedByDate, nil
	}

	// Org-wide single-scan path. dates[0] is the selected (max) as-of, so one dated scan to
	// it covers every column. Accumulate a running balance and snapshot at each cutoff.
	maxDate := dates[0]

	// (a) per-account balances, from the dated account scan.
	acctRows, err := tk.store.SubDatedBalancesAsOf(ctx, maxDate, p.Scope)
	if err != nil {
		return nil, nil, err
	}
	// Collapse the holding-subsidiary dimension (the balance sheet consolidates the whole
	// subtree into one balance per account/currency): sum activity to (account, currency, date).
	type acctKey struct {
		acct AccountID
		ccy  string
	}
	acctDated := map[acctKey]map[string]int64{} // key -> date -> summed activity that date
	for _, r := range acctRows {
		k := acctKey{acct: AccountID(r.AccountID), ccy: r.Currency}
		m := acctDated[k]
		if m == nil {
			m = map[string]int64{}
			acctDated[k] = m
		}
		m[r.Date] += r.Amount
	}
	for i, cutoff := range dates {
		bals := map[AccountID][]CurAmt{}
		byAcct := map[AccountID]map[string]int64{}
		for k, dm := range acctDated {
			var sum int64
			for date, amt := range dm {
				if date <= cutoff {
					sum += amt
				}
			}
			if sum == 0 {
				// A SubtreeBalancesAsOf GROUP BY would still emit a zero row if any split
				// existed for the (account, currency) on-or-before the cutoff. Preserve that:
				// emit the cell iff any dated activity falls on-or-before the cutoff.
				anyBefore := false
				for date := range dm {
					if date <= cutoff {
						anyBefore = true
						break
					}
				}
				if !anyBefore {
					continue
				}
			}
			cm := byAcct[k.acct]
			if cm == nil {
				cm = map[string]int64{}
				byAcct[k.acct] = cm
			}
			cm[k.ccy] = sum
		}
		// Emit per account in currency-sorted order (matches the store query's ORDER BY
		// account, currency -> the downstream bsLine.byCcy map is order-independent anyway).
		for acct, cm := range byAcct {
			for _, ccy := range sortedKeys(cm) {
				bals[acct] = append(bals[acct], CurAmt{Currency: ccy, Minor: cm[ccy]})
			}
		}
		balancesByDate[i] = bals
	}

	// (b) "with donor restrictions", from the dated monetary fund scan (restricted funds only).
	monRows, err := tk.store.MonetaryFundDatedBalancesAsOf(ctx, maxDate, p.Scope)
	if err != nil {
		return nil, nil, err
	}
	// Sum restricted-fund monetary activity to (currency, date); a fund's restriction is
	// date-independent, so filter here once.
	monDated := map[string]map[string]int64{} // ccy -> date -> activity (restricted funds)
	for _, r := range monRows {
		if r.FundID == 0 || !sc.restricted[FundID(r.FundID)] {
			continue
		}
		m := monDated[r.Currency]
		if m == nil {
			m = map[string]int64{}
			monDated[r.Currency] = m
		}
		m[r.Date] += r.Amount
	}
	for i, cutoff := range dates {
		wr := map[string]int64{}
		for ccy, dm := range monDated {
			var sum int64
			for date, amt := range dm {
				if date <= cutoff {
					sum += amt
				}
			}
			if sum != 0 {
				wr[ccy] = sum
			}
		}
		restrictedByDate[i] = wr
	}

	return balancesByDate, restrictedByDate, nil
}

// computeSnapshot computes the position AS OF `d` (p15.4) from the pre-fetched per-account
// native `balances` and "with donor restrictions" `withRestriction` for that column: it
// classifies the balances into Assets/Liabilities, derives the by-restriction net-asset
// split and the net surplus, and (converted view, nonzero intercompany residual only) runs
// the CTA reclassification. All the whole-run/date-independent work lives in `sc`; the only
// IO here is the rare per-column CTA residual split. This is the SAME computation the old
// per-date function did, split so the balances/restricted scans are done once up front.
func computeSnapshot(sc *snapshotContext, d string, balances map[AccountID][]CurAmt, withRestriction map[string]int64) (bsSnapshot, error) {
	tk, p, tree := sc.tk, sc.p, sc.tree
	ctx := sc.ctx
	target := p.TargetCurrency
	consolidated := sc.consolidated
	icAccts := sc.icAccts
	reReport := sc.reReport

	// --- classify LEAF accounts into the Assets and Liabilities sections. Walk the
	// tree pre-order (stable order + resolved names). Net-debit signs (D2): assets are
	// positive as stored; liabilities are stored NEGATIVE (credit), so negate to show
	// a positive liability balance. Equity/revenue/expense accounts are skipped --
	// they are absorbed into the net-asset plug below. Intercompany-flagged accounts
	// are ELIMINATED at a consolidated scope (icAccts is empty at a leaf scope).
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

	// --- Net assets. total NA = plug (Assets - Liabilities) per currency.
	netAssetsTotal := map[string]int64{}
	for ccy, v := range assetTotal {
		netAssetsTotal[ccy] += v
	}
	for ccy, v := range liabilityTotal {
		netAssetsTotal[ccy] -= v
	}

	// "With donor restrictions" is pre-fetched for this column (fundRestrictedNetAssets for a
	// single fund, or the org-wide dated monetary scan accumulation) -- see fetchColumns.
	withoutRestriction := map[string]int64{}
	for ccy, v := range netAssetsTotal {
		withoutRestriction[ccy] = v - withRestriction[ccy]
	}
	for ccy, v := range withRestriction {
		if _, ok := netAssetsTotal[ccy]; !ok {
			withoutRestriction[ccy] = -v
		}
	}

	// Net surplus to date: cumulative R/E activity from inception to `d`. An R/E account's
	// as-of balance IS its inception-to-date activity (R/E carry no opening balance), so it
	// is already present in `balances`. NetIncome is net-debit (a surplus is a net CREDIT,
	// negative); present it as a positive surplus.
	surplus := map[string]int64{}
	for acct, amts := range balances {
		if !reReport[acct] {
			continue
		}
		for _, a := range amts {
			surplus[a.Currency] -= a.Minor
		}
	}

	// Intercompany residual (D19, p26.70): the flagged accounts, collapsed across the
	// CONSOLIDATED scope, ideally net to zero per currency. In the CONVERTED view a nonzero
	// residual is reclassified into a CTA line + a reconciling line, both carved out of the
	// without-restriction figure so the net-assets total is unchanged.
	var icNet []CurAmt
	var icSplit ICResidualSplit
	if consolidated {
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
			split, err := tk.IntercompanyResidualSplit(ctx, Scope{Sub: p.Scope}, d, target)
			if err != nil {
				return bsSnapshot{}, err
			}
			icSplit = split
		}
	}

	// p26.70 CTA reclassification (converted view only). See the report doc-comment: the
	// three added deltas sum to zero so A == L + NA still holds exactly. The per-currency
	// NATIVE DETAIL view has no single rate, so it must NOT run the split.
	withoutShown := withoutRestriction
	showCTA := consolidated && hasNonzero(icNet) && target != "" && !p.DetailCurrency()
	var cta, reconciling map[string]int64
	if showCTA {
		withoutShown = map[string]int64{}
		for ccy, v := range withoutRestriction {
			withoutShown[ccy] = v
		}
		withoutShown[target] += icSplit.Closing // restore the undistorted figure
		cta = map[string]int64{target: icSplit.Historical - icSplit.Closing}
		reconciling = map[string]int64{target: -icSplit.Historical}
	}

	return bsSnapshot{
		asOf:            d,
		assetLeaf:       assetLeaf,
		liabLeaf:        liabLeaf,
		assetTotal:      assetTotal,
		liabilityTotal:  liabilityTotal,
		netAssetsTotal:  netAssetsTotal,
		withoutShown:    withoutShown,
		withRestriction: withRestriction,
		surplus:         surplus,
		cta:             cta,
		reconciling:     reconciling,
		showCTA:         showCTA,
		icNet:           icNet,
	}, nil
}

// sectionKind selects which section a snapshot's leaf index a builder walk reads: the
// Assets leaves (asset==true) or the Liabilities leaves.
type sectionKind struct{ asset bool }

func (k sectionKind) leaf(s bsSnapshot) map[AccountID]bsLine {
	if k.asset {
		return s.assetLeaf
	}
	return s.liabLeaf
}

// bsBuilder accumulates the Table rows with the right column shape for the current
// detail mode. In converted-only mode the columns are [Line, <as-of>...] (one value
// column per date in the p18 series); in per-currency detail mode they are
// [Line, Currency, Native, Converted] (single as-of).
type bsBuilder struct {
	tk     *Toolkit
	ctx    context.Context
	p      Params
	target string
	detail bool
	snaps  []bsSnapshot // one per as-of date; snaps[0] is the selected as-of.
	rows   []Row
}

// columns builds the column set. Detail mode: [Line, Currency, Native, Converted]
// (single as-of). Converted-only: [Line, <as-of date>...] one value column per date in
// the series, each header the raw ISO as-of date (a verbatim date marker the web layer
// renders per the user's setting -- like the cashflow/budget bucket headers, so no
// per-date i18n key is invented).
func (b *bsBuilder) columns(dates []string) []Column {
	if b.detail {
		return []Column{
			{HeaderKey: "reports.balance_sheet.col.line", Align: AlignLeft},
			{HeaderKey: "reports.balance_sheet.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.balance_sheet.col.native", Align: AlignRight},
			{HeaderKey: "reports.balance_sheet.col.converted", Align: AlignRight},
		}
	}
	cols := []Column{{HeaderKey: "reports.balance_sheet.col.line", Align: AlignLeft}}
	for _, d := range dates {
		cols = append(cols, Column{HeaderKey: d, Align: AlignRight})
	}
	return cols
}

// convertAt converts a native per-currency map to the target's minor total at the given
// as-of closing rate (D12), summing each currency's converted contribution. Multi-period
// columns each convert at THEIR column's closing date, so the rate is passed in (not
// b.p.AsOf).
func (b *bsBuilder) convertAt(byCcy map[string]int64, asOf string) (int64, error) {
	var total int64
	for _, ccy := range sortedKeys(byCcy) {
		conv := byCcy[ccy]
		if b.target != "" {
			c, err := b.tk.ConvertMinorAt(b.ctx, byCcy[ccy], ccy, b.target, asOf)
			if err != nil {
				return 0, err
			}
			conv = c
		}
		total += conv
	}
	return total, nil
}

// convCcy is the converted column's currency (target, or -- with no target -- blank).
func (b *bsBuilder) convCcy() string { return b.target }

// sectionHeader appends a section heading row (a label + blank value cells).
func (b *bsBuilder) sectionHeader(key string) {
	b.rows = append(b.rows, Row{Cells: b.labelRow(LabelCell(key)), Kind: RowData})
}

// emitSectionTree walks the account tree pre-order (p26.53) and emits the section's
// NESTED hierarchy, driven by the PRIMARY (selected as-of) column's leaf set: a
// PLACEHOLDER parent that has any in-section leaf beneath it in the primary column
// becomes a rolled-up SUBTOTAL row; each in-section LEAF (present in the primary column)
// becomes an account row. Every row carries one value cell per as-of column (looked up
// in that column's snapshot; an account absent from an older column shows a zero cell).
func (b *bsBuilder) emitSectionTree(
	children map[AccountID][]AccountID, roots []AccountID, isPlaceholder map[AccountID]bool,
	name map[AccountID]string, depth map[AccountID]int, kind sectionKind,
) {
	primLeaf := kind.leaf(b.snaps[0])

	// hasLeaf marks a node whose subtree carries an in-section leaf IN THE PRIMARY COLUMN
	// (so empty placeholder branches drop out). A leaf qualifies iff it is in primLeaf.
	hasLeaf := map[AccountID]bool{}
	var mark func(id AccountID) bool
	mark = func(id AccountID) bool {
		if !isPlaceholder[id] {
			_, ok := primLeaf[id]
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
			b.parentSubtotal(name[id], id, children, isPlaceholder, kind, depth[id]+1)
			for _, c := range children[id] {
				walk(c)
			}
			return
		}
		b.accountLine(id, primLeaf[id], kind, depth[id]+1)
	}
	for _, r := range roots {
		walk(r)
	}
}

// subtreeByCcy sums the per-currency native balances of every in-section LEAF beneath id
// in the given snapshot (id inclusive when it is itself a leaf) -- a placeholder parent's
// rolled figure for that column.
func subtreeByCcy(
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
// indent: one converted rollup cell per as-of column (blank native in detail mode). Not
// drillable (a rollup spans many leaves).
func (b *bsBuilder) parentSubtotal(
	nm string, id AccountID, children map[AccountID][]AccountID, isPlaceholder map[AccountID]bool,
	kind sectionKind, indent int,
) {
	if !b.detail {
		cells := []Cell{TextCell(nm)}
		for _, s := range b.snaps {
			conv, _ := b.convertAt(subtreeByCcy(id, children, isPlaceholder, kind.leaf(s)), s.asOf)
			cells = append(cells, MoneyCell(conv, b.convCcy()))
		}
		b.rows = append(b.rows, Row{Cells: cells, Indent: indent, Kind: RowSubtotal})
		return
	}
	// Detail (single as-of) mode: converted rollup, blank native.
	s := b.snaps[0]
	conv, _ := b.convertAt(subtreeByCcy(id, children, isPlaceholder, kind.leaf(s)), s.asOf)
	b.rows = append(b.rows, Row{
		Cells:  []Cell{TextCell(nm), TextCell(""), BlankMoneyCell(), MoneyCell(conv, b.convCcy())},
		Indent: indent,
		Kind:   RowSubtotal,
	})
}

// accountLine appends an asset/liability account leaf line at the given indent. Converted
// -only mode: one converted cell per as-of column, each drillable to that column's as-of
// (single-currency accounts only). Detail mode: one row per native currency (single as-of).
// `primLine` is the leaf in the primary column (its name + drill currency set).
func (b *bsBuilder) accountLine(id AccountID, primLine bsLine, kind sectionKind, indent int) {
	if !b.detail {
		cells := []Cell{TextCell(primLine.name)}
		for _, s := range b.snaps {
			line := kind.leaf(s)[id] // zero-value bsLine (empty byCcy) when absent this column.
			conv, _ := b.convertAt(line.byCcy, s.asOf)
			cell := MoneyCell(conv, b.convCcy())
			if d := b.accountDrillAll(id, line, s.asOf); d != nil {
				cell = cell.WithDrill(d)
			}
			cells = append(cells, cell)
		}
		b.rows = append(b.rows, Row{Cells: cells, Indent: indent, Kind: RowData})
		return
	}
	// Detail (single as-of) mode: one row per native currency, drill on the native cell.
	s := b.snaps[0]
	line := kind.leaf(s)[id]
	first := true
	for _, ccy := range sortedKeys(line.byCcy) {
		native := line.byCcy[ccy]
		conv := native
		if b.target != "" {
			conv, _ = b.tk.ConvertMinorAt(b.ctx, native, ccy, b.target, s.asOf)
		}
		nameCell := TextCell("")
		if first {
			nameCell = TextCell(primLine.name)
			first = false
		}
		nativeCell := MoneyCell(native, ccy)
		if d := b.accountDrill(id, ccy, s.asOf); d != nil {
			nativeCell = nativeCell.WithDrill(d)
		}
		b.rows = append(b.rows, Row{
			Cells:  []Cell{nameCell, TextCell(ccy), nativeCell, MoneyCell(conv, b.convCcyOr(ccy))},
			Indent: indent,
			Kind:   RowData,
		})
	}
}

// syntheticLine appends a net-asset line (no account; not drillable) whose per-column
// value comes from pick(snapshot). ofWhich renders it as an indented "of which"
// disclosure memo (net surplus to date).
func (b *bsBuilder) syntheticLine(key string, pick func(bsSnapshot) map[string]int64, ofWhich bool) {
	indent := 1
	if ofWhich {
		indent = 2
	}
	if !b.detail {
		cells := []Cell{LabelCell(key)}
		for _, s := range b.snaps {
			conv, _ := b.convertAt(pick(s), s.asOf)
			cells = append(cells, MoneyCell(conv, b.convCcy()))
		}
		b.rows = append(b.rows, Row{Cells: cells, Indent: indent, Kind: RowData})
		return
	}
	// Detail (single as-of) mode: one row per native currency.
	s := b.snaps[0]
	byCcy := pick(s)
	first := true
	for _, ccy := range sortedKeys(byCcy) {
		native := byCcy[ccy]
		conv := native
		if b.target != "" {
			conv, _ = b.tk.ConvertMinorAt(b.ctx, native, ccy, b.target, s.asOf)
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

// withoutLine appends the "net assets without donor restrictions" line. In the
// converted-only view each column's cell is derived as a RESIDUAL --
// Total NA(conv) - with(conv) - CTA(conv) - reconciling(conv) -- rather than converting
// the native without-map, so the net-asset section FOOTS EXACTLY in converted space:
// without + with (+ CTA + reconciling) == Total net assets per column, with no half-even
// drift from converting three sibling figures independently (the p15.5 "footing wins"
// rule, mirrored to the balance sheet). On column 1 this reproduces the historical figure
// byte-for-byte (NA - with, no residual). The `hasCTA` flag mirrors whether the CTA lines
// are emitted so the residual subtracts them iff they exist. Detail (per-currency) mode is
// unchanged: it renders the native withoutShown map, which already foots per currency by
// subtraction.
func (b *bsBuilder) withoutLine(key string, hasCTA bool) {
	if b.detail {
		b.syntheticLine(key, func(s bsSnapshot) map[string]int64 { return s.withoutShown }, false)
		return
	}
	cells := []Cell{LabelCell(key)}
	for _, s := range b.snaps {
		na, _ := b.convertAt(s.netAssetsTotal, s.asOf)
		with, _ := b.convertAt(s.withRestriction, s.asOf)
		conv := na - with
		if hasCTA {
			cta, _ := b.convertAt(s.cta, s.asOf)
			rec, _ := b.convertAt(s.reconciling, s.asOf)
			conv -= cta + rec
		}
		cells = append(cells, MoneyCell(conv, b.convCcy()))
	}
	b.rows = append(b.rows, Row{Cells: cells, Indent: 1, Kind: RowData})
}

// totalLine appends a SECTION total row (RowSectionTotal, p30.10): one converted total
// per as-of column (per-currency in detail).
func (b *bsBuilder) totalLine(key string, pick func(bsSnapshot) map[string]int64) {
	b.emphasized(key, pick, RowSectionTotal, 0)
}

// grandTotalLine appends the identity's right-hand grand total (L + NA).
func (b *bsBuilder) grandTotalLine(key string, pick func(bsSnapshot) map[string]int64) {
	b.emphasized(key, pick, RowTotal, 0)
}

func (b *bsBuilder) emphasized(key string, pick func(bsSnapshot) map[string]int64, kind RowKind, indent int) {
	if !b.detail {
		cells := []Cell{LabelCell(key)}
		for _, s := range b.snaps {
			conv, _ := b.convertAt(pick(s), s.asOf)
			cells = append(cells, MoneyCell(conv, b.convCcy()))
		}
		b.rows = append(b.rows, Row{Cells: cells, Indent: indent, Kind: kind})
		return
	}
	s := b.snaps[0]
	byCcy := pick(s)
	first := true
	for _, ccy := range sortedKeys(byCcy) {
		native := byCcy[ccy]
		conv := native
		if b.target != "" {
			conv, _ = b.tk.ConvertMinorAt(b.ctx, native, ccy, b.target, s.asOf)
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

// warningLine appends the D19 intercompany reconciling row (nonzero native residual): one
// converted residual per as-of column (per currency in detail). Only reached in the native
// (no-target) converted view; pick reads each column's residual.
func (b *bsBuilder) warningLine(key string, pick func(bsSnapshot) map[string]int64) {
	b.emphasized(key, pick, RowWarning, 0)
}

// labelRow builds a row's cells for a pure label heading (blank value columns).
func (b *bsBuilder) labelRow(label Cell) []Cell {
	if b.detail {
		return []Cell{label, TextCell(""), BlankMoneyCell(), BlankMoneyCell()}
	}
	cells := []Cell{label}
	for range b.snaps {
		cells = append(cells, BlankMoneyCell())
	}
	return cells
}

// convCcyOr returns the converted-column currency: the target, or ccy when no target
// is set (so a native-mode run mirrors the native value honestly).
func (b *bsBuilder) convCcyOr(ccy string) string {
	if b.target == "" {
		return ccy
	}
	return b.target
}

// accountDrill builds the p15.3d drill for one (account, currency) as-of balance at the
// given as-of date -- the trial-balance retrofit pattern.
func (b *bsBuilder) accountDrill(id AccountID, ccy, asOf string) *Drill {
	if id == 0 {
		return nil
	}
	return &Drill{
		Scope:      b.p.Scope,
		AccountIDs: []AccountID{id},
		Currency:   ccy,
		Mode:       DrillAsOf,
		AsOf:       asOf,
	}
}

// accountDrillAll builds a drill for a converted-only account cell at the given as-of. A
// single-currency account (in THAT column) drills to that currency; a multi-currency (or
// empty) account cell is left non-drillable.
func (b *bsBuilder) accountDrillAll(id AccountID, line bsLine, asOf string) *Drill {
	if id == 0 || len(line.byCcy) != 1 {
		return nil
	}
	for ccy := range line.byCcy {
		return b.accountDrill(id, ccy, asOf)
	}
	return nil
}

func (b *bsBuilder) table(dates []string) Table {
	return Table{Columns: b.columns(dates), Rows: b.rows}
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
// NA exactly.
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
// identity logic runs unchanged over a single fund's ledger.
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
// fund is RESTRICTED (non-empty Restriction), else an empty map. It mirrors the org-wide
// restrictedNetAssets narrowed to one fund.
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
// from inception.
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
// reconciling row is emitted, D19).
func hasNonzero(amts []CurAmt) bool {
	for _, a := range amts {
		if a.Minor != 0 {
			return true
		}
	}
	return false
}

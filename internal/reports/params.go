package reports

// Params are the resolved, validated report parameters a Run receives: the shared
// controls the params form collects (subsidiary scope, as-of / period, granularity,
// target currency), already parsed and defaulted by the web layer. A report reads
// only the params its ParamsSpec declares; the others hold zero values. The
// subsidiary scope is ALWAYS present (every report is scoped, D18) regardless of
// the spec.
type Params struct {
	// Scope is the subsidiary the report consolidates: this subsidiary plus ALL its
	// descendants (D18). It is always set (defaults to the user's default
	// subsidiary, else the root — full consolidation).
	Scope int64

	// AsOf is the balance-sheet as-of date (YYYY-MM-DD): cumulative balances to this
	// date. Meaningful when ParamsSpec.AsOf is set.
	AsOf string

	// From and To bound a period (YYYY-MM-DD, inclusive) for activity reports.
	// Meaningful when ParamsSpec.Period is set.
	From string
	To   string

	// Granularity is the period-column breakdown for comparative reports.
	// Meaningful when ParamsSpec.Granularity is set.
	Granularity Granularity

	// TargetCurrency is the ISO currency the report converts to (D12). Defaults to
	// the scope subsidiary's base currency. Meaningful when ParamsSpec.Currency is
	// set; otherwise reports render native currencies.
	TargetCurrency string

	// Lang is the request UI language (a catalog lang code, e.g. "en"/"es") a report
	// resolves STORED per-language names in (account_names, D5) — the ONE piece of
	// request context a report needs beyond the scope/dates that isn't carried on
	// ctx (the lang ctx key is web-private, and AGENTS "nothing but the actor rides
	// the context"). The web layer sets it from langOf(ctx); it defaults to "en"
	// (the report author reads it via LangOr so a zero Params still resolves names).
	Lang string

	// Detail requests an expanded per-currency breakdown (p15.4 balance sheet): the
	// empty string is the default (converted totals only, one column), "currency"
	// expands each section line into one row per native currency plus its converted
	// amount. Meaningful only when ParamsSpec.Detail is set; a report reads it via
	// DetailCurrency() so an unset value defaults to the converted-only view.
	Detail string

	// Account is the single account a report-specific "which account" control names
	// (p15.6 account ledger): the account whose register the report prints. Meaningful
	// only when ParamsSpec.Account is set; the empty value (0) means "no account
	// chosen" and the report returns an empty Table (the framework's nothing-to-show
	// rule), so a bare hit (the permission-matrix / scope-selector test) still renders
	// 200. Unlike Scope (always present, every report is scoped), Account is a
	// report-specific param the web layer parses only for a report whose spec declares
	// it, and validates against the real leaf-account set.
	Account int64

	// Fund is the single fund a report-specific "which fund" control names (p15.8 fund
	// balances & activity): the fund whose period statement the report prints. Meaningful
	// only when ParamsSpec.Fund is set; the empty value (0) means "no fund chosen" and the
	// fund report renders its LIST view (the fund roster) rather than a single-fund
	// statement. Mirrors Account: a report-specific param the web layer parses only for a
	// report whose spec declares it, validated against the real fund set (active, plus
	// closed when a closed fund is explicitly requested). Fund id 0 is never a valid
	// selection (it is the synthetic unrestricted group, which appears only in the list).
	Fund int64

	// Reconciliation is the single finalized reconciliation a report-specific "which
	// reconciliation" control names (p16.4 statement report): the recon whose statement
	// detail (statement info + included splits + opening/closing chain) the report
	// prints. Meaningful only when ParamsSpec.Reconciliation is set; the empty value (0)
	// means "no reconciliation chosen" and the report returns an empty Table (the
	// framework's nothing-to-show rule), so a bare hit still renders 200. Mirrors
	// Account/Fund/Program: a report-specific param the web layer parses only for a
	// report whose spec declares it (from either ?reconciliation= or ?recon=), validated
	// against the real finalized-recon set. It is NOT scoped by subsidiary -- a
	// reconciliation spans all funds AND subsidiaries (D13/D20), so Scope is inert on the
	// statement report.
	Reconciliation int64

	// Program is the single program a report-specific "which program" control names
	// (p15.10 program statement): the program whose subtree the report prints (that
	// program plus ALL its descendants, rolled up, D24). Meaningful only when
	// ParamsSpec.Program is set; the empty value (0) means "no program chosen" and the
	// program statement renders its COMPARATIVE view (every program in the tree as a
	// side-by-side column) rather than a single-program subtree. Mirrors Account/Fund:
	// a report-specific param the web layer parses only for a report whose spec declares
	// it, validated against the real program set.
	Program int64

	// ProgramScope is a GRANT-imposed program-subtree filter (p27.4): when the current
	// user holds a program-SCOPED report grant for this report's group, the web layer
	// resolves the granted program node's subtree (self + descendants, via
	// ProgramSubtree) into this set, and a PROGRAM-DIMENSIONED report restricts its
	// rows to splits whose program is in it. Empty/nil means NO grant filter (admin, or
	// an unscoped grant, or a non-program report) -- the report runs org-wide as before.
	// It is DISTINCT from Program (the user's own single-program SELECTION control): the
	// user may pick any program, but the grant clamps what they may actually see, so the
	// web layer intersects a user Program selection with this scope. Enforced in the
	// toolkit's program-keyed aggregation (a data-level filter, so subtotals/totals
	// reflect only the granted subtree), never by dropping rendered rows.
	ProgramScope []int64

	// Budget is the single budget a report-specific "which budget" control names
	// (p19.4 actuals-vs-budget + cashflow projection): the budget whose lines drive the
	// forecast/variance and the projected fund balances. Meaningful only when
	// ParamsSpec.Budget is set; the empty value (0) means "no budget chosen" and the
	// budget reports return an empty Table (the framework's nothing-to-show rule), so a
	// bare hit still renders 200. Mirrors Account/Fund/Program/Reconciliation: a
	// report-specific param the web layer parses only for a report whose spec declares
	// it, validated against the real budget set. When a budget IS chosen the web layer
	// also defaults From/To to the budget's own period (p19.4).
	Budget int64
}

// InProgramScope reports whether program id is visible under the grant's
// program-subtree filter (p27.4). An empty ProgramScope means NO filter (admin /
// unscoped grant / non-program report), so every program is visible. Otherwise only
// ids in the granted subtree are visible -- a data-level check the program-keyed
// toolkit methods apply to each raw split's program before aggregation, so a sibling
// subtree never contributes to a total.
func (p Params) InProgramScope(id int64) bool {
	if len(p.ProgramScope) == 0 {
		return true
	}
	for _, v := range p.ProgramScope {
		if v == id {
			return true
		}
	}
	return false
}

// DetailCurrency reports whether the per-currency detail toggle is on (Detail ==
// "currency"). A report gated by ParamsSpec.Detail branches on it to expand
// per-currency rows vs. showing only converted totals.
func (p Params) DetailCurrency() bool { return p.Detail == "currency" }

// LangOr returns p.Lang, or "en" when it is empty, so a report resolving stored
// per-language names (account_names) always has a valid catalog lang even for a
// zero-value Params (a test that builds Params by hand need not set Lang).
func (p Params) LangOr() string {
	if p.Lang == "" {
		return "en"
	}
	return p.Lang
}

// Granularity is the period-column breakdown a comparative activity report uses
// (income statement's monthly/quarterly columns, p15.5). Reports that don't offer
// comparative columns leave ParamsSpec.Granularity false and ignore it.
type Granularity int

const (
	// GranNone is a single aggregate column (no period breakdown). The default.
	GranNone Granularity = iota
	// GranMonth breaks the period into monthly columns.
	GranMonth
	// GranQuarter breaks the period into quarterly columns.
	GranQuarter
	// GranWeek breaks the period into ISO weeks (Monday-start). Used by the budget
	// toolkit's occurrence bucketing (p19.2); a week's bucket key is the date of the
	// Monday that starts it.
	GranWeek
	// GranYear breaks the period into calendar years. Used by the budget toolkit's
	// occurrence bucketing (p19.2); a year's bucket key is YYYY-01-01.
	GranYear
)

// String renders a Granularity as its stable query-param token (round-trips
// through the params form). Unknown values render as "none".
func (g Granularity) String() string {
	switch g {
	case GranMonth:
		return "month"
	case GranQuarter:
		return "quarter"
	case GranWeek:
		return "week"
	case GranYear:
		return "year"
	default:
		return "none"
	}
}

// ParseGranularity maps a query-param token to a Granularity, defaulting to
// GranNone for empty/unknown input (forgiving, like the money parsers).
func ParseGranularity(s string) Granularity {
	switch s {
	case "month":
		return GranMonth
	case "quarter":
		return GranQuarter
	case "week":
		return GranWeek
	case "year":
		return GranYear
	default:
		return GranNone
	}
}

// ParamsSpec declares WHICH shared params a report consumes, so the web layer
// renders only the relevant controls (an as-of report shows a single date; a
// period report shows from/to; a comparative report adds a granularity select; a
// converting report adds a target-currency select). The subsidiary scope selector
// is UNCONDITIONAL (not in the spec) — every report is scoped (D18), and the
// "scope selector on every report" test relies on it being always present.
type ParamsSpec struct {
	// AsOf: the report takes a single as-of date (cumulative balances). Mutually
	// typical-exclusive with Period, but not enforced here — a report may declare
	// neither (scope-only) or (unusually) both.
	AsOf bool
	// Period: the report takes a from/to date range (activity over a period).
	Period bool
	// Granularity: the report offers comparative period columns (needs Period).
	Granularity bool
	// Currency: the report converts to a chosen target currency (D12); the form
	// offers a currency select defaulting to the scope's base currency.
	Currency bool
	// CurrencyOptional: the report offers a target-currency select BUT defaults to
	// NATIVE (no conversion) -- the currency select carries a leading "— native —"
	// choice and the web layer leaves TargetCurrency EMPTY unless a currency is
	// explicitly picked (p26.54 program statement, whose default is native per-currency
	// rows by design). Distinct from Currency (which defaults the target to the scope
	// base, always converting). A report sets AT MOST one of Currency / CurrencyOptional.
	CurrencyOptional bool
	// Detail: the report offers a per-currency detail toggle (p15.4 balance sheet);
	// the form offers a "converted only" vs "per currency" select bound to
	// Params.Detail.
	Detail bool
	// Account: the report takes a single ACCOUNT (p15.6 account ledger); the form
	// offers a leaf-account select bound to Params.Account. It is a report-SPECIFIC
	// control (unlike the always-present subsidiary scope), so only a report whose
	// spec sets this parses/validates the account param and renders the selector.
	Account bool
	// Fund: the report takes a single FUND (p15.8 fund balances & activity); the form
	// offers a fund select bound to Params.Fund (with a "— all funds —" default that
	// yields the list view). Report-SPECIFIC, like Account.
	Fund bool
	// Program: the report takes a single PROGRAM (p15.10 program statement); the form
	// offers a program select bound to Params.Program (with an "— all programs —" default
	// that yields the comparative side-by-side view). Report-SPECIFIC, like Account/Fund.
	Program bool
	// Reconciliation: the report takes a single finalized RECONCILIATION (p16.4 statement
	// report); the form offers a finalized-recon select bound to Params.Reconciliation.
	// Report-SPECIFIC, like Account/Fund/Program. The scope selector still renders (every
	// report is scoped) but is INERT here -- a reconciliation spans all funds AND
	// subsidiaries (D13/D20), so the statement's included set never narrows by scope.
	Reconciliation bool
	// Budget: the report takes a single BUDGET (p19.4 actuals-vs-budget + cashflow
	// projection); the form offers a budget select bound to Params.Budget. Report-
	// SPECIFIC, like Account/Fund/Program/Reconciliation. When set the web layer also
	// defaults the period (From/To) to the selected budget's own period.
	Budget bool
}

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
)

// String renders a Granularity as its stable query-param token (round-trips
// through the params form). Unknown values render as "none".
func (g Granularity) String() string {
	switch g {
	case GranMonth:
		return "month"
	case GranQuarter:
		return "quarter"
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
	// Detail: the report offers a per-currency detail toggle (p15.4 balance sheet);
	// the form offers a "converted only" vs "per currency" select bound to
	// Params.Detail.
	Detail bool
	// Account: the report takes a single ACCOUNT (p15.6 account ledger); the form
	// offers a leaf-account select bound to Params.Account. It is a report-SPECIFIC
	// control (unlike the always-present subsidiary scope), so only a report whose
	// spec sets this parses/validates the account param and renders the selector.
	Account bool
}

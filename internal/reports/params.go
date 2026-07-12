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
}

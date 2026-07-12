package reports

import "cuento/internal/store"

// Toolkit is the computation context a Report.Run is handed: the read-only store
// plus the resolved Params for this run. In p15.1 it holds exactly that and
// exposes the store to reports so the framework is provably end-to-end (the smoke
// report reads a real balance through it). p15.2 grows the Toolkit with the
// Appendix-E computation methods — BalancesAsOf/Activity/Rollup/NetIncome/
// FundBalances/FunctionalMatrix/ProgramActivity/Group990/IntercompanyNet — layered
// over the p08.4 store queries (internal/store/balances.go) with D12 conversion,
// descendant-closure consolidation (D18), and D19 intercompany collapse. Reports
// call those methods instead of the raw store, so the conversion/consolidation/
// collapse logic lives in ONE place. The Toolkit never writes (rule 2): every
// method is a pure read.
type Toolkit struct {
	// store is the read funnel the toolkit methods (and, in p15.1, reports
	// directly) query. Unexported so reports go through toolkit methods; the smoke
	// report uses Store() until the Appendix-E methods exist.
	store *store.Store

	// Params is the resolved run context (scope, dates, granularity, target
	// currency). p15.2's methods read Scope/dates/TargetCurrency from here so a
	// report need not thread them through every call.
	Params Params

	// expCache memoizes currency minor-unit exponents for the duration of one report
	// run (compute.go's conversion path looks them up per cell). Currencies are
	// static reference data (D1), so a single fetch per code per run is safe.
	expCache map[string]int
}

// NewToolkit builds a Toolkit for one report run over st with the resolved params.
// The web layer constructs it per request after resolving the params form; p15.2's
// methods hang off it.
func NewToolkit(st *store.Store, p Params) *Toolkit {
	return &Toolkit{store: st, Params: p}
}

// Store returns the read-only store the toolkit wraps. It exists so the p15.1
// smoke report can read a real balance end-to-end BEFORE p15.2 adds the typed
// Appendix-E methods; real reports (p15.3+) will call those methods instead of
// reaching for the raw store. Kept as a narrow accessor rather than an exported
// field so the eventual migration to toolkit methods is a compile-time nudge.
func (tk *Toolkit) Store() *store.Store { return tk.store }

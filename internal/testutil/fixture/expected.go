package fixture

import "cuento/internal/ids"

// subCcyMap keys a per-scope currency list by subsidiary id. Named so the map
// literal in expectedFor (where the `ids` param shadows the ids package) can be
// written without naming the package.
type subCcyMap = map[ids.SubsidiaryID][]string

// Expected holds the hand-computed aggregates a golden/report test asserts
// against the p08.4 balance queries. Every number is NATIVE-CURRENCY (no FX
// conversion -- that is p14) and is derived independently from the transaction
// amounts in transactions.go (an out-of-band tally), NOT read back from a query,
// so TestFixtureKnownAggregates genuinely validates the queries rather than
// comparing a query to itself.
//
// The as-of date and activity period below are the dates the aggregates are
// computed at. Balances/fund balances/trial balances are AS OF AsOf; functional,
// program, and 990 activity span the whole ledger (ActivityFrom..ActivityTo) so
// the fixture's full R/E activity is captured in one window.
type Expected struct {
	// AsOf is the balance-sheet date for all balance/fund/trial-balance
	// aggregates (after every fixture transaction).
	AsOf string
	// ActivityFrom/ActivityTo bound the functional/program/990 activity window
	// (the whole fixture span).
	ActivityFrom string
	ActivityTo   string

	// TrialBalanceZero: at AsOf, each (subsidiary-scope, currency) trial balance
	// -- the sum over ALL splits in the scope's descendant closure -- is exactly
	// zero (double-entry, D2). Keyed by scope subsidiary id; the value lists the
	// currencies that must each sum to zero. The ROOT scope (full consolidation)
	// is the strongest: every currency nets to zero across the whole org.
	TrialBalanceCurrencies subCcyMap

	// AccountBalances: selected (account id, currency) -> signed minor-unit
	// balance at AsOf, ROOT scope (full consolidation). Net-debit signs (D2).
	AccountBalances []AccountBalance

	// FundBalances: (fund id, currency) -> asset-side unexpended balance at AsOf,
	// ROOT scope, INCLUDING the unrestricted group (fund id 0, D20).
	FundBalances []FundBalance

	// Functional: (expense account id, class, currency) -> activity over the
	// window, ROOT scope (D21). The functional matrix cells.
	Functional []FunctionalCell

	// Program: (program id, account id, currency) -> activity over the window,
	// ROOT scope (D24). Per-program activity (rollup-ready).
	Program []ProgramCell

	// Rollup990: effective-990-code rollups over the window, ROOT scope (D25),
	// INCLUDING the Unmapped bucket (code ""). Revenue (Part VIII) and expense
	// (Part IX) leaves rolled to their effective code (own or nearest ancestor).
	Rollup990 []Line990

	// IntercompanyNetZero: at AsOf, ROOT scope, the intercompany-flagged accounts
	// net to zero per currency (D19). Listed as the currencies that must be zero.
	IntercompanyNetCurrencies []string

	// UnmappedRevenueLeaf is the account id deliberately left without an effective
	// 990 code (Event Income) -- the single Z19 warning + the Unmapped bucket.
	UnmappedRevenueLeaf ids.AccountID

	// EditedMidAsOf is the RFC3339Nano timestamp of edit 1 to IDs.EditedTxn -- an
	// as-of read (store.TransactionAsOf) at this instant reconstructs the MIDDLE
	// (restricted, Educacion) state of the twice-edited transaction, distinct from
	// its create and final states. Populated by New (from the versions table)
	// since the deterministic clock is internal.
	EditedMidAsOf string

	// Rates is the FX seam (p14), populated ONLY after (*Fixture).ExtendRates(t) is
	// called; the zero value (empty) means the seam has not been applied and every
	// aggregate above is native-currency (the default state New leaves the fixture
	// in). It captures the deterministic monthly USD->MXN schedule and the
	// hand-computed CONVERTED aggregates p15 will assert against.
	Rates RatesExpected

	// Reconciliation is the p16 seam, populated ONLY after
	// (*Fixture).ExtendReconciliation(t) is called; the zero value (ID 0) means the
	// seam has not been applied and no reconciliation exists. It captures the
	// finalized 2026-05-31 Checking US (USD) reconciliation's expected state.
	Reconciliation ReconciliationExpected

	// SampleBudgetPlan is the p27.2 sample budget-PLAN seam (the NEW split-derived
	// model), populated ONLY after (*Fixture).ExtendSampleBudgetPlan(t) is called;
	// the zero value (Plan 0) means the seam has not been applied. It captures the
	// plan id + the natural report window (the span of its split dates) so the p27.3
	// cash-flow / variance report tests can drive the plan without re-deriving them.
	SampleBudgetPlan SampleBudgetPlanExpected
}

// SampleBudgetPlanExpected holds the p27.2 sample budget-PLAN seam's expectations:
// the plan id and the period the p27.3 budget reports run over (the span of the
// plan's split dates). Amounts are SYNTHETIC (rule 11) and asserted off the emitted
// report cells / reviewed goldens rather than pinned here.
type SampleBudgetPlanExpected struct {
	Plan   ids.BudgetPlanID
	From   string // earliest split date (the reports' default window start)
	To     string // latest split date (the reports' default window end)
	Splits int    // number of budget-splits the seam created
}

// ReconciliationExpected holds the p16 reconciliation seam's expectations: the
// finalized 2026-05-31 Checking US (USD) reconciliation. Opening is 0 (first
// finalized recon on the pair); StatementBalance == Opening + the net-debit sum of
// the ClearedCount cleared splits (the Finalize gate / Z9). UnclearedTxns are the
// two transactions deliberately left uncleared.
type ReconciliationExpected struct {
	ID               ids.ReconciliationID
	Account          ids.AccountID
	Currency         string
	StatementDate    string
	StatementBalance int64
	Opening          int64
	ClearedCount     int
	UnclearedTxns    []ids.TransactionID
}

// RatesExpected holds the p14 exchange-rate seam's expectations: the schedule
// ExtendRates loads and the converted values derived from it under an explicit
// rule. Converted values are UNROUNDED floats on purpose -- p15 owns the rounding
// mode + rules (D12), so pinning rounded int64 here would pre-commit p15 to
// reproduce this step's arithmetic. p15 reads these floats and applies its own
// rounding. The conversion RULE (documented in DECISIONS p14.1):
//
//   - report base currency USD; convert an MXN balance to USD with the CLOSING
//     rate at the balance-sheet date AsOf (2026-06-30);
//   - USD->MXN on-or-before 2026-06-30 resolves to the last scheduled point
//     (2026-06-01, rate 18.10); an MXN amount converts to USD by 1/18.10
//     (RateOn(MXN,USD,AsOf) is the reciprocal of the direct USD->MXN rate).
type RatesExpected struct {
	// Source is the source string every seam rate row carries.
	Source string
	// FirstDate/LastDate/Months bound the monthly schedule ExtendRates loads
	// (First 2025-01-01 @ 17.00, Last 2026-06-01 @ 18.10, 18 monthly points).
	FirstDate string
	LastDate  string
	Months    int
	// FirstRate/LastRate are the schedule endpoints (USD->MXN). Intermediate
	// points are the exact linear interpolation FirstRate + i*(LastRate-FirstRate)/(Months-1).
	FirstRate float64
	LastRate  float64
	// ClosingUSDPerMXN is the USD->MXN rate effective on-or-before AsOf (== LastRate,
	// the 2026-06-01 point). An MXN->USD conversion at AsOf uses 1/ClosingUSDPerMXN.
	ClosingUSDPerMXN float64
	// ConvertedFundBalances is the fixture's FundBalances converted to the USD
	// report base at the AsOf closing rate, UNROUNDED. USD funds pass through 1:1;
	// MXN funds convert by 1/ClosingUSDPerMXN. p15 rounds these.
	ConvertedFundBalances []ConvertedFundBalance
}

// ConvertedFundBalance is one fund's balance converted to the report base (USD),
// as an unrounded float in major units (dollars), plus the native minor-unit input
// and its native currency so p15 can re-derive and round independently.
type ConvertedFundBalance struct {
	Fund         ids.FundID
	NativeCcy    string
	NativeMinor  int64   // the native FundBalance amount (minor units)
	ConvertedUSD float64 // NativeMinor/100 converted to USD at AsOf, unrounded
}

// AccountBalance is one expected (account, currency) balance.
type AccountBalance struct {
	Account  ids.AccountID
	Currency string
	Amount   int64
}

// FundBalance is one expected (fund, currency) asset-side balance. Fund 0 is the
// unrestricted group.
type FundBalance struct {
	Fund     ids.FundID
	Currency string
	Amount   int64
}

// FunctionalCell is one expected (expense account, class, currency) activity cell.
type FunctionalCell struct {
	Account  ids.AccountID
	Class    string
	Currency string
	Amount   int64
}

// ProgramCell is one expected (program, account, currency) activity cell.
type ProgramCell struct {
	Program  ids.ProgramID
	Account  ids.AccountID
	Currency string
	Amount   int64
}

// Line990 is one expected effective-990-code rollup cell. Code "" is the Unmapped
// bucket (D25).
type Line990 struct {
	Code     string
	Currency string
	Amount   int64
}

// expectedFor builds Expected from the created ids. The literal amounts are the
// out-of-band tally of transactions.go (see the package doc); if a transaction
// amount changes, these change with it, and the invariant checks (trial balance
// zero, fund conservation) catch a mismatch immediately.
func expectedFor(ids IDs) Expected {
	return Expected{
		AsOf:         "2026-06-30",
		ActivityFrom: "2025-01-01",
		ActivityTo:   "2026-06-30",

		TrialBalanceCurrencies: subCcyMap{
			ids.Root: {"USD", "MXN"},
			ids.US:   {"USD"},
			ids.MX:   {"USD", "MXN"},
		},

		AccountBalances: []AccountBalance{
			// Assets.
			{ids.CheckingUS, "USD", 3_593_500},
			{ids.CheckingMX, "MXN", 39_500_000},
			{ids.Savings, "USD", 2_000_000},
			{ids.CashMXN, "MXN", 640_000},
			{ids.Building, "USD", 16_000_000},
			{ids.DueFromMX, "USD", 1_000_000},
			{ids.FXClearing, "USD", 974_000},
			{ids.FXClearing, "MXN", 500_000},
			// Liabilities / equity.
			{ids.CreditCard, "USD", -300_000},
			{ids.DueToIntl, "USD", -1_000_000},
			{ids.OpeningBalances, "USD", -18_700_000},
			{ids.OpeningBalances, "MXN", -31_500_000},
			// Revenue (credits, negative).
			{ids.Contributions, "USD", -5_275_000},
			{ids.GovernmentGrants, "MXN", -10_000_000},
			{ids.GovernmentGrants, "USD", -200_000},
			{ids.ProgramFees, "USD", -120_000},
			{ids.EventIncome, "USD", -300_000},
			// Expenses (debits, positive).
			{ids.Salaries, "USD", 1_650_000},
			{ids.ProgramSupplies, "USD", 210_000},
			{ids.ProgramSupplies, "MXN", 500_000},
			{ids.FoodPurchases, "MXN", 360_000},
			{ids.Occupancy, "USD", 305_000},
			{ids.Insurance, "USD", 60_000},
			{ids.BankFees, "USD", 2_500},
			{ids.EventCosts, "USD", 100_000},
		},

		FundBalances: []FundBalance{
			{ids.BecaAgua, "MXN", 9_700_000},
			{ids.BecaAgua, "USD", 50_000},
			{ids.BuildingFund, "USD", 5_000_000},
			{0, "MXN", 30_940_000}, // unrestricted
			{0, "USD", 18_517_500}, // unrestricted
		},

		Functional: []FunctionalCell{
			{ids.Salaries, "program", "USD", 1_650_000},
			{ids.ProgramSupplies, "program", "USD", 210_000},
			{ids.ProgramSupplies, "program", "MXN", 500_000},
			{ids.FoodPurchases, "program", "MXN", 360_000},
			{ids.Occupancy, "management", "USD", 305_000},
			{ids.Insurance, "management", "USD", 60_000},
			{ids.BankFees, "management", "USD", 2_500},
			{ids.EventCosts, "fundraising", "USD", 100_000},
		},

		Program: []ProgramCell{
			// Educacion.
			{ids.Educacion, ids.GovernmentGrants, "MXN", -10_000_000},
			{ids.Educacion, ids.GovernmentGrants, "USD", -200_000},
			{ids.Educacion, ids.ProgramFees, "USD", -120_000},
			{ids.Educacion, ids.ProgramSupplies, "MXN", 500_000},
			{ids.Educacion, ids.ProgramSupplies, "USD", 150_000},
			// Food Pantry.
			{ids.FoodPantry, ids.FoodPurchases, "MXN", 150_000},
			// General.
			{ids.General, ids.Contributions, "USD", -5_275_000},
			{ids.General, ids.EventIncome, "USD", -300_000},
			{ids.General, ids.Salaries, "USD", 1_650_000},
			{ids.General, ids.ProgramSupplies, "USD", 60_000},
			{ids.General, ids.FoodPurchases, "MXN", 210_000},
			{ids.General, ids.Occupancy, "USD", 305_000},
			{ids.General, ids.Insurance, "USD", 60_000},
			{ids.General, ids.BankFees, "USD", 2_500},
			{ids.General, ids.EventCosts, "USD", 100_000},
		},

		Rollup990: []Line990{
			// Part VIII revenue.
			{"VIII.1f", "USD", -5_275_000},  // Contributions
			{"VIII.1e", "MXN", -10_000_000}, // Government Grants
			{"VIII.1e", "USD", -200_000},
			{"VIII.2", "USD", -120_000}, // Program Service Fees
			{"", "USD", -300_000},       // Event Income -> Unmapped bucket
			// Part IX expenses.
			{"IX.7", "USD", 1_650_000}, // Salaries
			{"IX.16", "USD", 305_000},  // Occupancy
			{"IX.11g", "USD", 2_500},   // Bank Fees (leaf override)
			// IX.24e = Expenses parent, inherited by Program Supplies, Food
			// Purchases, Insurance, Event Costs (no own codes).
			{"IX.24e", "USD", 370_000}, // 210,000 + 60,000 + 100,000
			{"IX.24e", "MXN", 860_000}, // 500,000 + 360,000
		},

		IntercompanyNetCurrencies: []string{"USD"},
		UnmappedRevenueLeaf:       ids.EventIncome,

		// Rates is left ZERO here (the seam is opt-in): New does NOT load rates, so
		// the default fixture stays native-currency. (*Fixture).ExtendRates fills
		// this in place when a p15 test opts into conversion.
	}
}

// Package synth builds cuento's canonical SYNTHETIC dataset by CALLING THE STORE
// (the write funnel + every invariant, rule 2) -- never raw SQL. It is the ONE
// place the invented org/chart/funds/transactions and the four opt-in extension
// seams (rates, reconciliation, capital campaign, sample budget) are constructed,
// so both the test fixture (internal/testutil/fixture, which wraps this with the
// golden oracle) AND the `cuento demo` generator share exactly one tested builder.
//
// Real export data NEVER enters here (AGENTS rule 11): every value below is
// invented. Because it is imported by the production `cuento demo` path, this
// package imports NO `testing` -- every constructor returns an error rather than
// failing a *testing.T. The fixture package's New(t)/Extend*(t) are thin wrappers
// that call these and t.Fatalf on error; that indirection keeps `testing` out of
// the shipped binary while the golden oracle stays test-only.
//
// Determinism: Build takes a caller-supplied monotonic clock (see BuildClock) and
// fixed base dates, so a run is reproducible -- no time.Now, no network. The one
// non-reproducible surface is argon2id password salts (in the demo's users), which
// live OUTSIDE this package; callers that need byte-stability assert on data and
// counts, not file bytes.
package synth

import (
	"context"
	"time"

	"cuento/internal/ids"
	"cuento/internal/store"
)

// SystemActor is the seeded system user (id 1); the builder posts as it.
var SystemActor = store.Actor{ID: 1}

// BaseClock is the deterministic start instant. BuildClock advances a monotonic
// clock from here so every change row gets a distinct, increasing timestamp (the
// twice-edited transaction's edits must be orderable for as-of tests).
var BaseClock = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// BuildClock returns a fresh monotonic clock closure starting at BaseClock: each
// call returns a strictly-increasing instant one second after the last. Both the
// fixture and the demo generator install it via store.WithClock so successive
// writes get orderable timestamps without any wall-clock dependency.
func BuildClock() func() time.Time {
	var tick int64
	return func() time.Time {
		tick++
		return BaseClock.Add(time.Duration(tick) * time.Second)
	}
}

// Seeded roots keep their migration ids.
const (
	seedRootSub     ids.SubsidiaryID = 1 // "Organization" (USD), renamed below
	seedRootProgram int64            = 1 // "General"
)

// IDs holds every entity id the builder creates, so callers (the fixture, the demo
// generator, the p14/p16 seams) can refer to them without re-deriving. Seeded roots
// keep their migration ids (subsidiary 1, program 1).
type IDs struct {
	// Subsidiaries.
	Root ids.SubsidiaryID // renamed seed: "Rio Verde Internacional" (USD)
	US   ids.SubsidiaryID // "RV Estados Unidos" (USD)
	MX   ids.SubsidiaryID // "RV Mexico" (MXN)

	// Programs.
	General    ids.ProgramID // seeded root
	Educacion  ids.ProgramID
	FoodPantry ids.ProgramID

	// Asset accounts.
	CheckingUS ids.AccountID
	CheckingMX ids.AccountID
	Savings    ids.AccountID
	CashMXN    ids.AccountID
	Building   ids.AccountID
	DueFromMX  ids.AccountID // intercompany
	FXClearing ids.AccountID

	// Liability accounts.
	CreditCard ids.AccountID
	DueToIntl  ids.AccountID // intercompany

	// Equity accounts.
	OpeningBalances ids.AccountID

	// Revenue accounts (placeholder parent + leaves).
	Revenue          ids.AccountID // placeholder
	Contributions    ids.AccountID // VIII.1f
	GovernmentGrants ids.AccountID // VIII.1e
	ProgramFees      ids.AccountID // VIII.2, default program Educacion
	EventIncome      ids.AccountID // DELIBERATELY UNMAPPED (Z19 + Unmapped bucket)

	// Expense accounts (placeholder parent + leaves).
	Expenses        ids.AccountID // placeholder, code IX.24e (inherited by leaves)
	Salaries        ids.AccountID // default class program
	ProgramSupplies ids.AccountID // default class program, default program Educacion
	FoodPurchases   ids.AccountID // default class program, default program Food Pantry
	Occupancy       ids.AccountID // IX.16 own code, class management
	Insurance       ids.AccountID // class management
	BankFees        ids.AccountID // IX.11g LEAF OVERRIDE, class management
	EventCosts      ids.AccountID // class fundraising

	// Funds.
	BecaAgua     ids.FundID // purpose, subs {MX, US}, program Educacion
	BuildingFund ids.FundID // purpose, subs {US}, no program

	// Transactions of special interest to as-of / audit tests.
	EditedTxn  ids.TransactionID // edited twice
	DeletedTxn ids.TransactionID // soft-deleted

	// The two Checking US (USD) transactions deliberately LEFT UNCLEARED by the
	// ExtendReconciliation seam: the 2026-05-25 May rent and the 2026-06-10 June
	// donation. Captured so the seam clears the complement deterministically (no
	// id/amount hardcoding) and tests can assert them uncleared.
	MayRentTxn      ids.TransactionID
	JuneDonationTxn ids.TransactionID

	// CheckingUSRecon is the finalized 2026-05-31 Checking US (USD) reconciliation
	// ExtendReconciliation creates -- zero until that seam is called.
	CheckingUSRecon ids.ReconciliationID

	// --- Capital-campaign seam (ExtendCapitalCampaign, opt-in) -----------------
	// Zero until ExtendCapitalCampaign is called. A restricted CAPITAL CAMPAIGN
	// fund whose revenue is partly deployed into a LAND purchase and a FIXED-ASSET
	// (construction) purchase across several quarters, leaving an unspent restricted
	// (spendable) balance -- the Capital Campaign report's data.
	Campaign     ids.FundID    // the restricted capital-campaign fund
	FixedAssets  ids.AccountID // placeholder parent for the campaign's capital accounts
	CampaignLand ids.AccountID // "Land" leaf under Fixed Assets
	Construction ids.AccountID // fixed-asset leaf under Fixed Assets
	ConstrLoan   ids.AccountID // liability: a construction loan that financed a purchase

	// --- Sample budget-PLAN seam (ExtendSampleBudgetPlan, opt-in) --------------
	// Zero until ExtendSampleBudgetPlan is called. A SAMPLE budget PLAN (the p27.2
	// split-derived model: a plan + several PROJECTED, dated budget-splits across
	// >=2 programs, incl. R/E legs AND an open_item A/R leg, on varied dates) so the
	// p27.3 cash-flow / variance reports have something to show. (The old schedule-
	// based sample budget was retired in p27.3.)
	SampleBudgetPlan ids.BudgetPlanID // the sample budget plan
}

// ptr returns a pointer to v (concise optional-field construction).
func ptr[T any](v T) *T { return &v }

// notesPtr maps a synthetic notes string to the store's optional *string: "" -> nil
// (no note, stored NULL), else a pointer to the text (p28.7).
func notesPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Build constructs the canonical synthetic dataset (Appendix D) into the store, in
// dependency order, and returns the ids. All amounts are minor units (cents; USD
// and MXN both have exponent 2). Net-debit signs (D2): asset/expense debits +,
// revenue/liability/equity credits -.
//
// It writes exclusively through the store (rule 2); every scenario's per-fund and
// overall zero-sum is verified by the store on write, so any invariant/API drift
// surfaces here as an error rather than silent corruption.
func Build(ctx context.Context, s *store.Store) (IDs, error) {
	var ids IDs
	if err := buildOrg(ctx, s, &ids); err != nil {
		return ids, err
	}
	if err := buildAccounts(ctx, s, &ids); err != nil {
		return ids, err
	}
	if err := buildFunds(ctx, s, &ids); err != nil {
		return ids, err
	}
	if err := buildTransactions(ctx, s, &ids); err != nil {
		return ids, err
	}
	return ids, nil
}

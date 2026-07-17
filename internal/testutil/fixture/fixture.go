// Package fixture builds cuento's canonical synthetic test fixture (PLAN
// Appendix D) -- the ONE dataset CI, goldens, and integrity tests use -- by wrapping
// the shared, non-test builder in internal/synth with the golden ORACLE (the
// hand-computed Expected aggregates). Real export data NEVER enters here (AGENTS
// rule 11): every value is invented (see internal/synth).
//
// Why a subpackage of testutil (and not testutil itself, as PLAN p09.1's text says):
// the store's INTERNAL test files (package store) import testutil for
// NewDB/AssertVersioned. If testutil imported store, the store test binary would be
// store(test) -> testutil -> store, an import cycle Go forbids for internal test
// packages. So the store-importing builder lives in this leaf subpackage; testutil
// stays store-free. Recorded as the p09.1 DECISIONS deviation.
//
// Construction split (p26.81): the DATA-BUILDING half (org/accounts/funds/txns + the
// four Extend* seams) moved to the non-test package internal/synth so the production
// `cuento demo` generator can share the exact same tested builder without linking
// `testing`. THIS package keeps the test-only ORACLE (Expected) and thin New(t) /
// Extend*(t) wrappers that call synth and t.Fatalf on error. IDs is a type alias to
// synth.IDs, so every existing `fixture.IDs` / `f.IDs.*` reference is unchanged and
// `make golden` stays byte-identical -- the proof the extraction is faithful.
//
// The seams ExtendRates/ExtendReconciliation/ExtendCapitalCampaign/ExtendSampleBudget
// are OPT-IN: New does NOT call them, so the default fixture stays native-currency
// with no recon/campaign/budget and every existing golden/tally is untouched.
package fixture

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"cuento/internal/db"
	"cuento/internal/store"
	"cuento/internal/synth"
)

// IDs is the entity-id set the builder produces. It aliases synth.IDs so callers can
// keep referring to fixture.IDs while the construction lives in the shared package.
type IDs = synth.IDs

// Fixture is the built synthetic dataset: the db, a store over it, the entity ids,
// and the hand-computed expected aggregates.
type Fixture struct {
	DB       *sql.DB
	Store    *store.Store
	IDs      IDs
	Expected Expected
}

// New constructs Appendix D exactly (via synth.Build) and returns the migrated db +
// store + ids + expected aggregates. It fails the test on any error (a fixture that
// cannot be built is a test-infrastructure bug, not a case to handle).
func New(t *testing.T) *Fixture {
	t.Helper()

	sqldb := newDB(t)

	// synth.BuildClock is a monotonic deterministic clock: each write() reads now()
	// once for its change row, so successive builder calls get strictly increasing
	// timestamps. The twice-edited txn thus has orderable edits.
	s := store.New(sqldb, store.WithClock(synth.BuildClock()))
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	ids, err := synth.Build(ctx, s)
	if err != nil {
		t.Fatalf("fixture: build: %v", err)
	}

	exp := expectedFor(ids)
	// The twice-edited transaction's whole point is a recoverable MIDDLE state for
	// as-of tests. The clock closure is internal (a caller cannot reconstruct the
	// instant between the two edits), so expose edit 1's valid_from here: an as-of
	// read at this timestamp reconstructs the middle (restricted, Educacion) state,
	// distinct from both the create and the final state.
	exp.EditedMidAsOf = editOneValidFrom(t, sqldb, ids.EditedTxn)

	return &Fixture{
		DB:       sqldb,
		Store:    s,
		IDs:      ids,
		Expected: exp,
	}
}

// editOneValidFrom returns the valid_from of the FIRST update (edit 1) to the
// twice-edited transaction's header -- the timestamp an as-of test uses to pick the
// middle state. Header version rows are ordered create, update(edit1), update(edit2)
// by append id; the first op='update' row is edit 1.
func editOneValidFrom(t *testing.T, sqldb *sql.DB, txnID int64) string {
	t.Helper()
	var validFrom string
	err := sqldb.QueryRow(
		`SELECT valid_from
		   FROM transactions_versions
		  WHERE entity_id = ? AND op = 'update'
		  ORDER BY id ASC
		  LIMIT 1`, txnID,
	).Scan(&validFrom)
	if err != nil {
		t.Fatalf("fixture: middle-edit timestamp for txn %d: %v", txnID, err)
	}
	return validFrom
}

// newDB returns a migrated, isolated temp-file database (the same shape
// testutil.NewDB gives store tests, duplicated here to keep testutil store-free and
// this subpackage self-contained).
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cuento-fixture-*.db")
	if err != nil {
		t.Fatalf("fixture: temp file: %v", err)
	}
	path := f.Name()
	_ = f.Close()
	if err := db.Migrate(context.Background(), path); err != nil {
		t.Fatalf("fixture: migrate: %v", err)
	}
	sqldb, err := db.Open(path)
	if err != nil {
		t.Fatalf("fixture: open: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	return sqldb
}

// ExtendRates is the p14 seam wrapper: it loads the deterministic monthly USD->MXN
// rate schedule (synth.ExtendRates) and fills f.Expected.Rates with the schedule
// metadata + the CONVERTED fund balances p15 asserts against. It is OPT-IN: New does
// not call it, so the default fixture stays native-currency.
//
// Only USD->MXN rows are stored (18 points). MXN->USD conversions use the reciprocal
// (RateOn's fallback), exercising that path in p15 for free.
func (f *Fixture) ExtendRates(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	if err := synth.ExtendRates(ctx, f.Store); err != nil {
		t.Fatalf("fixture: ExtendRates: %v", err)
	}

	// The closing USD->MXN rate on-or-before AsOf (2026-06-30) is the last scheduled
	// point (2026-06-01 == LastRate); an MXN balance converts to USD by 1/LastRate.
	closing := synth.LastRate
	converted := make([]ConvertedFundBalance, 0, len(f.Expected.FundBalances))
	for _, fb := range f.Expected.FundBalances {
		major := float64(fb.Amount) / 100.0 // both USD and MXN have exponent 2
		usd := major
		if fb.Currency == "MXN" {
			usd = major / closing // reciprocal of the direct USD->MXN rate
		}
		converted = append(converted, ConvertedFundBalance{
			Fund:         fb.Fund,
			NativeCcy:    fb.Currency,
			NativeMinor:  fb.Amount,
			ConvertedUSD: usd,
		})
	}

	f.Expected.Rates = RatesExpected{
		Source:                synth.RatesSource,
		FirstDate:             "2025-01-01",
		LastDate:              "2026-06-01",
		Months:                synth.RateMonths,
		FirstRate:             synth.FirstRate,
		LastRate:              synth.LastRate,
		ClosingUSDPerMXN:      closing,
		ConvertedFundBalances: converted,
	}
}

// ExtendReconciliation is the p16 seam wrapper: it finalizes the 2026-05-31 Checking
// US (USD) reconciliation (synth.ExtendReconciliation) over the account's restricted
// AND unrestricted splits, leaving EXACTLY the two 2026-05-25 / 2026-06-10 items
// uncleared, then records the expected state. It is OPT-IN: New does not call it.
func (f *Fixture) ExtendReconciliation(t *testing.T) {
	t.Helper()
	ctx := store.WithActor(context.Background(), synth.SystemActor)

	cleared, err := synth.ExtendReconciliation(ctx, f.Store, f.DB, &f.IDs)
	if err != nil {
		t.Fatalf("fixture: ExtendReconciliation: %v", err)
	}

	f.Expected.Reconciliation = ReconciliationExpected{
		ID:               f.IDs.CheckingUSRecon,
		Account:          f.IDs.CheckingUS,
		Currency:         "USD",
		StatementDate:    "2026-05-31",
		StatementBalance: synth.ReconStatementBalance,
		Opening:          0,
		ClearedCount:     cleared,
		UnclearedTxns:    []int64{f.IDs.MayRentTxn, f.IDs.JuneDonationTxn},
	}
}

// Package fixture builds cuento's canonical synthetic test fixture (PLAN
// Appendix D) -- the ONE dataset CI, goldens, and integrity tests use. Real
// export data NEVER enters here (AGENTS rule 11): every value below is invented.
//
// Why a subpackage of testutil (and not testutil itself, as PLAN p09.1's text
// says): the store's INTERNAL test files (package store) import testutil for
// NewDB/AssertVersioned. If testutil imported store, the store test binary would
// be store(test) -> testutil -> store, an import cycle Go forbids for internal
// test packages. So the store-importing builder lives in this leaf subpackage;
// testutil stays store-free. Recorded as the p09.1 DECISIONS deviation.
//
// DEFERRALS (tables do not exist yet -- do NOT build here):
//   - exchange_rates (p14): every expected aggregate below is NATIVE-CURRENCY
//     (no conversion). p14 EXTENDS this fixture with monthly USD/MXN rates
//     (synthetic drift 17.00 -> 18.10, 2025-01 .. 2026-06) via ExtendRates once
//     the table exists.
//   - the finalized 2026-05-31 Checking US reconciliation (p16): p16 EXTENDS this
//     fixture via ExtendReconciliation. The transactions are designed so the
//     reconciliation can be added later without renumbering (Checking US carries
//     both restricted and unrestricted splits and leaves two uncleared).
//
// The seams ExtendRates/ExtendReconciliation are documented no-op-today hooks so
// p14/p16 slot in without reshaping New.
package fixture

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"cuento/internal/db"
	"cuento/internal/store"
)

// systemActor is the seeded system user (id 1); the fixture posts as it.
var systemActor = store.Actor{ID: 1}

// baseClock is the deterministic start instant. New advances a monotonic clock
// from here so every change row gets a distinct, increasing timestamp (the
// twice-edited transaction's edits must be orderable for as-of tests).
var baseClock = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// IDs holds every entity id the fixture creates, so tests (and p14/p16
// extensions) can refer to them without re-deriving. Seeded roots keep their
// migration ids (subsidiary 1, program 1).
type IDs struct {
	// Subsidiaries.
	Root int64 // renamed seed: "Rio Verde Internacional" (USD)
	US   int64 // "RV Estados Unidos" (USD)
	MX   int64 // "RV Mexico" (MXN)

	// Programs.
	General    int64 // seeded root
	Educacion  int64
	FoodPantry int64

	// Asset accounts.
	CheckingUS int64
	CheckingMX int64
	Savings    int64
	CashMXN    int64
	Building   int64
	DueFromMX  int64 // intercompany
	FXClearing int64

	// Liability accounts.
	CreditCard int64
	DueToIntl  int64 // intercompany

	// Equity accounts.
	OpeningBalances int64

	// Revenue accounts (placeholder parent + leaves).
	Revenue          int64 // placeholder
	Contributions    int64 // VIII.1f
	GovernmentGrants int64 // VIII.1e
	ProgramFees      int64 // VIII.2, default program Educacion
	EventIncome      int64 // DELIBERATELY UNMAPPED (Z19 + Unmapped bucket)

	// Expense accounts (placeholder parent + leaves).
	Expenses        int64 // placeholder, code IX.24e (inherited by leaves)
	Salaries        int64 // default class program
	ProgramSupplies int64 // default class program, default program Educacion
	FoodPurchases   int64 // default class program, default program Food Pantry
	Occupancy       int64 // IX.16 own code, class management
	Insurance       int64 // class management
	BankFees        int64 // IX.11g LEAF OVERRIDE, class management
	EventCosts      int64 // class fundraising

	// Funds.
	BecaAgua     int64 // purpose, subs {MX, US}, program Educacion
	BuildingFund int64 // purpose, subs {US}, no program

	// Transactions of special interest to as-of / audit tests.
	EditedTxn  int64 // edited twice
	DeletedTxn int64 // soft-deleted
}

// Fixture is the built synthetic dataset: the db, a store over it, the entity
// ids, and the hand-computed expected aggregates.
type Fixture struct {
	DB       *sql.DB
	Store    *store.Store
	IDs      IDs
	Expected Expected
}

// New constructs Appendix D exactly and returns the migrated db + store + ids +
// expected aggregates. It fails the test on any error (a fixture that cannot be
// built is a test-infrastructure bug, not a case to handle).
func New(t *testing.T) *Fixture {
	t.Helper()

	sqldb := newDB(t)

	// Monotonic deterministic clock: each write() reads now() once for its
	// change row, so successive builder calls get strictly increasing
	// timestamps. The twice-edited txn thus has orderable edits.
	var tick int64
	clock := func() time.Time {
		tick++
		return baseClock.Add(time.Duration(tick) * time.Second)
	}
	s := store.New(sqldb, store.WithClock(clock))
	ctx := store.WithActor(context.Background(), systemActor)

	ids := build(t, ctx, s)

	exp := expectedFor(ids)
	// The twice-edited transaction's whole point is a recoverable MIDDLE state
	// for as-of tests. The clock closure is internal (a caller cannot reconstruct
	// the instant between the two edits), so expose edit 1's valid_from here: an
	// as-of read at this timestamp reconstructs the middle (restricted, Educacion)
	// state, distinct from both the create and the final state.
	exp.EditedMidAsOf = editOneValidFrom(t, sqldb, ids.EditedTxn)

	return &Fixture{
		DB:       sqldb,
		Store:    s,
		IDs:      ids,
		Expected: exp,
	}
}

// editOneValidFrom returns the valid_from of the FIRST update (edit 1) to the
// twice-edited transaction's header -- the timestamp an as-of test uses to pick
// the middle state. Header version rows are ordered create, update(edit1),
// update(edit2) by append id; the first op='update' row is edit 1.
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
// testutil.NewDB gives store tests, duplicated here to keep testutil store-free
// and this subpackage self-contained).
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

// ExtendRates is the p14 seam. Today it is a no-op: exchange_rates does not
// exist and every expected aggregate is native-currency. p14 fills it (monthly
// USD/MXN rates 17.00 -> 18.10) and adds converted expectations THERE.
func (f *Fixture) ExtendRates(_ *testing.T) {}

// ExtendReconciliation is the p16 seam. Today a no-op: reconciliations /
// splits.reconciliation_id do not exist. p16 fills it (finalize the 2026-05-31
// Checking US reconciliation over the restricted + unrestricted splits, leaving
// two uncleared) without renumbering transactions.
func (f *Fixture) ExtendReconciliation(_ *testing.T) {}

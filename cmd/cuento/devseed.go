package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"cuento/internal/db"
	"cuento/internal/store"
)

// devseed is a LOCAL-ONLY developer helper: it seeds SYNTHETIC sample data into an
// existing db so the dev server (:3390) can exercise features that need data the
// real import does not produce. Today it seeds a SAMPLE BUDGET (a budget + several
// budget lines) so the budget-group reports (actuals_vs_budget / cashflow_projection)
// have something to render; more `devseed <thing>` subcommands can follow.
//
// It is built on the STORE (the write funnel + invariants, rule 2) -- NOT raw SQL --
// so it stays aligned with schema changes automatically. All amounts are SYNTHETIC
// round figures (rule 11): no real value is embedded. It resolves the accounts /
// programs / subsidiary / schedules it needs DYNAMICALLY from whatever the db already
// contains (no hardcoded real names), so it works against any db -- the dev.db, a
// scaffold, or a fresh migrate. It is idempotent per db (skips if the sample budget
// already exists), so `make dev-db` reruns and standalone reruns never pile up
// duplicates.
//
// It is a dev tool, so it is NOT wired into any deployed path; `usage()` lists it but
// production never runs it.

// devseedBudgetName is the name devseed gives its sample budget. The idempotency
// guard keys on it, so a rerun is a no-op.
const devseedBudgetName = "Sample Operating Budget"

// devseedCmd implements `cuento devseed <thing> [-db PATH]`. Currently the only
// thing is `budget`.
func devseedCmd(args []string) error {
	if len(args) == 0 {
		devseedUsage()
		return errors.New("devseed: a target is required (e.g. budget)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "budget":
		return devseedBudgetCmd(rest)
	default:
		devseedUsage()
		return fmt.Errorf("devseed: unknown target %q", sub)
	}
}

func devseedUsage() {
	fmt.Fprintf(stdout, "usage: cuento devseed <target> [-db PATH]\n\ntargets:\n"+
		"  budget    seed a SYNTHETIC sample budget + lines (for the budget reports)\n")
}

// devseedBudgetCmd migrates the db (so the seeded schedules exist), opens the store,
// and seeds the sample budget. It migrates FIRST so the single command is
// self-sufficient against a db built before the schedule-seed migration landed.
func devseedBudgetCmd(args []string) error {
	fs := flag.NewFlagSet("devseed budget", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Migrate first: the sample budget resolves schedules seeded by the schedule
	// migrations, which a db built before them lacks until migrated.
	if err := db.Migrate(ctx, *dbPath); err != nil {
		return fmt.Errorf("devseed budget: migrate: %w", err)
	}

	st, closeFn, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer closeFn()

	ctx = store.WithActor(ctx, systemActor)
	created, err := seedSampleBudget(ctx, st)
	if err != nil {
		return fmt.Errorf("devseed budget: %w", err)
	}
	if !created {
		fmt.Fprintf(stdout, "devseed budget: %q already exists in %s (no-op)\n", devseedBudgetName, *dbPath)
		return nil
	}
	fmt.Fprintf(stdout, "devseed budget: created %q in %s\n", devseedBudgetName, *dbPath)
	return nil
}

// seedSampleBudget creates a synthetic sample budget with several lines against
// whatever accounts/programs/funds/subsidiaries the db already has. It returns
// created=false (and writes nothing) when the sample budget already exists. Every
// reference is resolved DYNAMICALLY -- no real names are hardcoded (rule 11) -- and a
// db missing the minimum shape (a subsidiary, a program, a revenue leaf, an expense
// leaf, a seeded schedule) is a clear error rather than a partial seed.
func seedSampleBudget(ctx context.Context, st *store.Store) (bool, error) {
	// Idempotency: skip if the sample budget already exists.
	budgets, err := st.ListBudgets(ctx)
	if err != nil {
		return false, fmt.Errorf("list budgets: %w", err)
	}
	for _, b := range budgets {
		if b.Name == devseedBudgetName {
			return false, nil
		}
	}

	// Resolve the pieces a budget line needs from the live db.
	subID, err := firstSubsidiary(ctx, st)
	if err != nil {
		return false, err
	}
	progID, err := firstProgram(ctx, st)
	if err != nil {
		return false, err
	}
	revAcct, err := firstLeafAccount(ctx, st, "revenue")
	if err != nil {
		return false, err
	}
	expAcct, err := firstLeafAccount(ctx, st, "expense")
	if err != nil {
		return false, err
	}
	// A restricted fund is optional (a db may have none); nil = unrestricted line.
	var fundPtr *int64
	if fundID, ok, err := firstFund(ctx, st); err != nil {
		return false, err
	} else if ok {
		fundPtr = &fundID
	}

	// The seeded common schedules (p26.28 + p26.79), resolved by name. A monthly and
	// a biweekly cadence give the two lines distinct occurrence patterns; both are
	// present after the migrations run.
	monthlySched, err := scheduleByName(ctx, st, "Monthly (1st)")
	if err != nil {
		return false, err
	}
	biweeklySched, err := scheduleByName(ctx, st, "Biweekly")
	if err != nil {
		return false, err
	}

	budgetID, err := st.CreateBudget(ctx, store.BudgetInput{
		Name:        devseedBudgetName,
		PeriodStart: "2026-01-01",
		PeriodEnd:   "2026-12-31",
		Notes:       "Synthetic sample operating budget (cuento devseed).",
	})
	if err != nil {
		return false, fmt.Errorf("create budget: %w", err)
	}

	// Two lines: a revenue line (monthly) and an expense line (biweekly). Amounts are
	// SYNTHETIC round figures. Each line's currency is its account's default currency,
	// so it always exists in `currencies`.
	type line struct {
		account int64
		fund    *int64
		amount  int64
		sched   int64
	}
	lines := []line{
		{revAcct.id, fundPtr, 500_000, monthlySched},
		{expAcct.id, fundPtr, 180_000, biweeklySched},
	}
	for _, ln := range lines {
		ccy := revAcct.currency
		if ln.account == expAcct.id {
			ccy = expAcct.currency
		}
		if _, err := st.CreateBudgetLine(ctx, budgetID, store.BudgetLineInput{
			SubsidiaryID: subID,
			AccountID:    ln.account,
			FundID:       ln.fund,
			ProgramID:    progID,
			Amount:       ln.amount,
			Currency:     ccy,
			ScheduleID:   ln.sched,
		}); err != nil {
			return false, fmt.Errorf("create budget line (account %d): %w", ln.account, err)
		}
	}
	return true, nil
}

// leafAccount is a resolved leaf account: its id and default currency.
type leafAccount struct {
	id       int64
	currency string
}

// firstSubsidiary returns the id of the first subsidiary in the tree (the root),
// erroring on an empty tree.
func firstSubsidiary(ctx context.Context, st *store.Store) (int64, error) {
	tree, err := st.SubTree(ctx)
	if err != nil {
		return 0, fmt.Errorf("sub tree: %w", err)
	}
	if len(tree) == 0 {
		return 0, errors.New("no subsidiaries in db")
	}
	return tree[0].ID, nil
}

// firstProgram returns the id of the first active program, erroring if none exists
// (a budget line requires a program, Z15).
func firstProgram(ctx context.Context, st *store.Store) (int64, error) {
	tree, err := st.ProgramTree(ctx)
	if err != nil {
		return 0, fmt.Errorf("program tree: %w", err)
	}
	for _, p := range tree {
		if p.Active != 0 {
			return p.ID, nil
		}
	}
	return 0, errors.New("no active program in db (a budget line needs one)")
}

// firstFund returns the id of the first active fund, if any (ok=false when the db has
// no fund -- the line is then unrestricted).
func firstFund(ctx context.Context, st *store.Store) (int64, bool, error) {
	funds, err := st.ListFunds(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("list funds: %w", err)
	}
	for _, fnd := range funds {
		if fnd.Active != 0 {
			return fnd.ID, true, nil
		}
	}
	return 0, false, nil
}

// firstLeafAccount returns the first ACTIVE LEAF account of the given type (revenue
// or expense -- the only types a budget line accepts). A leaf is a row that is no
// other row's parent. Errors if none exists.
func firstLeafAccount(ctx context.Context, st *store.Store, typ string) (leafAccount, error) {
	tree, err := st.Tree(ctx, "en", nil)
	if err != nil {
		return leafAccount{}, fmt.Errorf("account tree: %w", err)
	}
	isParent := make(map[int64]bool, len(tree))
	for _, r := range tree {
		if r.ParentID.Valid {
			isParent[r.ParentID.Int64] = true
		}
	}
	for _, r := range tree {
		if r.Type != typ || r.Active == 0 || isParent[r.ID] {
			continue
		}
		acct, err := st.GetAccount(ctx, r.ID)
		if err != nil {
			return leafAccount{}, fmt.Errorf("get account %d: %w", r.ID, err)
		}
		return leafAccount{id: r.ID, currency: acct.DefaultCurrency}, nil
	}
	return leafAccount{}, fmt.Errorf("no active leaf %s account in db", typ)
}

// scheduleByName resolves a seeded schedule by name, erroring if absent (the
// migrations seed it, so absence means the db was not migrated).
func scheduleByName(ctx context.Context, st *store.Store, name string) (int64, error) {
	rows, err := st.ListSchedules(ctx)
	if err != nil {
		return 0, fmt.Errorf("list schedules: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, fmt.Errorf("seeded schedule %q not found (db not migrated?)", name)
}

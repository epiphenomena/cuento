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
// real import does not produce. Today it seeds a SAMPLE BUDGET PLAN (a plan + several
// projected budget-splits, the p27.2 split-derived model) so the budget-group reports
// (cashflow_projection / budget_variance) have something to render; more
// `devseed <thing>` subcommands can follow.
//
// It is built on the STORE (the write funnel + invariants, rule 2) -- NOT raw SQL --
// so it stays aligned with schema changes automatically. All amounts are SYNTHETIC
// round figures (rule 11): no real value is embedded. It resolves the accounts /
// programs / subsidiary / funds it needs DYNAMICALLY from whatever the db already
// contains (no hardcoded real names), so it works against any db -- the dev.db, a
// scaffold, or a fresh migrate. It is idempotent per db (skips if the sample plan
// already exists), so `make dev-db` reruns and standalone reruns never pile up
// duplicates.
//
// It is a dev tool, so it is NOT wired into any deployed path; `usage()` lists it but
// production never runs it.

// devseedBudgetName is the name devseed gives its sample budget plan. The idempotency
// guard keys on it, so a rerun is a no-op.
const devseedBudgetName = "Sample Cash-Flow Plan"

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
	_, _ = fmt.Fprintf(stdout, "usage: cuento devseed <target> [-db PATH]\n\ntargets:\n"+
		"  budget    seed a SYNTHETIC sample budget plan + splits (for the budget reports)\n")
}

// devseedBudgetCmd migrates the db, opens the store, and seeds the sample budget plan.
// It migrates FIRST so the single command is self-sufficient against a db built before
// the budget-plan migrations landed.
func devseedBudgetCmd(args []string) error {
	fs := flag.NewFlagSet("devseed budget", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database file")
	if err := fs.Parse(args); err != nil {
		// flag.ErrHelp (from -h) is not a failure: usage was already printed.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, *dbPath); err != nil {
		return fmt.Errorf("devseed budget: migrate: %w", err)
	}

	st, closeFn, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer closeFn()

	ctx = store.WithActor(ctx, systemActor)
	created, err := seedSampleBudgetPlan(ctx, st)
	if err != nil {
		return fmt.Errorf("devseed budget: %w", err)
	}
	if !created {
		_, _ = fmt.Fprintf(stdout, "devseed budget: %q already exists in %s (no-op)\n", devseedBudgetName, *dbPath)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "devseed budget: created %q in %s\n", devseedBudgetName, *dbPath)
	return nil
}

// seedSampleBudgetPlan creates a synthetic sample budget PLAN (the p27.2 split-derived
// model) with several projected splits against whatever accounts/programs/funds the db
// already has. It returns created=false (and writes nothing) when the sample plan
// already exists. Every reference is resolved DYNAMICALLY -- no real names are
// hardcoded (rule 11) -- and a db missing the minimum shape (a subsidiary, a program,
// a revenue leaf, an expense leaf) is a clear error rather than a partial seed. Each
// split carries an explicit date (no schedule object): the cadence is materialized at
// entry, not stored.
func seedSampleBudgetPlan(ctx context.Context, st *store.Store) (bool, error) {
	// Idempotency: skip if the sample plan already exists.
	plans, err := st.ListBudgetPlans(ctx)
	if err != nil {
		return false, fmt.Errorf("list budget plans: %w", err)
	}
	for _, p := range plans {
		if p.Name == devseedBudgetName {
			return false, nil
		}
	}

	// Resolve the pieces a budget-split needs from the live db. The plan's accounts
	// must be scoped to the PLAN's subsidiary (the store rejects an out-of-subsidiary
	// budget-split account, ErrBudgetSplitAccountSub): a real import scopes R/E leaves
	// to the OPERATING subsidiaries, not the root, so we pick a subsidiary that has
	// BOTH a revenue and an expense leaf and resolve the accounts within it -- rather
	// than the root subsidiary + any-scoped leaves, which fails on a real-import db.
	subID, revAcct, expAcct, err := firstSubsidiaryWithREPair(ctx, st)
	if err != nil {
		return false, err
	}
	progID, err := firstProgram(ctx, st)
	if err != nil {
		return false, err
	}

	planID, err := st.CreateBudgetPlan(ctx, store.BudgetPlanInput{
		Name:         devseedBudgetName,
		SubsidiaryID: subID,
		Notes:        "Synthetic sample cash-flow plan (cuento devseed).",
	})
	if err != nil {
		return false, fmt.Errorf("create budget plan: %w", err)
	}

	// Several projected splits: revenue inflows and expense outflows on varied dates
	// (materialized cadence). Amounts are SYNTHETIC round figures; each split's
	// currency is its account's default currency, so it always exists in `currencies`.
	type split struct {
		account int64
		date    string
		amount  int64
		ccy     string
	}
	splits := []split{
		{revAcct.id, "2026-01-15", 500_000, revAcct.currency},
		{revAcct.id, "2026-04-15", 500_000, revAcct.currency},
		{expAcct.id, "2026-02-01", 180_000, expAcct.currency},
		{expAcct.id, "2026-03-01", 180_000, expAcct.currency},
		{expAcct.id, "2026-05-01", 180_000, expAcct.currency},
	}
	// Splits are UNRESTRICTED (nil fund): a resolved fund's program-subtree scope may
	// not include the resolved program, and this seed favors working against any db over
	// exercising fund restriction (the demo's ExtendSampleBudgetPlan covers restricted).
	for _, sp := range splits {
		if _, err := st.CreateBudgetSplit(ctx, planID, store.BudgetSplitInput{
			Description: "Sample projection",
			Date:        sp.date,
			AccountID:   sp.account,
			ProgramID:   &progID,
			Amount:      sp.amount,
			Currency:    sp.ccy,
		}); err != nil {
			return false, fmt.Errorf("create budget split (account %d): %w", sp.account, err)
		}
	}
	return true, nil
}

// leafAccount is a resolved leaf account: its id and default currency.
type leafAccount struct {
	id       int64
	currency string
}

// firstSubsidiaryWithREPair returns the first subsidiary (in tree order) that has
// BOTH an active revenue leaf AND an active expense leaf mapped to it, along with
// those two leaves (each carrying its default currency). The store scopes a budget-
// split's account to the plan's subsidiary (ErrBudgetSplitAccountSub), so the plan's
// subsidiary and its splits' accounts must agree: a real import maps R/E leaves to
// the OPERATING subsidiaries (not the txn-less root), so picking the root + any leaf
// fails. Resolving the subsidiary and its leaves together guarantees they match on
// any db shape (the fixture's root holds everything; a real import's operating subs
// hold the R/E leaves). Errors if no subsidiary has such a pair.
func firstSubsidiaryWithREPair(ctx context.Context, st *store.Store) (int64, leafAccount, leafAccount, error) {
	tree, err := st.SubTree(ctx)
	if err != nil {
		return 0, leafAccount{}, leafAccount{}, fmt.Errorf("sub tree: %w", err)
	}
	if len(tree) == 0 {
		return 0, leafAccount{}, leafAccount{}, errors.New("no subsidiaries in db")
	}
	for _, s := range tree {
		rev, revErr := firstLeafAccountInSub(ctx, st, s.ID, "revenue")
		if revErr != nil {
			continue
		}
		exp, expErr := firstLeafAccountInSub(ctx, st, s.ID, "expense")
		if expErr != nil {
			continue
		}
		return s.ID, rev, exp, nil
	}
	return 0, leafAccount{}, leafAccount{}, errors.New("no subsidiary has both a revenue and an expense leaf account")
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

// firstLeafAccountInSub returns the first ACTIVE LEAF account of the given type
// (revenue or expense -- the only types a budget line accepts) MAPPED TO subID. A
// leaf is a row that is no other row's parent; the leaf/parent relation is computed
// over the FULL tree (a parent may live outside subID), but the candidate is drawn
// from the subsidiary-filtered tree so the returned account is guaranteed scoped to
// subID (the store's budget-split subsidiary check, ErrBudgetSplitAccountSub).
// Errors if none exists in that subsidiary.
func firstLeafAccountInSub(ctx context.Context, st *store.Store, subID int64, typ string) (leafAccount, error) {
	full, err := st.Tree(ctx, "en", nil)
	if err != nil {
		return leafAccount{}, fmt.Errorf("account tree: %w", err)
	}
	isParent := make(map[int64]bool, len(full))
	for _, r := range full {
		if r.ParentID.Valid {
			isParent[r.ParentID.Int64] = true
		}
	}
	inSub, err := st.Tree(ctx, "en", &subID)
	if err != nil {
		return leafAccount{}, fmt.Errorf("account tree (sub %d): %w", subID, err)
	}
	for _, r := range inSub {
		if r.Type != typ || r.Active == 0 || isParent[r.ID] {
			continue
		}
		acct, err := st.GetAccount(ctx, r.ID)
		if err != nil {
			return leafAccount{}, fmt.Errorf("get account %d: %w", r.ID, err)
		}
		return leafAccount{id: r.ID, currency: acct.DefaultCurrency}, nil
	}
	return leafAccount{}, fmt.Errorf("no active leaf %s account in subsidiary %d", typ, subID)
}

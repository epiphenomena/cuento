package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"sort"
	"syscall"

	"cuento/internal/rates"
	"cuento/internal/store"
)

// ratesync (p14.2) pulls the configured currency pairs "as of today" from a
// RateSource (Yahoo Finance by default, behind the internal/rates seam) and writes
// them via PutRates as ONE change. It depends on the RateSource INTERFACE, not
// Yahoo directly, so the unofficial endpoint's inevitable breakage is confined to
// internal/rates (its parser is the tested unit; no network in `go test`).
//
// CONFIGURED PAIRS (documented in DECISIONS): derived, zero-config, from the org
// base currency (the ROOT subsidiary's base_currency, D18 -- report base follows
// the subsidiary, never an org_settings key) against every ACTIVE currency, minus
// the identity pair. Adding an active currency automatically extends coverage; no
// separate pairs list to maintain. The systemd timer that runs this unattended is
// documented in phase 18 (p18), NOT built here.

// ratesyncCmd implements `cuento ratesync [-db PATH]`: open the db, derive the
// pairs, fetch them through the real Yahoo source, PutRates the batch, and print a
// one-line summary. Factored so runRatesync (the logic) is tested with a fake
// source and ratesyncPairs (the derivation) is tested against the seeded db -- both
// without a network.
func ratesyncCmd(args []string) error {
	fs := flag.NewFlagSet("ratesync", flag.ContinueOnError)
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

	st, closeFn, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer closeFn()

	n, err := runRatesync(ctx, st, rates.NewYahoo())
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "ratesync: imported %d rate(s)\n", n)
	return nil
}

// runRatesync derives the configured pairs, fetches them through src, and PutRates
// the result under the system actor as ONE change. It returns the number of rates
// written. A source error aborts BEFORE any write (no partial batch); an empty fetch
// is a clean no-op (PutRates of nothing writes no change row). The system actor is
// bound here (not just in the dispatch) so callers -- including tests passing a bare
// context.Background() -- always write as the system user, satisfying the funnel.
func runRatesync(ctx context.Context, st *store.Store, src rates.RateSource) (int, error) {
	pairs, err := ratesyncPairs(ctx, st)
	if err != nil {
		return 0, err
	}
	if len(pairs) == 0 {
		return 0, nil
	}

	fetched, err := src.Fetch(ctx, pairs)
	if err != nil {
		return 0, fmt.Errorf("ratesync fetch: %w", err)
	}

	ctx = store.WithActor(ctx, systemActor)
	if err := st.PutRates(ctx, fetched); err != nil {
		return 0, err
	}
	return len(fetched), nil
}

// ratesyncPairs derives the base->quote pairs to fetch: the org base currency (the
// root subsidiary's base_currency) against every ACTIVE currency, skipping the
// identity pair. Quotes are ordered by currency code for deterministic fetch order.
// The base currency it derives is carried on every returned pair's Base field. An
// empty subsidiary tree (no root) is a clear error, never an index panic.
func ratesyncPairs(ctx context.Context, st *store.Store) ([]rates.Pair, error) {
	tree, err := st.SubTree(ctx)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, errors.New("ratesync: no root subsidiary (empty tree); cannot determine base currency")
	}
	base := tree[0].BaseCurrency // SubTree is pre-order: the root is first.

	curs, err := st.Currencies(ctx)
	if err != nil {
		return nil, err
	}
	var pairs []rates.Pair
	for _, c := range curs {
		if c.Active == 0 || c.Code == base {
			continue
		}
		pairs = append(pairs, rates.Pair{Base: base, Quote: c.Code})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Quote < pairs[j].Quote })
	return pairs, nil
}

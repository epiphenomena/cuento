package main

import (
	"context"
	"errors"
	"testing"

	"cuento/internal/rates"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// fakeSource is an in-memory RateSource: it records the pairs it was asked for and
// returns a canned result (or error). No network -- the point of the RateSource seam
// is that ratesync's logic is tested without touching Yahoo.
type fakeSource struct {
	got    []rates.Pair
	result []store.Rate
	err    error
}

func (f *fakeSource) Fetch(_ context.Context, pairs []rates.Pair) ([]store.Rate, error) {
	f.got = pairs
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// ratesyncStore builds a store over a fresh migrated db plus an actorless
// context.Background() -- runRatesync must bind the system actor itself (mirrors the
// user.go CLI funcs) so PutRates' write funnel accepts the write.
func ratesyncStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	d := testutil.NewDB(t)
	return store.New(d), context.Background()
}

// TestRatesyncPairsFromCurrencies: the pairs come from active currencies x the org
// base (root subsidiary base_currency), identity pairs skipped. A fresh db seeds the
// root at USD base and USD/MXN/EUR/HNL currencies, so the derived pairs are
// USD->MXN, USD->EUR, USD->HNL (order by currency code), and NOT USD->USD.
func TestRatesyncPairsFromCurrencies(t *testing.T) {
	st, ctx := ratesyncStore(t)

	pairs, base, err := ratesyncPairs(ctx, st)
	if err != nil {
		t.Fatalf("ratesyncPairs: %v", err)
	}
	if base != "USD" {
		t.Fatalf("base = %q, want USD", base)
	}
	got := map[string]bool{}
	for _, p := range pairs {
		if p.Base != "USD" {
			t.Errorf("pair base = %q, want USD", p.Base)
		}
		if p.Base == p.Quote {
			t.Errorf("identity pair %s->%s not skipped", p.Base, p.Quote)
		}
		got[p.Quote] = true
	}
	for _, want := range []string{"MXN", "EUR", "HNL"} {
		if !got[want] {
			t.Errorf("missing pair USD->%s", want)
		}
	}
	if got["USD"] {
		t.Error("identity pair USD->USD present")
	}
}

// TestRatesyncWritesFetched: runRatesync fetches the derived pairs through the source
// and PutRates the result as ONE change; RateOn then resolves each written pair.
func TestRatesyncWritesFetched(t *testing.T) {
	st, ctx := ratesyncStore(t)

	src := &fakeSource{result: []store.Rate{
		{RateDate: "2024-01-01", Base: "USD", Quote: "MXN", Value: 17.1, Source: rates.SourceYahoo},
		{RateDate: "2024-01-01", Base: "USD", Quote: "EUR", Value: 0.92, Source: rates.SourceYahoo},
	}}

	n, err := runRatesync(ctx, st, src)
	if err != nil {
		t.Fatalf("runRatesync: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported = %d, want 2", n)
	}
	// The source was asked for pairs derived from currencies, never an identity pair.
	for _, p := range src.got {
		if p.Base == p.Quote {
			t.Errorf("source asked for identity pair %s->%s", p.Base, p.Quote)
		}
	}
	// The fetched rates are persisted.
	got, err := st.RateOn(store.WithActor(ctx, store.Actor{ID: 1}), "USD", "MXN", "2024-01-01")
	if err != nil {
		t.Fatalf("RateOn USD->MXN: %v", err)
	}
	if got.Rate != 17.1 {
		t.Errorf("USD->MXN = %v, want 17.1", got.Rate)
	}
}

// TestRatesyncSourceErrorNoWrite: a source error surfaces and writes NOTHING (no
// partial batch) -- RateOn finds no rate afterwards.
func TestRatesyncSourceErrorNoWrite(t *testing.T) {
	st, ctx := ratesyncStore(t)

	src := &fakeSource{err: errors.New("yahoo exploded")}

	if _, err := runRatesync(ctx, st, src); err == nil {
		t.Fatal("runRatesync: want error from source, got nil")
	}
	if _, err := st.RateOn(store.WithActor(ctx, store.Actor{ID: 1}), "USD", "MXN", "2024-12-31"); !errors.Is(err, store.ErrRateMissing) {
		t.Errorf("after source error, RateOn err = %v, want ErrRateMissing (nothing written)", err)
	}
}

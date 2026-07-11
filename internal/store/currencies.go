package store

import (
	"context"
	"fmt"

	"cuento/internal/db/sqlc"
)

// Currencies and Currency are the store's first READ methods. Reads bypass the
// write funnel (no actor, no changes row): currencies is static reference data
// (D1), so these are thin pass-throughs to the sqlc-generated queries on the
// base *sqlc.Queries. Keeping reads in sqlc satisfies rules 2 and 6. Any
// currency-domain logic (Amount, exponent-aware math) lives in internal/money,
// a later step — not here.

// Currencies returns all currency rows ordered by code.
func (s *Store) Currencies(ctx context.Context) ([]sqlc.Currency, error) {
	cs, err := s.q.ListCurrencies(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list currencies: %w", err)
	}
	return cs, nil
}

// Currency returns the currency with the given code.
func (s *Store) Currency(ctx context.Context, code string) (sqlc.Currency, error) {
	c, err := s.q.GetCurrency(ctx, code)
	if err != nil {
		return sqlc.Currency{}, fmt.Errorf("store: get currency %q: %w", code, err)
	}
	return c, nil
}

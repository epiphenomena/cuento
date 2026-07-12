package store

import (
	"context"
	"errors"
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

// ErrInvalidCurrency is returned by AddCurrency when the input is malformed (a
// non 3-letter code or an exponent outside 0..4). currencies is reference data
// with no form-error convention of its own, so the web layer maps this to a
// page-level 422 guard. The migration's exponent CHECK is a backstop; the store
// is the guard.
var ErrInvalidCurrency = errors.New("store: invalid currency")

// AddCurrencyInput is the desired state of a currency to add/enable (p13.2). Code
// is uppercased and must be exactly three ASCII letters (ISO-4217 shape, though
// cuento accepts any 3-letter code); Exponent is the minor-unit scale (0..4, D1);
// Symbol/Name are display strings.
type AddCurrencyInput struct {
	Code     string
	Exponent int64
	Symbol   string
	Name     string
}

// AddCurrency inserts (or re-enables) a currency. currencies is STATIC reference
// data (D1), NOT a versioned business table -- so this write is a plain
// reference-data upsert OUTSIDE the write funnel (no actor, no changes row, no
// version twin), exactly like SyncReportGroups / SetOrgSetting (rule 2). The
// upsert is idempotent (a re-add refreshes metadata and re-enables), so an admin
// re-adding an existing code never errors. Input shape is validated first
// (ErrInvalidCurrency).
func (s *Store) AddCurrency(ctx context.Context, in AddCurrencyInput) error {
	if !validCurrencyCode(in.Code) || in.Exponent < 0 || in.Exponent > 4 {
		return ErrInvalidCurrency
	}
	if in.Symbol == "" || in.Name == "" {
		return ErrInvalidCurrency
	}
	if err := s.q.InsertCurrency(ctx, sqlc.InsertCurrencyParams{
		Code:     in.Code,
		Exponent: in.Exponent,
		Symbol:   in.Symbol,
		Name:     in.Name,
	}); err != nil {
		return fmt.Errorf("store: add currency %q: %w", in.Code, err)
	}
	return nil
}

// SetCurrencyActive enables/disables a currency (p13.2). A disabled currency is
// hidden from the subsidiary base-currency picker; its historical rows keep their
// code. Reference-data write, outside the funnel (currencies is not versioned).
func (s *Store) SetCurrencyActive(ctx context.Context, code string, active bool) error {
	a := int64(0)
	if active {
		a = 1
	}
	if err := s.q.SetCurrencyActive(ctx, sqlc.SetCurrencyActiveParams{Active: a, Code: code}); err != nil {
		return fmt.Errorf("store: set currency %q active=%v: %w", code, active, err)
	}
	return nil
}

// validCurrencyCode reports whether code is exactly three ASCII uppercase letters.
// AddCurrency requires the caller to uppercase first; this keeps the guard simple
// and the stored codes uniform (currencies.code is the PK joined against subsidiary
// base currencies).
func validCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

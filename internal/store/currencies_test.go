package store

import (
	"context"
	"testing"

	"cuento/internal/db/sqlc"
	"cuento/internal/testutil"
)

// TestSeedCurrencies proves the p03.1 migration seeds USD, MXN and EUR with the
// correct exponents (all 2, D1), read back through the store's thin sqlc
// wrappers — not raw SQL (rule 2/6). Currencies is static reference data, so no
// actor is needed for the read.
func TestSeedCurrencies(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	ctx := context.Background()

	list, err := s.Currencies(ctx)
	if err != nil {
		t.Fatalf("Currencies: %v", err)
	}

	// Key by code so row ordering isn't load-bearing.
	byCode := make(map[string]sqlc.Currency, len(list))
	for _, c := range list {
		byCode[c.Code] = c
	}

	want := map[string]int64{"USD": 2, "MXN": 2, "EUR": 2}
	for code, exp := range want {
		c, ok := byCode[code]
		if !ok {
			t.Errorf("currency %q missing from seed", code)
			continue
		}
		if c.Exponent != exp {
			t.Errorf("%s exponent = %d, want %d", code, c.Exponent, exp)
		}
	}

	// Single-currency lookup returns the same row.
	usd, err := s.Currency(ctx, "USD")
	if err != nil {
		t.Fatalf("Currency(USD): %v", err)
	}
	if usd.Code != "USD" || usd.Exponent != 2 {
		t.Errorf("Currency(USD) = %+v, want code USD exponent 2", usd)
	}
}

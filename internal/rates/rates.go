// Package rates isolates the FRAGILE, unofficial exchange-rate fetch (Yahoo
// Finance) behind a small RateSource interface. The `cuento ratesync` command
// (p14.2) depends on this interface, never on Yahoo directly, so the day Yahoo's
// unofficial endpoint breaks or changes shape the blast radius is one file
// (yahoo.go) and one parser -- not the command, the store, or the tests.
//
// The tested unit is the PARSER: it turns a recorded response body into
// []store.Rate. No test in this package touches the network (AGENTS testing
// convention); the parser runs against SYNTHETIC bodies in testdata/ (DATA RULE
// 11) and the HTTP layer is a thin, test-injectable shim over a settable base URL.
package rates

import (
	"context"
	"errors"

	"cuento/internal/store"
)

// ErrNoData is returned when a source response carries no usable rate for a pair:
// an empty result, an error channel populated by the provider, or a missing price
// field. Because the endpoint is unofficial, callers treat a per-pair ErrNoData as
// "skip this pair" rather than a fatal error where sensible; a whole-fetch failure
// (network, non-200) is a different, wrapped error.
var ErrNoData = errors.New("rates: no rate data in response")

// Pair is one currency pair to fetch: the base->quote direction (e.g. USD->MXN),
// matching how exchange_rates stores a base->quote multiplier (D12).
type Pair struct {
	Base  string
	Quote string
}

// RateSource fetches current rates for a set of pairs. It is the seam that isolates
// the unofficial provider: ratesync holds a RateSource, tests inject a fake or a
// source reading recorded bodies, and the real network call lives behind the
// interface (yahoo.go). Fetch returns one store.Rate per pair it could resolve; a
// pair with no data is omitted (not an error) so one bad symbol never sinks a batch.
type RateSource interface {
	Fetch(ctx context.Context, pairs []Pair) ([]store.Rate, error)
}

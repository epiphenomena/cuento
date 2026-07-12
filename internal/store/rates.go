package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
)

// Exchange rates (p14.1) -- report-time FX conversion input (D12). Unlike the
// versioned business tables, exchange_rates is CHANGE-ID-ANCHORED reference data:
// a rate batch IS audited (PutRates runs through the write funnel so a whole batch
// shares ONE changes row), but there is NO *_versions twin -- a rate is a fact
// about a day, not an editable entity with a history to reconstruct. So PutRates
// inserts the live rows carrying the funnel's changeID directly; there is no
// snapshot-from-live append.
//
// RateOn is a READ (no funnel): it answers "the rate for base->quote effective on
// or before this date", preferring a DIRECT row, then a reciprocal (quote,base)
// row (returning 1/it), else ErrRateMissing. It always returns the rate's ACTUAL
// rate_date so a report can footnote staleness ("as of <rate_date>") -- the whole
// point of surfacing the date rather than echoing the query date.

// ErrRateMissing is returned by RateOn when neither a direct (base,quote) nor a
// reciprocal (quote,base) rate exists on or before the requested date. Callers
// (p15 reporting) branch on it via errors.Is to footnote a conversion gap rather
// than fail the report, so it is wrapped with %w at the return site.
var ErrRateMissing = errors.New("store: no exchange rate on or before the date")

// Rate is one desired exchange-rate row for a PutRates batch. RateDate is
// YYYY-MM-DD (the same date convention as transactions); Base/Quote are currency
// codes; Value is the base->quote multiplier (a REAL, D12); Source names the
// origin (e.g. a rate provider or "manual") for the audit trail.
type Rate struct {
	RateDate string
	Base     string
	Quote    string
	Value    float64
	Source   string
}

// RateResult is what RateOn returns: the resolved rate AND the ACTUAL rate_date of
// the row it came from (which may be older than the query date -- staleness the
// report footnotes). Reciprocal indicates the value was derived as 1/(quote,base)
// because no direct (base,quote) row existed on or before the date.
type RateResult struct {
	Rate       float64
	RateDate   string
	Reciprocal bool
}

// PutRates writes a batch of rates under ONE change (the "batch, one change" of the
// PLAN): every row in rates shares the funnel's single changes row via change_id,
// so the audit trail records one load event rather than one per pair. An empty
// batch is a no-op (no change row). Insert is idempotent per (date, base, quote)
// so a re-load of the same day/pair overwrites rather than PK-colliding.
func (s *Store) PutRates(ctx context.Context, rates []Rate) error {
	if len(rates) == 0 {
		return nil
	}
	_, err := s.write(ctx, "rates.put", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			for _, r := range rates {
				if err := q.InsertRate(ctx, sqlc.InsertRateParams{
					RateDate: r.RateDate,
					Base:     r.Base,
					Quote:    r.Quote,
					Rate:     r.Value,
					Source:   r.Source,
					ChangeID: changeID,
				}); err != nil {
					return fmt.Errorf("insert rate %s %s->%s: %w", r.RateDate, r.Base, r.Quote, err)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("put rates: %w", err)
	}
	return nil
}

// RateOn returns the rate for base->quote effective ON OR BEFORE date, plus the
// ACTUAL rate_date of the row used. Resolution order:
//
//   - base == quote: identity 1.0, no row needed (its rate_date is the query date;
//     an identity rate is never stale, so the date is immaterial to footnotes);
//   - a DIRECT (base,quote) row on or before date: return it (direct always wins,
//     even if a reciprocal row has a later date);
//   - else a RECIPROCAL (quote,base) row on or before date: return 1/it with ITS
//     rate_date and Reciprocal=true;
//   - else ErrRateMissing (wrapped with %w so callers errors.Is it).
//
// The returned rate_date can be older than date (the newest rate predates the
// query) -- RateOn surfaces the real date so a report footnotes the gap.
func (s *Store) RateOn(ctx context.Context, base, quote, date string) (RateResult, error) {
	if base == quote {
		return RateResult{Rate: 1, RateDate: date}, nil
	}

	// Direct (base,quote) takes precedence: only a genuine absence (ErrNoRows)
	// falls through to the reciprocal attempt.
	direct, err := s.q.RateOnOrBefore(ctx, sqlc.RateOnOrBeforeParams{Base: base, Quote: quote, RateDate: date})
	if err == nil {
		return RateResult{Rate: direct.Rate, RateDate: direct.RateDate}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RateResult{}, fmt.Errorf("rate on %s->%s: %w", base, quote, err)
	}

	// Reciprocal fallback: an inverse (quote,base) row on or before the date.
	inv, err := s.q.RateOnOrBefore(ctx, sqlc.RateOnOrBeforeParams{Base: quote, Quote: base, RateDate: date})
	if err == nil {
		return RateResult{Rate: 1 / inv.Rate, RateDate: inv.RateDate, Reciprocal: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RateResult{}, fmt.Errorf("rate on %s->%s (reciprocal): %w", base, quote, err)
	}

	return RateResult{}, fmt.Errorf("rate on %s->%s at %s: %w", base, quote, date, ErrRateMissing)
}

package store

import (
	"context"
	"errors"
	"testing"

	"cuento/internal/testutil"
)

// ratesStore builds a store over a fresh migrated db and an actor-bearing context
// (PutRates goes through the write funnel, which requires an actor). USD/MXN/EUR
// are seeded by migration 00003, so rate rows referencing them satisfy the
// base/quote FKs without extra setup.
func ratesStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	d := testutil.NewDB(t)
	s := New(d)
	ctx := WithActor(context.Background(), Actor{ID: 1})
	return s, ctx
}

// TestRateOnOrBefore: a rate set on date D is returned for D and every later date
// until a newer rate supersedes it; a date BEFORE the first rate is ErrRateMissing.
func TestRateOnOrBefore(t *testing.T) {
	s, ctx := ratesStore(t)

	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-01-01", Base: "USD", Quote: "MXN", Value: 17.00, Source: "test"},
		{RateDate: "2025-03-01", Base: "USD", Quote: "MXN", Value: 18.00, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates: %v", err)
	}

	cases := []struct {
		date     string
		wantRate float64
		wantDate string
	}{
		{"2025-01-01", 17.00, "2025-01-01"}, // exactly on D
		{"2025-01-15", 17.00, "2025-01-01"}, // D+n, before the next rate
		{"2025-02-28", 17.00, "2025-01-01"}, // day before the next rate
		{"2025-03-01", 18.00, "2025-03-01"}, // the next rate supersedes on its date
		{"2025-06-30", 18.00, "2025-03-01"}, // and stays until superseded again
	}
	for _, c := range cases {
		got, err := s.RateOn(ctx, "USD", "MXN", c.date)
		if err != nil {
			t.Fatalf("RateOn USD->MXN %s: %v", c.date, err)
		}
		if got.Rate != c.wantRate || got.RateDate != c.wantDate {
			t.Errorf("RateOn USD->MXN %s = {%v, %q}, want {%v, %q}",
				c.date, got.Rate, got.RateDate, c.wantRate, c.wantDate)
		}
		if got.Reciprocal {
			t.Errorf("RateOn USD->MXN %s: unexpected reciprocal (a direct row exists)", c.date)
		}
	}

	// A date before the first rate has nothing on or before it -> ErrRateMissing.
	if _, err := s.RateOn(ctx, "USD", "MXN", "2024-12-31"); !errors.Is(err, ErrRateMissing) {
		t.Errorf("RateOn before first rate: err = %v, want ErrRateMissing", err)
	}
}

// TestRateReciprocalFallback: only (MXN,USD) is stored; RateOn(USD,MXN,.) returns
// 1/it with the reciprocal row's ACTUAL date and Reciprocal=true. A DIRECT row,
// once present, takes precedence even over a later reciprocal.
func TestRateReciprocalFallback(t *testing.T) {
	s, ctx := ratesStore(t)

	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-02-01", Base: "MXN", Quote: "USD", Value: 0.05, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates: %v", err)
	}

	got, err := s.RateOn(ctx, "USD", "MXN", "2025-02-15")
	if err != nil {
		t.Fatalf("RateOn USD->MXN (reciprocal): %v", err)
	}
	if !got.Reciprocal {
		t.Errorf("RateOn USD->MXN: Reciprocal = false, want true (only the inverse is stored)")
	}
	if got.Rate != 1.0/0.05 {
		t.Errorf("RateOn USD->MXN reciprocal rate = %v, want %v", got.Rate, 1.0/0.05)
	}
	if got.RateDate != "2025-02-01" {
		t.Errorf("RateOn USD->MXN reciprocal date = %q, want %q", got.RateDate, "2025-02-01")
	}

	// Add a DIRECT row EARLIER than the reciprocal; direct must win unconditionally.
	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-01-10", Base: "USD", Quote: "MXN", Value: 17.50, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates direct: %v", err)
	}
	got, err = s.RateOn(ctx, "USD", "MXN", "2025-02-15")
	if err != nil {
		t.Fatalf("RateOn USD->MXN (direct precedence): %v", err)
	}
	if got.Reciprocal || got.Rate != 17.50 || got.RateDate != "2025-01-10" {
		t.Errorf("RateOn USD->MXN with a direct row = {%v, %q, recip=%v}, want {17.5, \"2025-01-10\", false}",
			got.Rate, got.RateDate, got.Reciprocal)
	}
}

// TestRateMissing: an unknown pair and a date-before-first both return a typed
// error callers can errors.Is. base == quote is identity 1.0 without a row.
func TestRateMissing(t *testing.T) {
	s, ctx := ratesStore(t)

	// No rows at all for the pair, in either direction.
	if _, err := s.RateOn(ctx, "USD", "EUR", "2025-05-01"); !errors.Is(err, ErrRateMissing) {
		t.Errorf("RateOn unknown pair: err = %v, want ErrRateMissing", err)
	}

	// A stored pair, but the query date is before the earliest row.
	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-05-01", Base: "USD", Quote: "MXN", Value: 18.00, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates: %v", err)
	}
	if _, err := s.RateOn(ctx, "USD", "MXN", "2025-04-30"); !errors.Is(err, ErrRateMissing) {
		t.Errorf("RateOn date-before-first: err = %v, want ErrRateMissing", err)
	}

	// base == quote is identity, no row needed and never missing.
	got, err := s.RateOn(ctx, "USD", "USD", "2025-04-30")
	if err != nil {
		t.Fatalf("RateOn USD->USD identity: %v", err)
	}
	if got.Rate != 1.0 {
		t.Errorf("RateOn USD->USD identity rate = %v, want 1.0", got.Rate)
	}
}

// TestRateStaleness: when the newest rate is OLDER than the query date, the lookup
// still returns it AND surfaces ITS real rate_date (not the query date), so a
// report can say "as of <rate_date>".
func TestRateStaleness(t *testing.T) {
	s, ctx := ratesStore(t)

	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-06-30", Base: "USD", Quote: "MXN", Value: 18.10, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates: %v", err)
	}

	// Query well AFTER the newest rate: it is returned, dated to when it was set.
	got, err := s.RateOn(ctx, "USD", "MXN", "2026-01-15")
	if err != nil {
		t.Fatalf("RateOn USD->MXN stale: %v", err)
	}
	if got.Rate != 18.10 {
		t.Errorf("stale rate = %v, want 18.10", got.Rate)
	}
	if got.RateDate != "2025-06-30" {
		t.Errorf("stale rate_date = %q, want %q (the rate's real date, NOT the query date)",
			got.RateDate, "2025-06-30")
	}
}

// TestPutRatesOneChange: a single PutRates batch of many rows shares ONE change_id
// across every inserted row (the "batch, one change" of the PLAN).
func TestPutRatesOneChange(t *testing.T) {
	s, ctx := ratesStore(t)

	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-01-01", Base: "USD", Quote: "MXN", Value: 17.00, Source: "test"},
		{RateDate: "2025-02-01", Base: "USD", Quote: "MXN", Value: 17.20, Source: "test"},
		{RateDate: "2025-03-01", Base: "USD", Quote: "MXN", Value: 17.40, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates: %v", err)
	}

	var distinct, rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT change_id), COUNT(*) FROM exchange_rates`,
	).Scan(&distinct, &rows); err != nil {
		t.Fatalf("count change_ids: %v", err)
	}
	if rows != 3 {
		t.Fatalf("inserted %d rows, want 3", rows)
	}
	if distinct != 1 {
		t.Errorf("batch spanned %d change_ids, want 1 (batch = one change)", distinct)
	}

	// The change_id must reference a real changes row (the audit anchor exists).
	var ok int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM changes c
		  WHERE c.id = (SELECT change_id FROM exchange_rates LIMIT 1)
		    AND c.kind = 'rates.put'`,
	).Scan(&ok); err != nil {
		t.Fatalf("verify change anchor: %v", err)
	}
	if ok != 1 {
		t.Errorf("rate change_id does not anchor a 'rates.put' changes row (got %d)", ok)
	}

	// A second batch is a SEPARATE change (distinct change_id).
	if err := s.PutRates(ctx, []Rate{
		{RateDate: "2025-04-01", Base: "USD", Quote: "MXN", Value: 17.60, Source: "test"},
	}); err != nil {
		t.Fatalf("PutRates second batch: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT change_id) FROM exchange_rates`,
	).Scan(&distinct); err != nil {
		t.Fatalf("count change_ids after 2nd batch: %v", err)
	}
	if distinct != 2 {
		t.Errorf("after two batches, distinct change_ids = %d, want 2", distinct)
	}
}

// TestPutRatesEmptyNoChange: an empty batch is a no-op -- no rate rows and no
// changes row (nothing to audit).
func TestPutRatesEmptyNoChange(t *testing.T) {
	s, ctx := ratesStore(t)

	before := countChanges(t, s.db)
	if err := s.PutRates(ctx, nil); err != nil {
		t.Fatalf("PutRates(nil): %v", err)
	}
	if after := countChanges(t, s.db); after != before {
		t.Errorf("empty PutRates added %d changes rows, want 0", after-before)
	}
}

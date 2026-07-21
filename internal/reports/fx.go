package reports

// fx.go is the Phase 31 FX-remeasurement toolkit (ASC 830-20): the report-time
// computation that recognizes the FX gain/loss on foreign-currency BALANCE-CARRYING
// accounts (assets and liabilities) as income (the change in net assets), rather than
// letting it disappear into the balance-sheet net-asset plug. It is the shared core
// behind the FX-detail report and the Statement-of-Activities "FX gain/loss" line, and
// it is a pure read (rule 2).
//
// The accounting, precisely (docs/DECISIONS.md p31):
//
//   - Each subsidiary's FUNCTIONAL currency is its base_currency (D18). A balance held
//     in a currency that EQUALS its holding sub's functional currency carries no FX
//     exposure. A balance in a DIFFERENT currency is a foreign-currency item.
//   - The discriminator is the account's TYPE. ASSET and LIABILITY accounts are
//     balance-carrying: a foreign-currency asset/liability balance is remeasured to the
//     functional currency at the CLOSING rate on the report date, while the historical
//     transactions that built it were measured at their TRANSACTION-DATE rates. The
//     difference is a remeasurement gain/loss recognized in INCOME (ASC 830-20-35).
//     REVENUE and EXPENSE accounts are flows, measured at their transaction-date rates
//     and not remeasured. EQUITY accounts are excluded (equity translates to CTA, and
//     the equity-class FX Clearing counter-leg must not be remeasured to income).
//     Remeasuring every foreign asset/liability at the closing rate matches the balance
//     sheet's own closing conversion, so the statement articulation is exact for every
//     balance-carrying account.
//   - INTERCOMPANY balances are EXCLUDED from the income path: their FX effect is a
//     foreign-entity TRANSLATION adjustment routed to the Cumulative Translation
//     Adjustment within Net Assets (equity), which cuento already carves out of the
//     consolidation residual (p26.70 / IntercompanyResidualSplit). Recognizing an
//     intercompany leg's remeasurement in income would double-count against that CTA
//     and strand its equal-and-opposite FX-Clearing leg (which is equity-class), so
//     intercompany asset/liability balances are routed to CTA, not income.
//
// The remeasurement is computed in each holding sub's FUNCTIONAL currency. When a sub's
// functional currency is the reporting currency (the common case, and every exposed sub
// in the base + FX fixture), that figure is directly the amount recognized in
// consolidated income. Translating a foreign ENTITY's functional-currency income to a
// different reporting currency is the separate translation step (p26.70 CTA).

import (
	"context"
	"math"
	"sort"
)

// FXItem is one foreign-currency asset/liability balance and its ASC 830-20
// remeasurement detail, all amounts in the holding subsidiary's FUNCTIONAL currency
// (minor units).
type FXItem struct {
	Sub             SubsidiaryID // the holding subsidiary
	Functional      string       // sub.base_currency (the functional/target currency)
	Account         AccountID
	Currency        string  // the foreign transaction currency the balance is held in
	NativeMinor     int64   // residual native balance as of the report date (signed, D2)
	ClosingRate     float64 // Currency -> Functional, on-or-before the report date
	ClosingRateDate string  // the actual date of the rate row used (staleness, p14.1)
	HistBasisMinor  int64   // Σ dated flows valued at their transaction-date rates (functional)
	ClosingMinor    int64   // NativeMinor valued at the closing rate (functional)
	RemeasureMinor  int64   // ClosingMinor − HistBasisMinor; negative = loss (functional)
}

// FXRemeasurement is the Phase 31 remeasurement result for a scope as of a date: the
// per-item detail (foreign, asset/liability, NON-intercompany balances) and the total
// remeasurement gain/loss per functional currency.
type FXRemeasurement struct {
	AsOf         string
	Items        []FXItem
	ByFunctional map[string]int64 // functional currency -> Σ RemeasureMinor
}

// fxKey groups the dated rows into one remeasurement item per (holding sub, account,
// currency).
type fxKey struct {
	sub  SubsidiaryID
	acct AccountID
	ccy  string
}

// FXRemeasurementAsOf computes the ASC 830-20 remeasurement gain/loss on every
// foreign-currency, asset/liability, NON-intercompany balance in the scope's descendant
// closure as of d. Each item's balance is valued at the closing rate and its building
// flows at their transaction-date rates (accumulated UNROUNDED, rounded half-even once,
// matching Activity's RateTxnDate grain, D12); the difference is the gain/loss.
func (tk *Toolkit) FXRemeasurementAsOf(ctx context.Context, s Scope, d string) (FXRemeasurement, error) {
	// Per-run memo (p-perf): the Statement of Activities asks for the same boundary date
	// twice (a column's opening == the previous column's closing under contiguous ByPeriod),
	// each a full inception-to-date scan. FXRemeasurementAsOf is a pure read of static data,
	// so caching by (scope, date) is byte-identical. The cached FXRemeasurement is only read
	// (never mutated) by callers, so sharing the value is safe.
	key := fxSnapKey{sub: s.Sub, d: d}
	if tk.fxCache != nil {
		if r, ok := tk.fxCache[key]; ok {
			return r, nil
		}
	}
	rows, err := tk.store.SubDatedBalancesAsOf(ctx, d, s.Sub)
	if err != nil {
		return FXRemeasurement{}, err
	}

	// Functional currency per subsidiary (= base_currency, D18).
	subTree, err := tk.store.SubTree(ctx)
	if err != nil {
		return FXRemeasurement{}, err
	}
	functional := make(map[SubsidiaryID]string, len(subTree))
	for _, sub := range subTree {
		functional[sub.ID] = sub.BaseCurrency
	}

	// Aggregate the dated rows into a native residual balance and an unrounded
	// transaction-date basis per (sub, account, currency). The basis values each dated
	// flow at that date's on-or-before rate into the sub's functional currency.
	native := make(map[fxKey]int64)
	basis := make(map[fxKey]float64)
	for _, r := range rows {
		k := fxKey{sub: r.SubsidiaryID, acct: r.AccountID, ccy: r.Currency}
		func0 := functional[r.SubsidiaryID]
		native[k] += r.Amount
		if r.Currency == func0 {
			basis[k] += float64(r.Amount) // functional-currency flow: rate is identity
		} else {
			rr, err := tk.rateOn(ctx, r.Currency, func0, r.Date)
			if err != nil {
				return FXRemeasurement{}, err
			}
			exFrom, exTo, err := tk.exponents(ctx, r.Currency, func0)
			if err != nil {
				return FXRemeasurement{}, err
			}
			basis[k] += float64(r.Amount) * rr.Rate * pow10(exTo-exFrom)
		}
	}

	out := FXRemeasurement{AsOf: d, ByFunctional: map[string]int64{}}
	// Deterministic order: SubDatedBalancesAsOf already returns rows ordered by
	// (sub, account, currency), so first-seen order over the rows is stable.
	var order []fxKey
	added := make(map[fxKey]bool)
	for _, r := range rows {
		k := fxKey{sub: r.SubsidiaryID, acct: r.AccountID, ccy: r.Currency}
		if !added[k] {
			added[k] = true
			order = append(order, k)
		}
	}

	for _, k := range order {
		func0 := functional[k.sub]
		if k.ccy == func0 {
			continue // functional-currency balance: no FX exposure
		}
		// Account-TYPE AND non-intercompany gate. Asset/liability balances are
		// balance-carrying (remeasured at closing); revenue/expense are flows and
		// equity is excluded. The candidate set (foreign-currency balances) is small,
		// so a per-account read is cheap and clear.
		acct, err := tk.store.GetAccount(ctx, k.acct)
		if err != nil {
			return FXRemeasurement{}, err
		}
		if acct.Type != "asset" && acct.Type != "liability" {
			continue // revenue/expense -> flow; equity -> translation/CTA
		}
		if acct.Intercompany == 1 {
			continue // intercompany -> CTA (p26.70)
		}

		nativeMinor := native[k]
		closingMinor, err := tk.ConvertMinorAt(ctx, nativeMinor, k.ccy, func0, d)
		if err != nil {
			return FXRemeasurement{}, err
		}
		rr, err := tk.RateOn(ctx, k.ccy, func0, d)
		if err != nil {
			return FXRemeasurement{}, err
		}
		histMinor := RoundHalfEven(basis[k])
		remeasure := closingMinor - histMinor

		out.Items = append(out.Items, FXItem{
			Sub:             k.sub,
			Functional:      func0,
			Account:         k.acct,
			Currency:        k.ccy,
			NativeMinor:     nativeMinor,
			ClosingRate:     rr.Rate,
			ClosingRateDate: rr.RateDate,
			HistBasisMinor:  histMinor,
			ClosingMinor:    closingMinor,
			RemeasureMinor:  remeasure,
		})
		out.ByFunctional[func0] += remeasure
	}
	if tk.fxCache == nil {
		tk.fxCache = make(map[fxSnapKey]FXRemeasurement)
	}
	tk.fxCache[key] = out
	return out, nil
}

// fxSnapshotsByFunctional computes the FXRemeasurementAsOf ByFunctional totals for a set
// of ascending boundary dates from a SINGLE dated scan (p-perf), replacing N independent
// inception-to-date recomputations. It mirrors the balance-sheet single-scan pattern
// (6d9a399): the LATEST requested date is the max as-of, so one SubDatedBalancesAsOf scan
// to it covers every earlier boundary; each key's dated rows arrive contiguous and
// date-ASCENDING (the query's ORDER BY sub, account, currency, date), so accumulating the
// int64 native residual AND the float64 transaction-date basis in that exact row order,
// then snapshotting the running values at each cutoff, reproduces FXRemeasurementAsOf(date)
// byte-for-byte for every date -- the float basis is folded in the SAME order the per-date
// method uses (float addition is not associative, so order is load-bearing). The remeasure
// gate (foreign, asset/liability, non-intercompany) and its GetAccount lookups are hoisted
// OUT of the per-date loop and evaluated once per key. Every snapshot is written into
// tk.fxCache under its date, so a later FXRemeasurementAsOf(date) for a boundary date hits
// the cache and returns the identical value (the Items detail is NOT batched here -- the
// income statement reads only ByFunctional -- so a cached entry from this path carries an
// empty Items; a caller that needs Items must not share a date with this batch. Only the
// income statement uses this method, and it reads ByFunctional exclusively.)
//
// It returns date -> (functional currency -> Σ remeasure minor). A date with no exposure
// maps to an empty inner map.
func (tk *Toolkit) fxSnapshotsByFunctional(ctx context.Context, s Scope, dates []string) (map[string]map[string]int64, error) {
	out := make(map[string]map[string]int64, len(dates))
	if len(dates) == 0 {
		return out, nil
	}
	// Sort the requested cutoffs ascending and dedupe; the max is the scan bound.
	sorted := append([]string(nil), dates...)
	sort.Strings(sorted)
	uniq := sorted[:0]
	for i, d := range sorted {
		if i == 0 || d != sorted[i-1] {
			uniq = append(uniq, d)
		}
	}
	cutoffs := uniq
	maxDate := cutoffs[len(cutoffs)-1]

	// Serve any cutoff already cached (e.g. a boundary shared with a prior batch) from
	// tk.fxCache; only the uncached cutoffs need a fresh snapshot.
	need := cutoffs[:0:0]
	for _, d := range cutoffs {
		if tk.fxCache != nil {
			if r, ok := tk.fxCache[fxSnapKey{sub: s.Sub, d: d}]; ok {
				out[d] = r.ByFunctional
				continue
			}
		}
		need = append(need, d)
	}
	if len(need) == 0 {
		return out, nil
	}

	rows, err := tk.store.SubDatedBalancesAsOf(ctx, maxDate, s.Sub)
	if err != nil {
		return nil, err
	}

	subTree, err := tk.store.SubTree(ctx)
	if err != nil {
		return nil, err
	}
	functional := make(map[SubsidiaryID]string, len(subTree))
	for _, sub := range subTree {
		functional[sub.ID] = sub.BaseCurrency
	}

	// Per key, forward-accumulate native (int64) and basis (float64) in row order, and
	// snapshot the running values at each requested cutoff. Rows for a key are contiguous
	// and date-ascending, so a single left-to-right walk yields every cutoff's snapshot.
	type snap struct {
		native  int64
		basis   float64
		present bool // the key has >=1 row on-or-before this cutoff (else absent, like FXRemeasurementAsOf's order)
	}
	// snaps[k] holds, per requested cutoff index, the running native/basis at that cutoff.
	snaps := make(map[fxKey][]snap)
	var order []fxKey
	added := make(map[fxKey]bool)

	// Walk rows grouped by key (contiguous). Maintain a running native/basis and a cursor
	// into `need`; when the row date passes a cutoff, freeze the snapshot for that cutoff.
	i := 0
	for i < len(rows) {
		k := fxKey{sub: rows[i].SubsidiaryID, acct: rows[i].AccountID, ccy: rows[i].Currency}
		if !added[k] {
			added[k] = true
			order = append(order, k)
		}
		func0 := functional[k.sub]
		var run snap
		ks := make([]snap, len(need))
		ci := 0 // cursor into `need`
		for i < len(rows) {
			r := rows[i]
			rk := fxKey{sub: r.SubsidiaryID, acct: r.AccountID, ccy: r.Currency}
			if rk != k {
				break
			}
			// Before folding this row (date r.Date), freeze every cutoff strictly BEFORE it.
			for ci < len(need) && need[ci] < r.Date {
				ks[ci] = run
				ci++
			}
			run.present = true
			run.native += r.Amount
			if r.Currency == func0 {
				run.basis += float64(r.Amount)
			} else {
				rr, err := tk.rateOn(ctx, r.Currency, func0, r.Date)
				if err != nil {
					return nil, err
				}
				exFrom, exTo, err := tk.exponents(ctx, r.Currency, func0)
				if err != nil {
					return nil, err
				}
				run.basis += float64(r.Amount) * rr.Rate * pow10(exTo-exFrom)
			}
			i++
		}
		// Every remaining cutoff (>= this key's last row date) sees the full running total.
		for ci < len(need) {
			ks[ci] = run
			ci++
		}
		snaps[k] = ks
	}

	// Per cutoff, run the same remeasure gate + computation FXRemeasurementAsOf does, over
	// the keys in first-seen order, using that cutoff's snapshot. The gate (account type /
	// intercompany) is date-independent, so GetAccount is read once per key and reused.
	acctCache := make(map[AccountID]struct {
		typ string
		ic  int64
	})
	for ni, d := range need {
		result := FXRemeasurement{AsOf: d, ByFunctional: map[string]int64{}}
		for _, k := range order {
			func0 := functional[k.sub]
			if k.ccy == func0 {
				continue
			}
			ai, ok := acctCache[k.acct]
			if !ok {
				acct, err := tk.store.GetAccount(ctx, k.acct)
				if err != nil {
					return nil, err
				}
				ai.typ = acct.Type
				ai.ic = acct.Intercompany
				acctCache[k.acct] = ai
			}
			if ai.typ != "asset" && ai.typ != "liability" {
				continue
			}
			if ai.ic == 1 {
				continue
			}
			sp := snaps[k][ni]
			if !sp.present {
				continue // no row on-or-before this cutoff -> not in FXRemeasurementAsOf(d)'s key order
			}
			closingMinor, err := tk.ConvertMinorAt(ctx, sp.native, k.ccy, func0, d)
			if err != nil {
				return nil, err
			}
			histMinor := RoundHalfEven(sp.basis)
			result.ByFunctional[func0] += closingMinor - histMinor
		}
		out[d] = result.ByFunctional
		if tk.fxCache == nil {
			tk.fxCache = make(map[fxSnapKey]FXRemeasurement)
		}
		tk.fxCache[fxSnapKey{sub: s.Sub, d: d}] = result
	}
	return out, nil
}

// FXRemeasurementPeriodByFunctional returns the remeasurement gain/loss RECOGNIZED IN
// THE PERIOD [from,to], per functional currency (minor units). It is the CHANGE in each
// item's inception-to-date remeasurement between the day before `from` and `to`: the
// opening balance revalued from the prior period's closing rate to this period's, plus
// the period's own flows revalued from transaction-date to closing. It is the difference
// of two as-of snapshots (FXRemeasurementAsOf), so contiguous periods TELESCOPE exactly
// (a column's opening snapshot is the previous column's closing snapshot), and the sum
// over comparative columns equals the whole-window figure -- the income statement's
// footing rule (p15.5). A window starting at inception has an empty opening snapshot, so
// the period figure equals the as-of figure.
func (tk *Toolkit) FXRemeasurementPeriodByFunctional(ctx context.Context, s Scope, from, to string) (map[string]int64, error) {
	end, err := tk.FXRemeasurementAsOf(ctx, s, to)
	if err != nil {
		return nil, err
	}
	begin, err := tk.FXRemeasurementAsOf(ctx, s, dayBefore(from))
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(end.ByFunctional))
	for ccy, v := range end.ByFunctional {
		out[ccy] = v
	}
	for ccy, v := range begin.ByFunctional {
		out[ccy] -= v
	}
	return out, nil
}

// FXRemeasurementPeriodTarget returns the period remeasurement gain/loss over [from,to]
// converted to `target` and summed across functional currencies, each functional total
// translated at the period-end (`to`) closing rate. This is the single figure the
// Statement of Activities' FX gain/loss line carries for one column; when every
// subsidiary's functional currency is the target (the common case) the translation is an
// identity. Returns 0 when there is no exposure (so the caller can suppress the line).
func (tk *Toolkit) FXRemeasurementPeriodTarget(ctx context.Context, s Scope, from, to, target string) (int64, error) {
	byFunc, err := tk.FXRemeasurementPeriodByFunctional(ctx, s, from, to)
	if err != nil {
		return 0, err
	}
	var total int64
	for ccy, v := range byFunc {
		if v == 0 {
			continue
		}
		conv, err := tk.ConvertMinorAt(ctx, v, ccy, target, to)
		if err != nil {
			return 0, err
		}
		total += conv
	}
	return total, nil
}

// pow10 returns 10^n as a float (n may be negative) -- the exponent scale factor
// money.ConvertMinor applies, reused here so the unrounded basis accumulation matches
// the toolkit's rounding grain exactly.
func pow10(n int) float64 { return math.Pow(10, float64(n)) }

package main

import (
	"context"
	"fmt"
	"sort"

	"cuento/internal/money"
	"cuento/internal/store"
)

// This file implements the "Restore the Way" (RtW) restricted-fund model
// (D p26.43), which SUPERSEDES the p26.40 whole-transaction campus plug. The old
// model tagged EVERY kat=campus split to the fund and let the per-fund self-heal
// route the ~53% residual to Opening Balances -- 928 plug legs and a NEGATIVE
// (~-$180k) fund asset balance (a Z18 overspend warning). The new model treats RtW
// as a LIVE restricted fund that cannot overspend its revenue, so its asset
// balance stays >= 0 and Z18 is clean.
//
// Two ideas, in two passes:
//
//   Pass 1 (campusPlan) -- a GLOBAL, CHRONOLOGICAL, FX-NORMALIZED drawdown pool.
//   Restricted revenue received is spendable only until it runs out. We walk every
//   campus revenue/expense split in DATE order (tie-broken by tid then group index
//   for determinism -- and so every per-subsidiary import process, each handed the
//   FULL export, derives the IDENTICAL decision), maintaining a SINGLE running pool
//   balance in the org BASE currency (USD). Campus splits post in USD and HNL;
//   adding native minor units across currencies would be nonsense, so each split's
//   drawdown-pool contribution is CONVERTED to USD (D p26.47) on the SAME rate path
//   the consolidated-USD reports use: store.RateOn (on-or-before the split's date)
//   + money.ConvertMinor (D12). The STORED split amount stays NATIVE (rule 3); only
//   the overflow DECISION uses the converted USD figure. A campus REVENUE split
//   always joins the fund and grows the pool by its USD magnitude; a campus EXPENSE
//   split joins the fund only if the pool covers its USD amount (whole-split
//   granularity) and draws the pool down, else it OVERFLOWS to unrestricted (donor
//   ignored -- an overspent campus expense is just an ordinary unrestricted cost).
//   Once the pool is exhausted, later campus expenses stay unrestricted. The pool is
//   asymmetric ON PURPOSE: revenue may leave the fund positive (unexpended
//   restricted resources), but expense can never push it below zero.
//
//   Before D p26.47 the pool was PER CURRENCY (one bucket per USD/HNL), which
//   STARVED it: USD donations could not fund Honduran (HNL) construction, so ~30
//   campus expense legs wrongly overflowed to Unrestricted even though the campaign
//   never overspent in aggregate. Normalizing to one USD pool fixes that.
//
//   Pass 2 (offsetRtW, called from postBucket) -- OFFSET PAIRING so each
//   transaction still nets to zero PER FUND (rule 7/Z10) with NO plug leg in the
//   normal case. The RtW campus splits in a bucket sum to some nonzero amount; we
//   tag an equal-and-opposite portion of the bucket's BALANCE-SHEET
//   (asset/liability) splits to the fund so the RtW subset sums to zero. A
//   balance-sheet split that only partially offsets is DIVIDED into an RtW portion
//   and an unrestricted remainder (two SplitInputs, same account, different fund) --
//   the store allows this (per-fund and overall zero-sum are the only balance
//   invariants; two legs on one account are fine). Because the whole bucket already
//   nets to zero and the RtW subset nets to zero, the unrestricted remainder nets
//   to zero automatically -- so genuinely unrelated (non-campus) splits stay
//   unrestricted with their own offset portion, no extra bookkeeping.
//
// If a bucket's balance-sheet splits cannot supply the offset on the correct side
// (e.g. a multi-currency campus tid whose only counter-leg is the equity FX
// Clearing account, or a campus revenue whose counterpart is a non-campus expense),
// the leftover falls back to the existing per-fund self-heal (a `[campus-plug]`
// Opening-Balances leg). On the real data ~707 buckets hit this fallback; that is
// documented and expected (the plug is not eliminated, only made rare), and it
// does NOT drive the fund asset negative because the offset that DID land keeps the
// fund's asset backing positive.

// campusKey identifies one source split within its (skip-filtered) tid group. The
// group index is stable because Pass 1 and postGroup both walk the SAME `groups`
// map value in the same order.
type campusKey struct {
	tid string
	idx int
}

// campusPlan is the Pass-1 result: for each campus REVENUE/EXPENSE split, whether
// it is assigned RtW (true) or overflowed to unrestricted (false). A split with no
// entry is either non-campus or a campus balance-sheet split (offset candidate),
// neither of which the pool decides.
type campusPlan map[campusKey]bool

// buildCampusPlan runs the chronological drawdown pool over every campus R/E split
// in `groups` (already skip-country filtered, keyed by tid, group order preserved),
// returning the per-split RtW decision. campusFundOn is false when no campus fund
// is configured, in which case the plan is empty and campus splits follow the
// ordinary donor/unrestricted path.
func (b *builder) buildCampusPlan(ctx context.Context, groups map[string][]Record) (campusPlan, error) {
	plan := campusPlan{}
	if b.res.CampusFundID == nil {
		return plan, nil // feature off: no campus fund, no drawdown
	}

	// Collect every campus R/E split as a dated event carrying its stable key and its
	// drawdown contribution CONVERTED TO USD (D p26.47). The STORED amount stays
	// native (rule 3, resolveSplit uses db/cr); the pool decision alone is USD so a
	// USD donation can fund an HNL construction cost (and vice versa) -- the campaign
	// is one fund, not two per-currency sub-funds.
	type event struct {
		date  string
		tid   string
		idx   int
		usd   int64 // signed net-debit CONVERTED to USD minor units (revenue < 0, expense > 0)
		isRev bool  // classify by ACCOUNT TYPE, not sign, per the spec
		key   campusKey
	}
	var events []event
	for tid, recs := range groups {
		for i, r := range recs {
			if r.Kat != "campus" {
				continue
			}
			at, isRE, err := b.recordAcctType(r)
			if err != nil {
				return nil, err
			}
			if !isRE {
				continue // campus balance-sheet splits are offset candidates, not pool events
			}
			amt, err := b.recordAmount(r)
			if err != nil {
				return nil, err
			}
			if amt == 0 {
				continue // zero net-debit split is dropped at post time (errSkip); no pool effect
			}
			usd, err := b.recordAmountUSD(ctx, r, amt)
			if err != nil {
				return nil, err
			}
			events = append(events, event{
				date:  r.Dt,
				tid:   tid,
				idx:   i,
				usd:   usd,
				isRev: at == "revenue",
				key:   campusKey{tid: tid, idx: i},
			})
		}
	}

	// Chronological order: date, then tid, then group index -- a total order that is
	// identical no matter which subsidiary process computes it (each sees the full
	// export). Map iteration above is nondeterministic, so this sort is what makes
	// the plan reproducible.
	sort.Slice(events, func(i, j int) bool {
		a, c := events[i], events[j]
		if a.date != c.date {
			return a.date < c.date
		}
		if a.tid != c.tid {
			return a.tid < c.tid
		}
		return a.idx < c.idx
	})

	var pool int64 // restricted net assets available, in USD minor units (>= 0)
	for _, e := range events {
		if e.isRev {
			// Revenue received is restricted until spent: always RtW, grows the pool by
			// its USD magnitude (a credit net-debit is negative, so subtract to add).
			pool -= e.usd
			plan[e.key] = true
			continue
		}
		// Expense (positive net-debit): RtW only while the pool covers the whole split
		// (compared in USD); else it overflows to unrestricted and the pool is untouched.
		if pool >= e.usd {
			pool -= e.usd
			plan[e.key] = true
		} else {
			plan[e.key] = false
		}
	}
	return plan, nil
}

// recordAmountUSD converts a campus R/E split's native net-debit (nativeAmt, already
// computed by recordAmount) into org BASE-currency (USD) minor units for the drawdown
// pool DECISION only -- the stored split amount stays native (rule 3). It uses the
// SAME rate path the consolidated-USD reports use (store.RateOn on-or-before the
// split's date + money.ConvertMinor, D12), so the pool basis reconciles with the
// report figures by construction. A split already in the base currency is identity
// (no rate lookup). D p26.47.
func (b *builder) recordAmountUSD(ctx context.Context, r Record, nativeAmt int64) (int64, error) {
	base := b.cfg.BaseCurrency
	if r.Currency == base {
		return nativeAmt, nil
	}
	rr, err := b.st.RateOn(ctx, r.Currency, base, r.Dt)
	if err != nil {
		return 0, fmt.Errorf("campus pool rate %s->%s on %s: %w", r.Currency, base, r.Dt, err)
	}
	fromExp, ok := b.exponent[r.Currency]
	if !ok {
		return 0, fmt.Errorf("unknown currency %q", r.Currency)
	}
	toExp, ok := b.exponent[base]
	if !ok {
		return 0, fmt.Errorf("unknown base currency %q", base)
	}
	return money.ConvertMinor(nativeAmt, rr.Rate, fromExp, toExp), nil
}

// recordAcctType returns a source record's cuento account type and whether it is
// revenue/expense, resolving the account id from the reloaded map.
func (b *builder) recordAcctType(r Record) (string, bool, error) {
	acctID, ok := b.res.AccountIDs[r.Acct]
	if !ok {
		return "", false, fmt.Errorf("source account %q not mapped", r.Acct)
	}
	at := b.acctType[acctID]
	return at, at == "revenue" || at == "expense", nil
}

// recordAmount returns a source record's exact signed net-debit in ITS OWN
// currency's minor units (rule 3), from the base-vs-native column pair selected by
// currency (nativeNetDebit -- p26.56). recordAmountUSD then converts this native
// figure to USD for the drawdown-pool DECISION only; the stored split amount stays
// native. This MUST match resolveSplit's amount exactly so the pool basis and the
// posted splits agree.
func (b *builder) recordAmount(r Record) (int64, error) {
	exp, ok := b.exponent[r.Currency]
	if !ok {
		return 0, fmt.Errorf("unknown currency %q", r.Currency)
	}
	return nativeNetDebit(r, exp, b.cfg.BaseCurrency)
}

// offsetRtW performs Pass 2 for one currency bucket: it appends, to `splits`, the
// campus-fund offset portions drawn from the bucket's UNRESTRICTED balance-sheet
// splits so the RtW subset sums to zero, dividing a balance-sheet split into an RtW
// portion + unrestricted remainder when it only partially offsets. It returns the
// (possibly extended) split list and the residual RtW amount it could NOT offset
// (0 in the normal case); a nonzero residual falls through to the existing per-fund
// self-heal in postBucket (the rare `[campus-plug]` fallback, spec B).
//
// `pend` is the bucket's resolved pendings in split order (so campus-RtW splits are
// identified without re-deriving Pass 1), aligned 1:1 with the first len(pend)
// entries of `splits`.
func (b *builder) offsetRtW(splits []store.SplitInput, pend []pending) ([]store.SplitInput, int64) {
	if b.res.CampusFundID == nil {
		return splits, 0
	}
	campusID := *b.res.CampusFundID

	// Sum of the RtW campus splits already in this bucket. The offset legs must sum
	// to the negation of this so the campus-fund group nets to zero.
	var rtwSum int64
	for _, p := range pend {
		if p.campusRtW {
			rtwSum += p.split.Amount
		}
	}
	if rtwSum == 0 {
		return splits, 0 // no RtW campus split in this bucket -> nothing to offset
	}
	need := -rtwSum // total the offset portions must add up to

	// Draw from the bucket's UNRESTRICTED balance-sheet (asset/liability) splits, in
	// split order, same side only (a portion's sign must match `need`'s). Divide a
	// split into an RtW portion (retagged to the campus fund) + an unrestricted
	// remainder when it only partially covers `need`. Restricting to nil-fund
	// balance-sheet splits keeps a donor group intact (splitting a donor split would
	// unbalance that group and the store would reject the whole transaction).
	remaining := need
	var extra []store.SplitInput
	for i := range pend {
		if remaining == 0 {
			break
		}
		p := pend[i]
		if p.campusRtW || !p.isBalanceSheet || p.split.FundID != nil {
			continue // only unrestricted balance-sheet splits are offset candidates
		}
		a := p.split.Amount
		if a == 0 || (a > 0) != (remaining > 0) {
			continue // zero or wrong-side: cannot offset `remaining`
		}
		take := a
		if abs64(a) > abs64(remaining) {
			take = remaining // partial: this split more than covers the remaining need
		}
		if take == a {
			// Whole split offsets: retag it to the campus fund in place.
			splits[i].FundID = &campusID
		} else {
			// Partial: shrink the original to its unrestricted remainder and emit a new
			// split carrying the RtW portion on the SAME account. The two portions sum to
			// the original amount, so the bucket's overall zero-sum is preserved; the RtW
			// portion joins the campus-fund group, the remainder stays unrestricted.
			splits[i].Amount = a - take
			extra = append(extra, store.SplitInput{
				AccountID:   splits[i].AccountID,
				Amount:      take,
				FundID:      &campusID,
				Description: splits[i].Description,
				Memo:        splits[i].Memo,
			})
		}
		remaining -= take
	}

	splits = append(splits, extra...)
	// `remaining` (0 normally) is the RtW amount with no balance-sheet offset on the
	// correct side; postBucket's per-fund self-heal plugs it to Opening Balances.
	return splits, remaining
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

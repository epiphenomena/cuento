package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"cuento/internal/store"
)

// transactions groups the source splits by `tid` and posts one cuento
// transaction per single-currency group -- decomposing MULTI-CURRENCY groups into
// paired single-currency transactions through the configured FX Clearing account
// (D3, docs hazard #1). Opening-balance / single-split groups get a counter-leg on
// the per-subsidiary Equity:Opening Balances account (D22, hazard #4/#8). A group
// that still cannot be balanced is SURFACED as a warning, never force-balanced
// (hazard #4) -- the store would reject it anyway (Z1/Z10 enforced on write).
func (b *builder) transactions(ctx context.Context, recs []Record) error {
	if err := b.loadExponents(ctx); err != nil {
		return err
	}

	// Group by tid in first-seen order (deterministic; keeps large compound
	// entries intact -- up to ~188 splits, hazard #5).
	groups := map[string][]Record{}
	var order []string
	for _, r := range recs {
		if contains(b.cfg.SkipCountries, r.Country) {
			continue // consolidation-marker rows dropped entirely (hazard #3)
		}
		if _, ok := groups[r.Tid]; !ok {
			order = append(order, r.Tid)
		}
		groups[r.Tid] = append(groups[r.Tid], r)
	}

	// Pass 1 of the campus (Restore the Way) model: a GLOBAL chronological drawdown
	// pool over every campus revenue/expense split decides, per split, whether it is
	// assigned the restricted fund or overflows to unrestricted (D p26.43). It runs
	// over the SAME skip-filtered `groups` so the (tid, group-index) keys align with
	// the resolve loop below, and BEFORE any posting so postBucket can consult it.
	plan, err := b.buildCampusPlan(groups)
	if err != nil {
		return err
	}
	b.campusPlan = plan

	for _, tid := range order {
		if err := b.postGroup(ctx, tid, groups[tid]); err != nil {
			return err
		}
	}
	return nil
}

// loadExponents caches the minor-unit exponent per currency (D1) so NetDebit can
// scale amounts exactly.
func (b *builder) loadExponents(ctx context.Context) error {
	curs, err := b.st.Currencies(ctx)
	if err != nil {
		return fmt.Errorf("load currencies: %w", err)
	}
	b.exponent = map[string]int{}
	for _, c := range curs {
		b.exponent[c.Code] = int(c.Exponent)
	}
	return nil
}

// pending is a resolved split about to be posted, plus the source metadata the
// FX/opening-balance logic needs (currency, country) and the campus-model flags the
// offset pass needs (D p26.43): campusRtW marks a campus R/E split assigned the
// restricted fund, and isBalanceSheet marks an asset/liability split eligible to
// carry an offset portion.
type pending struct {
	currency       string
	country        string
	split          store.SplitInput
	campusRtW      bool
	isBalanceSheet bool
}

// postGroup converts one tid group into one or more transactions. It resolves
// each source split to a pending split, buckets them by currency, then posts each
// currency bucket as its own transaction (single-currency per D3), routing any
// residual through Opening Balances (opening groups) or FX Clearing (the balancing
// counter-leg of a multi-currency decomposition).
func (b *builder) postGroup(ctx context.Context, tid string, recs []Record) error {
	// Resolve every source split; bucket by currency.
	buckets := map[string][]pending{}
	var curOrder []string
	date := ""
	desc := ""
	for i, r := range recs {
		if date == "" {
			date = r.Dt
		}
		if desc == "" {
			desc = r.Desc
		}
		p, err := b.resolveSplit(tid, i, r)
		if errors.Is(err, errSkip) {
			continue // zero net-debit split: cannot post (amount <> 0 CHECK); drop
		}
		if err != nil {
			return fmt.Errorf("tid %s: %w", tid, err)
		}
		if _, ok := buckets[p.currency]; !ok {
			curOrder = append(curOrder, p.currency)
		}
		buckets[p.currency] = append(buckets[p.currency], p)
	}
	if len(buckets) == 0 {
		return nil // nothing postable (all-zero group)
	}
	sort.Strings(curOrder)

	multi := len(buckets) > 1
	opening := b.isOpening(recs)

	for _, cur := range curOrder {
		bucket := buckets[cur]
		// Per-subsidiary import: post only the bucket(s) whose subsidiary is the
		// import target. `multi` above was computed over the FULL group, so a
		// cross-currency intercompany transfer (legs in two subsidiaries) still
		// decomposes through FX Clearing (D3) instead of misrouting the lone
		// remaining leg to Opening Balances. Empty importSub = all subsidiaries
		// (the all-in-one build / scaffold+per-sub-loop path).
		if b.importSub != "" {
			subName, err := b.countryToSub(bucket[0].country)
			if err != nil {
				return fmt.Errorf("tid %s: %w", tid, err)
			}
			if subName != b.importSub {
				continue
			}
		}
		if err := b.postBucket(ctx, tid, cur, date, desc, bucket, multi, opening); err != nil {
			return err
		}
	}
	return nil
}

// postBucket posts one currency bucket as a single transaction. It balances the
// bucket by adding a counter-leg when the source splits do not net to zero:
//   - a MULTI-currency group's per-currency residual goes to FX Clearing (D3);
//   - an OPENING group's residual goes to Equity:Opening Balances (D22);
//   - otherwise a nonzero residual is a genuine imbalance -- SURFACED as a warning
//     and the store lets it through only if it actually balances (else rejects).
func (b *builder) postBucket(
	ctx context.Context,
	tid, currency, date, desc string,
	bucket []pending,
	multi, opening bool,
) error {
	// The transaction's subsidiary: the (single) subsidiary of this currency's
	// splits. All splits in a bucket share a country in a well-formed export; if
	// they diverge we take the first and let the store reject a mismatch (Z11).
	subName, err := b.countryToSub(bucket[0].country)
	if err != nil {
		return fmt.Errorf("tid %s: %w", tid, err)
	}
	subID := b.res.SubsidiaryIDs[subName]

	// Seed the split list from the bucket's resolved splits (positions assigned after
	// the offset pass, which may divide a split). Pass 2 of the campus model then
	// retags/divides the bucket's unrestricted balance-sheet splits so the RtW campus
	// subset nets to zero WITHOUT a plug leg (D p26.43); it aligns 1:1 with `bucket`
	// over the first len(bucket) entries, appending any divided RtW portions.
	splits := make([]store.SplitInput, 0, len(bucket))
	for _, p := range bucket {
		splits = append(splits, p.split)
	}
	splits, _ = b.offsetRtW(splits, bucket)

	// Residual is computed PER FUND, not just over the whole bucket: the store
	// enforces zero-sum WITHIN EACH FUND GROUP (D20/Z10), so a counter-leg that
	// balances the currency total but not each fund would be rejected. Key 0 = the
	// unrestricted (NULL fund) group; fundOf recovers the *int64 for the counter-leg.
	// After offsetRtW the campus-fund group normally nets to zero (no counter-leg);
	// the rare leftover falls through here to a [campus-plug] Opening-Balances leg.
	residual := map[int64]int64{}
	fundOf := map[int64]*int64{}
	var fundOrder []int64 // deterministic counter-leg order
	for _, s := range splits {
		key := int64(0)
		if s.FundID != nil {
			key = *s.FundID
		}
		if _, seen := residual[key]; !seen {
			fundOrder = append(fundOrder, key)
			fundOf[key] = s.FundID
		}
		residual[key] += s.Amount
	}

	// Choose the counter account for this group once (FX Clearing for a
	// multi-currency decomposition; Opening Balances for an opening group or a
	// genuine single-currency imbalance surfaced for review, hazard #4). Counter
	// LEGS are then emitted PER FUND so every fund group nets to zero.
	counterAcct := b.cfg.OpeningBalanceAccount
	switch {
	case multi:
		counterAcct = b.cfg.FXClearingAccount
	case opening:
		counterAcct = b.cfg.OpeningBalanceAccount
	}
	acctID, ok := b.res.AccountIDs[counterAcct]
	if !ok {
		return fmt.Errorf("tid %s: counter account %q not mapped", tid, counterAcct)
	}
	for _, key := range fundOrder {
		r := residual[key]
		if r == 0 {
			continue
		}
		if !multi && !opening {
			// A single-currency, non-opening group whose fund does not net to zero is
			// a genuine source imbalance. Route it to Opening Balances so a human sees
			// a real posting, and WARN -- never nudge amounts onto an existing split.
			// A campus-fund residual carries a distinct "campus-plug" marker so the
			// operator can COUNT how many campus transactions still needed a plug leg
			// after the offset pass (the rare pathological fallback, D p26.43) apart
			// from other imbalances.
			marker := ""
			if b.res.CampusFundID != nil && key == *b.res.CampusFundID {
				marker = " [campus-plug]"
			}
			b.res.Warnings = append(b.res.Warnings,
				fmt.Sprintf("tid %s (%s): fund-%d residual %d minor units routed to %s for review%s",
					tid, currency, key, r, counterAcct, marker))
		}
		splits = append(splits, store.SplitInput{
			AccountID: acctID,
			Amount:    -r, // net-debit counter-leg -> this fund group sums to zero
			FundID:    fundOf[key],
			Position:  int64(len(splits)),
		})
		b.res.splitAccounts[acctID] = true
	}

	if len(splits) < 2 {
		// A lone split with no residual cannot form a double entry; surface it.
		b.res.Warnings = append(b.res.Warnings,
			fmt.Sprintf("tid %s (%s): single balanced split cannot post as a transaction", tid, currency))
		return nil
	}

	// Assign contiguous display positions over the FINAL split set (the offset pass
	// may have divided a split, so positions are normalized here rather than during
	// the seed loop).
	for i := range splits {
		splits[i].Position = int64(i)
	}

	// No memo column in the export -> the transaction-level memo is left BLANK too
	// (p26.22); the per-split descriptions carry the ledger text. The importer mints no
	// payees (p26.16); the payee entity is fully removed as of p26.20.
	_ = desc // desc no longer feeds a memo; retained as a bucket arg for signature stability
	txnID, err := b.st.PostTransaction(ctx, store.PostTransactionInput{
		Date:         date,
		SubsidiaryID: subID,
		Memo:         "",
		Currency:     currency,
		Splits:       splits,
	})
	if err != nil {
		// A group the store rejects (unbalanced overall or per-fund) is surfaced for
		// human review, NOT force-balanced (hazard #4). Continue the import.
		b.res.Warnings = append(b.res.Warnings,
			fmt.Sprintf("tid %s (%s): store rejected transaction: %v", tid, currency, err))
		return nil
	}
	b.res.tidTxns[tid] = append(b.res.tidTxns[tid], txnID)
	for _, s := range splits {
		b.res.splitAccounts[s.AccountID] = true
	}
	return nil
}

// resolveSplit turns one source Record into a pending split: exact net-debit from
// db/cr (rule 3), fund from donor, program from kat (R/E only), functional class
// from kls (expense only). A zero net-debit yields errSkip (amount <> 0 CHECK).
//
// Campus (Restore the Way) fund assignment follows the p26.43 drawdown model, keyed
// by the source split's (tid, group index): a campus R/E split assigned RtW by Pass
// 1 takes the campus fund (OVERRIDING any donor); a campus R/E split that overflowed
// the pool stays UNRESTRICTED (nil fund, donor ignored -- an overspent campus cost
// is an ordinary unrestricted expense). Campus BALANCE-SHEET splits get no fund here;
// they are offset candidates the Pass-2 offsetRtW may retag. The kat->program path is
// unchanged for every campus split (RtW or overflowed): kat still feeds program.
func (b *builder) resolveSplit(tid string, idx int, r Record) (pending, error) {
	exp, ok := b.exponent[r.Currency]
	if !ok {
		return pending{}, fmt.Errorf("unknown currency %q", r.Currency)
	}
	amt, err := NetDebit(r.Db, r.Cr, exp)
	if err != nil {
		return pending{}, err
	}
	if amt == 0 {
		return pending{}, errSkip
	}

	acctID, ok := b.res.AccountIDs[r.Acct]
	if !ok {
		return pending{}, fmt.Errorf("source account %q not mapped", r.Acct)
	}
	acctType := b.acctType[acctID]
	isRE := acctType == "revenue" || acctType == "expense"
	isBalanceSheet := acctType == "asset" || acctType == "liability"

	s := store.SplitInput{AccountID: acctID, Amount: amt}

	// Fund from donor (D20); blank donor = unrestricted (nil).
	if r.Donor != "" {
		if fid, ok := b.res.FundIDs[r.Donor]; ok {
			s.FundID = &fid
		}
	}
	// Campus (Restore the Way) fund, per the p26.43 drawdown model. Only a campus
	// R/E split the chronological pool assigned RtW takes the fund -- overriding the
	// donor. A campus R/E split that OVERFLOWED the pool stays unrestricted (its donor
	// fund is dropped too: an overspent campus cost is an ordinary unrestricted
	// expense). Campus balance-sheet splits get no fund here; Pass-2 offsetRtW may
	// retag a portion of them. A missing plan entry (feature off, or a non-R/E campus
	// split) leaves the donor decision untouched.
	campusRtW := false
	if r.Kat == "campus" && b.res.CampusFundID != nil && isRE {
		if rtw, ok := b.campusPlan[campusKey{tid: tid, idx: idx}]; ok {
			if rtw {
				s.FundID = b.res.CampusFundID
				campusRtW = true
			} else {
				s.FundID = nil // overflowed the pool -> unrestricted (donor dropped)
			}
		}
	}
	// Program, ONLY on revenue/expense splits (the store rejects a program on an
	// A/L/E split, ErrProgramOnBalanceSheet). Resolution, finest-first:
	//   1. the raw `klass` (child program) -- finer AND more correct than kat (a
	//      US-ledger "UPH:Summer Camp" split is a UPH program though its kat is uplam);
	//   2. else the `kat` (parent program);
	//   3. else nothing -> the store defaults from the account (default_program).
	if isRE {
		pname := ""
		if p, ok := b.cfg.ProgramClasses[r.Klass]; ok {
			pname = p
		} else if p, ok := b.cfg.Programs[r.Kat]; ok && r.Kat != "" {
			pname = p
		}
		if pname != "" {
			if pid, ok := b.res.ProgramIDs[pname]; ok {
				s.ProgramID = &pid
			}
		}
	}
	// Functional class from kls (D21), ONLY on expense splits: the store rejects a
	// functional class on a non-expense split (ErrNonExpenseFunction), and the
	// source populates kls on non-expense lines (revenue/asset) too. The store
	// defaults from the account otherwise, so we set it only on expense accounts.
	if acctType == "expense" && r.Kls != "" {
		if fc, ok := b.cfg.FunctionalClasses[r.Kls]; ok {
			s.FunctionalClass = &fc
		}
	}

	// The export has a `desc` column but NO memo column, so desc feeds ONLY the
	// per-split description; memo is left BLANK (p26.22). A split's description is the
	// description of the ledger row that produced it. Synthesized counter-legs (FX
	// Clearing / Opening Balances) have no source row, so they carry an empty description.
	descVal := r.Desc
	if b.anonymize {
		descVal = hashText(r.Desc)
	}
	s.Memo = ""
	s.Description = descVal

	return pending{
		currency:       r.Currency,
		country:        r.Country,
		split:          s,
		campusRtW:      campusRtW,
		isBalanceSheet: isBalanceSheet,
	}, nil
}

// isOpening reports whether a tid group is an opening-balance entry (its typ is in
// the configured opening-balance typs, hazard #8), so its residual is absorbed by
// Equity:Opening Balances rather than treated as an error.
func (b *builder) isOpening(recs []Record) bool {
	for _, r := range recs {
		if contains(b.cfg.OpeningBalanceTyps, r.Typ) {
			return true
		}
	}
	return false
}

// countryToSub maps a source country code to the configured subsidiary name. The
// root country (none configured) falls back to the renamed root subsidiary.
func (b *builder) countryToSub(country string) (string, error) {
	if sc, ok := b.cfg.Subsidiaries[country]; ok {
		return sc.Name, nil
	}
	// Fall back to the root subsidiary for a country with no explicit child.
	return b.cfg.RootSubsidiaryName, nil
}

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
	b.payeeID = map[string]int64{}

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
// FX/opening-balance logic needs (currency, country).
type pending struct {
	currency string
	country  string
	split    store.SplitInput
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
	payeeSrc := ""
	for _, r := range recs {
		if date == "" {
			date = r.Dt
		}
		if desc == "" {
			desc = r.Desc
		}
		if payeeSrc == "" {
			payeeSrc = b.payeeSource(r)
		}
		p, err := b.resolveSplit(r)
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
		if err := b.postBucket(ctx, tid, cur, date, desc, payeeSrc, bucket, multi, opening); err != nil {
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
	tid, currency, date, desc, payeeSrc string,
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

	var splits []store.SplitInput
	// Residual is computed PER FUND, not just over the whole bucket: the store
	// enforces zero-sum WITHIN EACH FUND GROUP (D20/Z10), so a counter-leg that
	// balances the currency total but not each fund would be rejected. Key 0 = the
	// unrestricted (NULL fund) group; fundOf recovers the *int64 for the counter-leg.
	residual := map[int64]int64{}
	fundOf := map[int64]*int64{}
	var fundOrder []int64 // deterministic counter-leg order
	for i, p := range bucket {
		s := p.split
		s.Position = int64(i)
		splits = append(splits, s)

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
			b.res.Warnings = append(b.res.Warnings,
				fmt.Sprintf("tid %s (%s): fund-%d residual %d minor units routed to %s for review",
					tid, currency, key, r, counterAcct))
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

	payeeID, err := b.payee(ctx, payeeSrc)
	if err != nil {
		return err
	}
	memo := desc
	if b.anonymize {
		memo = hashText(desc)
	}

	txnID, err := b.st.PostTransaction(ctx, store.PostTransactionInput{
		Date:         date,
		SubsidiaryID: subID,
		PayeeID:      payeeID,
		Memo:         memo,
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
func (b *builder) resolveSplit(r Record) (pending, error) {
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

	s := store.SplitInput{AccountID: acctID, Amount: amt}

	// Fund from donor (D20); blank donor = unrestricted (nil).
	if r.Donor != "" {
		if fid, ok := b.res.FundIDs[r.Donor]; ok {
			s.FundID = &fid
		}
	}
	// Program from kat (D24), ONLY on revenue/expense splits: the store rejects a
	// program on an A/L/E split (ErrProgramOnBalanceSheet), and the source populates
	// kat on non-R/E lines too. The store defaults program from the account when
	// omitted, so we set it only when we have a mapped kat on an R/E account.
	if isRE && r.Kat != "" {
		if pname, ok := b.cfg.Programs[r.Kat]; ok {
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

	memo := r.Desc
	if b.anonymize {
		memo = hashText(r.Desc)
	}
	s.Memo = memo

	return pending{currency: r.Currency, country: r.Country, split: s}, nil
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

// payeeSource returns the raw payee name for a record from the configured
// source column (Config.PayeeColumn). The default ("") yields no payee -- on real
// data the `desc` memo is long/multi-line and would mint thousands of junk
// payees, so which column is the payee is a p09.4 tuning knob, not a hardcoded
// default.
func (b *builder) payeeSource(r Record) string {
	switch b.cfg.PayeeColumn {
	case "typ":
		return r.Typ
	case "klass":
		return r.Klass
	case "desc":
		return r.Desc
	default:
		return ""
	}
}

// payee finds-or-creates a payee from the configured payee source string. Under
// --anonymize the stored name is hashed so a shareable sample db carries no real
// names. A blank source yields no payee (nil).
func (b *builder) payee(ctx context.Context, src string) (*int64, error) {
	if src == "" {
		return nil, nil
	}
	name := src
	if b.anonymize {
		name = hashText(src)
	}
	if id, ok := b.payeeID[name]; ok {
		return &id, nil
	}
	id, err := b.st.CreatePayee(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create payee: %w", err)
	}
	b.payeeID[name] = id
	return &id, nil
}

package main

import (
	"context"
	"fmt"

	"cuento/internal/store"
)

// corrections posts the config's manual ADJUSTMENT transactions (cfg.Corrections,
// D p26.72) after the source import. Each Correction is a fully-specified,
// self-balancing journal entry the owner's consolidation worksheet requires but
// the mechanical CSV import cannot express (e.g. a year-end in-transit CUTOFF
// correction whose two legs the source books straddle across a fiscal boundary).
//
// An adjustment is NOT special-cased in the store: it posts through
// PostTransaction exactly like an imported transaction, so it is versioned
// (rule 5), passes every ledger invariant (rule 7: single currency + subsidiary,
// per-transaction AND per-fund zero-sum, program/class placement), and is refused
// loudly if mis-specified rather than silently corrupting the ledger. Unlike a
// source group, a Correction's amounts are given EXACTLY (int64 minor units,
// rule 3) and must already balance -- no Opening-Balances/FX-Clearing plug leg is
// synthesized; a residual is the operator's error to fix in the config.
//
// Keys are the same human-readable ones the rest of the mapping uses (subsidiary
// NAME, source_acct, donor, program name), resolved here against the maps the
// build/scaffold already populated (b.res.*). It runs in the all-in-one build
// path AFTER every subsidiary's transactions so any account it touches is live.
func (b *builder) corrections(ctx context.Context) error {
	if len(b.cfg.Corrections) == 0 {
		return nil
	}
	if err := b.loadExponents(ctx); err != nil {
		return err
	}
	for i, c := range b.cfg.Corrections {
		if err := b.postCorrection(ctx, i, c); err != nil {
			return fmt.Errorf("correction %d (%s): %w", i, c.Date, err)
		}
	}
	return nil
}

// postCorrection resolves one Correction's keys to ids and posts it through the
// store. It fails loud on any unknown key or an unbalanced entry (the store's
// zero-sum invariants would reject it anyway; catching it here names the entry).
func (b *builder) postCorrection(ctx context.Context, idx int, c Correction) error {
	subID, ok := b.res.SubsidiaryIDs[c.Subsidiary]
	if !ok {
		return fmt.Errorf("subsidiary %q not configured", c.Subsidiary)
	}
	if c.Currency == "" {
		return fmt.Errorf("currency is required")
	}
	if _, ok := b.exponent[c.Currency]; !ok {
		return fmt.Errorf("unknown currency %q", c.Currency)
	}
	if len(c.Splits) < 2 {
		return fmt.Errorf("needs at least two splits (a balanced double entry)")
	}

	splits := make([]store.SplitInput, 0, len(c.Splits))
	for j, cs := range c.Splits {
		acctID, ok := b.res.AccountIDs[cs.Account]
		if !ok {
			return fmt.Errorf("split %d: account %q not mapped", j, cs.Account)
		}
		if cs.Amount == 0 {
			return fmt.Errorf("split %d: zero amount (a split must move value)", j)
		}
		s := store.SplitInput{
			AccountID: acctID,
			Amount:    cs.Amount,
			Position:  int64(j),
		}
		if cs.Fund != "" {
			// A correction split's fund is a donor key (res.FundIDs, like the rest of
			// the import). The marker-driven campus fund (cfg.CampusFund, D p26.40) is
			// NOT donor-keyed -- it is created under its own name and tagged by kat=campus
			// at import, never by donor -- so it is absent from FundIDs. A correction that
			// reclassifies WITHIN the campus restricted fund (e.g. re-recognizing a campus
			// intercompany balance so it eliminates) must be able to name it, so a split's
			// fund also resolves against the campus fund's configured NAME as a fallback.
			fid, ok := b.res.FundIDs[cs.Fund]
			if !ok && b.cfg.CampusFund != nil && b.res.CampusFundID != nil && cs.Fund == b.cfg.CampusFund.Name {
				fid, ok = *b.res.CampusFundID, true
			}
			if !ok {
				return fmt.Errorf("split %d: fund %q not configured (donor key or campus fund name)", j, cs.Fund)
			}
			s.FundID = &fid
		}
		if cs.Program != "" {
			pid, ok := b.res.ProgramIDs[cs.Program]
			if !ok {
				return fmt.Errorf("split %d: program %q not configured", j, cs.Program)
			}
			s.ProgramID = &pid
		}
		if cs.FunctionalClass != "" {
			fc := cs.FunctionalClass
			s.FunctionalClass = &fc
		}
		desc := cs.Description
		if desc == "" {
			desc = c.Description
		}
		if b.anonymize {
			desc = hashText(desc)
		}
		s.Description = desc
		splits = append(splits, s)
	}

	txnID, err := b.st.PostTransaction(ctx, store.PostTransactionInput{
		Date:         c.Date,
		SubsidiaryID: subID,
		Memo:         c.Memo,
		Currency:     c.Currency,
		Splits:       splits,
	})
	if err != nil {
		// A correction is human-authored and MUST balance -- unlike a source group we
		// do not route a residual to Opening Balances or warn-and-continue. A rejection
		// is an operator error in the config, surfaced loudly so the build fails.
		return fmt.Errorf("store rejected adjustment: %w", err)
	}
	// Record it under a synthetic tid so the operator summary counts it and tests can
	// find the produced transaction id without hardcoding.
	tid := fmt.Sprintf("correction-%d", idx)
	b.res.tidTxns[tid] = append(b.res.tidTxns[tid], txnID)
	for _, s := range splits {
		b.res.splitAccounts[s.AccountID] = true
	}
	return nil
}

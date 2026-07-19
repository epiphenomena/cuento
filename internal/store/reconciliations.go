package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Reconciliation operations (p16.2) -- the bank-statement reconciliation lifecycle
// (D13, and the D20 payoff: a recon spans ALL funds). A reconciliation is per
// (account, currency) and covers ONE bank balance regardless of the fund or
// subsidiary badge a split carries -- a statement is about the account, not the
// fund. These COPY the versioned-entity discipline (p04.2/p07.3/p08.2): every
// mutation runs through the write funnel as ONE change; validation lives inside fn
// on the TX-BOUND q so a rejected op rolls the change row back and leaves no audit
// trace; the live write happens first, then the snapshot-from-live version append.
//
// Design decisions recorded in DECISIONS p16.2:
//   - ONE OPEN recon per (account, currency) at a time (D13): a second concurrent
//     open recon on the same pair is rejected (ErrOpenReconciliationExists). A
//     finalized recon does not block a new one -- that is the statement chain.
//   - splits.reconciliation_id is LIVE-ONLY / NOT versioned (00014). Clearing or
//     unclearing a split UPDATEs that one column and mints NO split version -- the
//     audited reconciliation event is FINALIZATION, recorded on reconciliations
//     (+ its versions twin). SetSplitReconciled STILL goes through the funnel
//     (rule 2): a changes row IS emitted for the tx boundary + actor; only the
//     split-VERSION append is skipped.
//   - Finalize's zero-difference gate MIRRORS ledger Z9 byte-for-byte (same
//     opening lookup + same cleared sum) so that if Finalize succeeds, Z9 passes.

// Typed sentinel errors handlers and tests branch on (errors.Is). Wrapped with %w
// at the call site so errors.Is sees them through the funnel.
var (
	// ErrReconciliationNotFound: the requested reconciliation does not exist.
	ErrReconciliationNotFound = errors.New("store: reconciliation not found")
	// ErrNotReconcilable: the account is not flagged reconcilable (D13) or is
	// inactive -- a reconciliation may only be started on a reconcilable account.
	ErrNotReconcilable = errors.New("store: account is not reconcilable")
	// ErrReconciliationCurrency: the statement currency is unknown or inactive.
	ErrReconciliationCurrency = errors.New("store: reconciliation currency inactive or unknown")
	// ErrOpenReconciliationExists: an open reconciliation already exists for this
	// (account, currency) -- only one may be open at a time (D13).
	ErrOpenReconciliationExists = errors.New("store: an open reconciliation already exists for this account and currency")
	// ErrReconciliationNotOpen: an operation requiring an OPEN recon (toggle a
	// split, finalize) was attempted on a finalized one.
	ErrReconciliationNotOpen = errors.New("store: reconciliation is not open")
	// ErrReconciliationNotFinalized: Reopen was attempted on a recon that is not
	// finalized.
	ErrReconciliationNotFinalized = errors.New("store: reconciliation is not finalized")
	// ErrReconciliationNotLatest: Reopen was attempted on a finalized recon while a
	// LATER finalized recon exists on the same (account, currency) (p16.5). Reopening
	// out of order corrupts the opening chain; reopen the later reconciliation(s)
	// first (reverse-chronological order, newest finalized first).
	ErrReconciliationNotLatest = errors.New("store: a later finalized reconciliation exists; reopen it first")
	// ErrSplitReconAccount: the split's account does not match the recon's account
	// (D13 -- a recon clears splits of ONE account).
	ErrSplitReconAccount = errors.New("store: split account does not match the reconciliation account")
	// ErrSplitReconCurrency: the split's currency (from its txn, D3) does not match
	// the recon's currency (D13 -- a recon is per currency).
	ErrSplitReconCurrency = errors.New("store: split currency does not match the reconciliation currency")
	// ErrSplitDeleted: the split lives on a soft-deleted transaction and cannot be
	// cleared (it is out of the ledger).
	ErrSplitDeleted = errors.New("store: split is on a deleted transaction")
	// ErrReconciliationDifference: Finalize found opening + cleared != statement
	// balance (TestFinalizeRequiresZeroDifference). The statement does not prove.
	ErrReconciliationDifference = errors.New("store: reconciliation does not balance to the statement")
	// ErrSplitReconciled: an UpdateTransaction tried a financial edit (date/amount/
	// account/fund) touching a split cleared in a FINALIZED reconciliation, or the
	// deletion of such a split, or a DeleteTransaction (void) of a transaction with
	// such a split (p16.5). Reopen the recon first (memo/payee edits are allowed).
	ErrSplitReconciled = errors.New("store: split is locked by a finalized reconciliation")
)

// StartReconciliation opens a new reconciliation on (accountID, currency) at the
// given statement date + balance, returning its id. The account must be
// reconcilable + active (ErrNotReconcilable) and the currency active (D13); only
// one OPEN recon may exist per (account, currency) (ErrOpenReconciliationExists).
// The recon spans ALL funds -- splitting by fund is deliberately not a parameter
// (D13/D20). Versioned op='create'. All validation runs inside fn on the tx-bound
// q so a rejection rolls the change back (no audit trace).
func (s *Store) StartReconciliation(ctx context.Context, accountID int64, currency, statementDate string, statementBalance int64) (ids.ReconciliationID, error) {
	if !validDate(statementDate) {
		return 0, ErrBadDate
	}

	var newID ids.ReconciliationID
	_, err := s.write(ctx, "reconciliation.start", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			// Account exists, reconcilable, active (D13).
			acct, err := q.GetAccount(ctx, accountID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotReconcilable
				}
				return fmt.Errorf("load account %d: %w", accountID, err)
			}
			if acct.Reconcilable == 0 || acct.Active == 0 {
				return ErrNotReconcilable
			}

			// Currency exists + active. Accounts are multi-currency (default_currency
			// is a default, not a constraint -- FX Clearing holds USD+MXN), so a recon
			// on any active currency is valid; the statement decides the currency.
			ccy, err := q.GetCurrency(ctx, currency)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrReconciliationCurrency
				}
				return fmt.Errorf("load currency %q: %w", currency, err)
			}
			if ccy.Active == 0 {
				return ErrReconciliationCurrency
			}

			// One OPEN recon per (account, currency) at a time (D13).
			open, err := q.CountOpenReconciliations(ctx, sqlc.CountOpenReconciliationsParams{AccountID: accountID, Currency: currency})
			if err != nil {
				return fmt.Errorf("count open reconciliations: %w", err)
			}
			if open > 0 {
				return ErrOpenReconciliationExists
			}

			id, err := q.InsertReconciliation(ctx, sqlc.InsertReconciliationParams{
				AccountID:        accountID,
				StatementDate:    statementDate,
				StatementBalance: statementBalance,
				Currency:         currency,
			})
			if err != nil {
				return fmt.Errorf("insert reconciliation: %w", err)
			}
			newID = id
			return insertReconciliationVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("start reconciliation: %w", err)
	}
	return newID, nil
}

// SetSplitReconciled clears (on) or unclears (off) a split against a recon. The
// recon must be OPEN (ErrReconciliationNotOpen -- toggling a finalized recon's
// membership is the locked case). When clearing, the split's account must match
// the recon's account (ErrSplitReconAccount) and its currency (from its txn, D3)
// the recon's currency (ErrSplitReconCurrency); a split on a soft-deleted txn
// cannot be cleared (ErrSplitDeleted). This is a LIVE-ONLY column update (00014):
// NO split version is appended -- but it STILL runs through the funnel (rule 2), so
// a changes row anchors the tx boundary + actor.
func (s *Store) SetSplitReconciled(ctx context.Context, reconID ids.ReconciliationID, splitID int64, on bool) error {
	_, err := s.write(ctx, "reconciliation.toggle", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			recon, err := q.GetReconciliation(ctx, reconID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrReconciliationNotFound
				}
				return fmt.Errorf("load reconciliation %d: %w", reconID, err)
			}
			// Toggling is allowed only while the recon is OPEN.
			if recon.Status != "open" {
				return ErrReconciliationNotOpen
			}

			sp, err := q.GetSplitForReconcile(ctx, splitID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrSplitNotFound
				}
				return fmt.Errorf("load split %d: %w", splitID, err)
			}

			if on {
				// Validate account + currency match (D13). A cross-account or
				// cross-currency clearing would violate Z8, so reject it here with a
				// clean typed error before the write.
				if sp.Deleted != 0 {
					return ErrSplitDeleted
				}
				if sp.AccountID != recon.AccountID {
					return ErrSplitReconAccount
				}
				if sp.Currency != recon.Currency {
					return ErrSplitReconCurrency
				}
				return q.SetSplitReconciliation(ctx, sqlc.SetSplitReconciliationParams{
					ReconciliationID: ids.Null(&reconID),
					ID:               splitID,
				})
			}
			// Unclear: NULL the column. Only touch a split actually cleared against
			// THIS recon (clearing off a split cleared elsewhere would silently
			// unclear the wrong recon's split).
			if sp.ReconciliationID.Valid && sp.ReconciliationID.Int64 != int64(reconID) {
				return ErrSplitReconAccount
			}
			return q.SetSplitReconciliation(ctx, sqlc.SetSplitReconciliationParams{
				ReconciliationID: sql.NullInt64{},
				ID:               splitID,
			})
		})
	if err != nil {
		return fmt.Errorf("set split %d reconciled=%t: %w", splitID, on, err)
	}
	return nil
}

// Finalize flips an OPEN reconciliation to finalized once it proves: opening + the
// net-debit sum of its cleared splits must equal its statement_balance (all minor
// units, D2 sign, no flip), else ErrReconciliationDifference. opening is the prior
// FINALIZED recon's statement_balance for the SAME (account, currency), else 0 --
// the statement chain. The computation MIRRORS ledger Z9 byte-for-byte so that a
// finalized recon always passes Z9. Versioned op='update'. After finalize the
// recon's cleared splits are locked (the DB trigger + the store's edit block).
func (s *Store) Finalize(ctx context.Context, reconID ids.ReconciliationID) error {
	_, err := s.write(ctx, "reconciliation.finalize", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			recon, err := q.GetReconciliation(ctx, reconID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrReconciliationNotFound
				}
				return fmt.Errorf("load reconciliation %d: %w", reconID, err)
			}
			if recon.Status != "open" {
				return ErrReconciliationNotOpen
			}

			// opening = prior finalized statement balance for (account, currency),
			// strictly before this recon by (statement_date, id) -- else 0 (Z9's
			// opening subquery, byte-identical).
			opening, err := q.PriorFinalizedStatementBalance(ctx, sqlc.PriorFinalizedStatementBalanceParams{
				AccountID:       recon.AccountID,
				Currency:        recon.Currency,
				StatementDate:   recon.StatementDate,
				StatementDate_2: recon.StatementDate,
				ID:              recon.ID,
			})
			if err != nil {
				return fmt.Errorf("opening balance for reconciliation %d: %w", reconID, err)
			}

			// cleared = net-debit sum of splits cleared against this recon on
			// non-deleted txns (Z9's inner SUM, byte-identical).
			cleared, err := q.ReconciliationClearedSum(ctx, reconID)
			if err != nil {
				return fmt.Errorf("cleared sum for reconciliation %d: %w", reconID, err)
			}

			if opening+cleared != recon.StatementBalance {
				return ErrReconciliationDifference
			}

			if err := q.SetReconciliationStatus(ctx, sqlc.SetReconciliationStatusParams{Status: "finalized", ID: reconID}); err != nil {
				return fmt.Errorf("finalize reconciliation %d: %w", reconID, err)
			}
			return insertReconciliationVersion(ctx, q, changeID, "update", reconID)
		})
	if err != nil {
		return fmt.Errorf("finalize reconciliation %d: %w", reconID, err)
	}
	return nil
}

// Reopen flips a FINALIZED reconciliation back to open (the audited unreconcile,
// D13), so its splits are editable again. Versioned op='update' -- the version row
// names the acting user (TestReopenAudited). ErrReconciliationNotFinalized if it
// was not finalized.
func (s *Store) Reopen(ctx context.Context, reconID ids.ReconciliationID) error {
	_, err := s.write(ctx, "reconciliation.reopen", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			recon, err := q.GetReconciliation(ctx, reconID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrReconciliationNotFound
				}
				return fmt.Errorf("load reconciliation %d: %w", reconID, err)
			}
			if recon.Status != "finalized" {
				return ErrReconciliationNotFinalized
			}
			// In-order reopen (p16.5, Gap 2). A finalized recon may only be reopened when
			// it is the LATEST finalized statement on its (account, currency): reopening an
			// earlier one while a later finalized statement stands would corrupt the
			// opening chain (its edits would land under the later statement). Refuse with
			// ErrReconciliationNotLatest -- the user backs up newest-first.
			later, err := q.HasLaterFinalizedReconciliation(ctx, sqlc.HasLaterFinalizedReconciliationParams{
				AccountID:       recon.AccountID,
				Currency:        recon.Currency,
				StatementDate:   recon.StatementDate,
				StatementDate_2: recon.StatementDate,
				ID:              recon.ID,
			})
			if err != nil {
				return fmt.Errorf("later finalized check for reconciliation %d: %w", reconID, err)
			}
			if later {
				return ErrReconciliationNotLatest
			}
			// One OPEN recon per (account, currency) at a time (D13, p22.5). Reopening
			// this finalized recon while ANOTHER open recon stands on the same
			// (account, currency) would yield two open recons -- a state
			// StartReconciliation refuses to create, with no cuento-check backstop. The
			// recon being reopened is still 'finalized' here (SetReconciliationStatus
			// runs below), so it is not counted; a positive count means a DIFFERENT open
			// recon exists. Refuse with ErrOpenReconciliationExists (same sentinel
			// StartReconciliation uses -- identical semantics).
			open, err := q.CountOpenReconciliations(ctx, sqlc.CountOpenReconciliationsParams{
				AccountID: recon.AccountID, Currency: recon.Currency,
			})
			if err != nil {
				return fmt.Errorf("count open reconciliations for %d: %w", reconID, err)
			}
			if open > 0 {
				return ErrOpenReconciliationExists
			}
			if err := q.SetReconciliationStatus(ctx, sqlc.SetReconciliationStatusParams{Status: "open", ID: reconID}); err != nil {
				return fmt.Errorf("reopen reconciliation %d: %w", reconID, err)
			}
			return insertReconciliationVersion(ctx, q, changeID, "update", reconID)
		})
	if err != nil {
		return fmt.Errorf("reopen reconciliation %d: %w", reconID, err)
	}
	return nil
}

// EditReconciliationStatement edits an OPEN reconciliation's statement date +
// ending (statement) balance (p26.57). The recon must be OPEN
// (ErrReconciliationNotOpen -- a finalized/discarded statement's figures are fixed);
// the date must be the loose YYYY-MM-DD shape (ErrBadDate). The opening balance is
// DERIVED from the prior finalized statement (the chain), not stored, so it is not a
// parameter. Versioned op='update': the snapshot append reflects the new figures, so
// the workspace summary (and thus the finalize-until-zero gate) recomputes against
// them. All validation runs inside fn on the tx-bound q so a rejection rolls the
// change back (no audit trace).
func (s *Store) EditReconciliationStatement(ctx context.Context, reconID ids.ReconciliationID, statementDate string, statementBalance int64) error {
	if !validDate(statementDate) {
		return ErrBadDate
	}
	_, err := s.write(ctx, "reconciliation.edit", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			recon, err := q.GetReconciliation(ctx, reconID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrReconciliationNotFound
				}
				return fmt.Errorf("load reconciliation %d: %w", reconID, err)
			}
			// Editing the statement is allowed only while the recon is OPEN.
			if recon.Status != "open" {
				return ErrReconciliationNotOpen
			}
			if err := q.SetReconciliationStatement(ctx, sqlc.SetReconciliationStatementParams{
				StatementDate:    statementDate,
				StatementBalance: statementBalance,
				ID:               reconID,
			}); err != nil {
				return fmt.Errorf("edit reconciliation %d statement: %w", reconID, err)
			}
			return insertReconciliationVersion(ctx, q, changeID, "update", reconID)
		})
	if err != nil {
		return fmt.Errorf("edit reconciliation %d: %w", reconID, err)
	}
	return nil
}

// DiscardReconciliation soft-abandons an OPEN reconciliation (p26.58, RULE 14: audit
// sacred -- NO hard delete). It flips status open -> 'discarded' (a NEW status value,
// 00023) and RELEASES every split it had cleared (reconciliation_id -> NULL) so those
// splits are available to a future reconciliation. The recon must be OPEN
// (ErrReconciliationNotOpen -- a finalized recon must be reopened first, and a
// discarded one is already discarded). Both effects run in ONE change through the
// funnel: the un-clear is a LIVE-ONLY column update (00014) minting no split version;
// the status flip appends a reconciliations version op='update'. NO row is deleted,
// anywhere -- the recon, its versions, its changes all remain (audit intact). A
// discarded recon is excluded from every status='open'/status='finalized' predicate
// automatically, so it no longer blocks a new open recon on the same (account,
// currency) (ErrOpenReconciliationExists passes), leaves the continue/open list, and
// is ignored by the opening chain + Z9. All validation runs inside fn on the tx-bound
// q so a rejection rolls the change back (no audit trace).
func (s *Store) DiscardReconciliation(ctx context.Context, reconID ids.ReconciliationID) error {
	_, err := s.write(ctx, "reconciliation.discard", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			recon, err := q.GetReconciliation(ctx, reconID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrReconciliationNotFound
				}
				return fmt.Errorf("load reconciliation %d: %w", reconID, err)
			}
			// Discarding is allowed only while the recon is OPEN.
			if recon.Status != "open" {
				return ErrReconciliationNotOpen
			}
			// Release the recon's cleared splits FIRST (live-only, no split version).
			// The recon is still 'open' here, so the finalized-lock trigger never fires.
			if err := q.UnclearReconciliationSplits(ctx, ids.Null(&reconID)); err != nil {
				return fmt.Errorf("unclear splits for reconciliation %d: %w", reconID, err)
			}
			if err := q.SetReconciliationStatus(ctx, sqlc.SetReconciliationStatusParams{Status: "discarded", ID: reconID}); err != nil {
				return fmt.Errorf("discard reconciliation %d: %w", reconID, err)
			}
			return insertReconciliationVersion(ctx, q, changeID, "update", reconID)
		})
	if err != nil {
		return fmt.Errorf("discard reconciliation %d: %w", reconID, err)
	}
	return nil
}

// ReconciliationSummary is the workspace's sticky summary (p16.3): the recon's
// statement balance, the opening balance (prior finalized statement, the chain),
// the cleared total (net-debit sum of this recon's cleared splits), and the
// resulting difference = statement - (opening + cleared). Difference == 0 is the
// finalize gate. All net-debit signed minor units (D2).
type ReconciliationSummary struct {
	StatementBalance int64
	Opening          int64
	Cleared          int64
	Difference       int64
}

// ReconciliationWorkspaceSplit is one split in the workspace list: the split's
// financial fields plus its txn context (date/subsidiary/description/memo) and whether
// it is currently cleared against this recon. It carries RAW values; the web layer
// formats them (rule 10) and resolves names.
type ReconciliationWorkspaceSplit struct {
	SplitID      int64
	TxnID        int64
	Amount       int64 // net-debit signed minor units (D2)
	FundID       *ids.FundID
	SubsidiaryID ids.SubsidiaryID
	SplitMemo    string
	Description  string // per-split free-text (p26.15); the workspace Description cell
	TxnMemo      string
	Date         string // raw YYYY-MM-DD
	Cleared      bool   // reconciliation_id == this recon
}

// ReconciliationSummaryFor computes the workspace summary for reconID. It REUSES the
// EXACT two queries Finalize uses -- PriorFinalizedStatementBalance (opening) and
// ReconciliationClearedSum (cleared) -- so the workspace's difference (and thus the
// finalize-disabled-until-zero gate) is byte-identical to Finalize's own zero-check:
// a difference of 0 here means Finalize will accept, a nonzero means it will reject
// with ErrReconciliationDifference. This is what makes the disabled button
// server-authoritative rather than a divergent client guess.
func (s *Store) ReconciliationSummaryFor(ctx context.Context, reconID ids.ReconciliationID) (ReconciliationSummary, error) {
	recon, err := s.q.GetReconciliation(ctx, reconID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReconciliationSummary{}, ErrReconciliationNotFound
		}
		return ReconciliationSummary{}, fmt.Errorf("store: reconciliation summary %d: %w", reconID, err)
	}
	opening, err := s.q.PriorFinalizedStatementBalance(ctx, sqlc.PriorFinalizedStatementBalanceParams{
		AccountID:       recon.AccountID,
		Currency:        recon.Currency,
		StatementDate:   recon.StatementDate,
		StatementDate_2: recon.StatementDate,
		ID:              recon.ID,
	})
	if err != nil {
		return ReconciliationSummary{}, fmt.Errorf("store: reconciliation summary opening %d: %w", reconID, err)
	}
	cleared, err := s.q.ReconciliationClearedSum(ctx, reconID)
	if err != nil {
		return ReconciliationSummary{}, fmt.Errorf("store: reconciliation summary cleared %d: %w", reconID, err)
	}
	return ReconciliationSummary{
		StatementBalance: recon.StatementBalance,
		Opening:          opening,
		Cleared:          cleared,
		Difference:       recon.StatementBalance - (opening + cleared),
	}, nil
}

// ReconciliationWorkspaceSplits returns the splits shown in an OPEN recon's
// workspace: every non-deleted split on the recon's account in the recon's currency
// that is UNCLEARED or cleared against THIS recon (splits cleared in a prior
// finalized recon are excluded -- they are folded into the opening balance). Ordered
// chronologically. The account + currency come from the recon row, so the caller
// only needs the recon id.
func (s *Store) ReconciliationWorkspaceSplits(ctx context.Context, reconID ids.ReconciliationID) ([]ReconciliationWorkspaceSplit, error) {
	recon, err := s.q.GetReconciliation(ctx, reconID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReconciliationNotFound
		}
		return nil, fmt.Errorf("store: reconciliation workspace splits %d: %w", reconID, err)
	}
	rows, err := s.q.WorkspaceSplits(ctx, sqlc.WorkspaceSplitsParams{
		AccountID:        recon.AccountID,
		Currency:         recon.Currency,
		ReconciliationID: ids.Null(&reconID),
	})
	if err != nil {
		return nil, fmt.Errorf("store: reconciliation workspace splits %d: %w", reconID, err)
	}
	out := make([]ReconciliationWorkspaceSplit, len(rows))
	for i, r := range rows {
		out[i] = ReconciliationWorkspaceSplit{
			SplitID:      r.ID,
			TxnID:        r.TransactionID,
			Amount:       r.Amount,
			FundID:       ids.Ptr[ids.FundID](r.FundID),
			SubsidiaryID: r.SubsidiaryID,
			SplitMemo:    r.Memo,
			Description:  r.Description,
			TxnMemo:      r.TxnMemo,
			Date:         r.Date,
			Cleared:      r.ReconciliationID.Valid && r.ReconciliationID.Int64 == int64(reconID),
		}
	}
	return out, nil
}

// ReconcilableAccount is one row of the recon LIST source: an active reconcilable
// account's id and its default (statement) currency.
type ReconcilableAccount struct {
	ID              int64
	DefaultCurrency string
}

// ReconcilableAccounts lists every active account flagged reconcilable (D13) -- the
// recon LIST source. A reconciliation may only start on one of these.
func (s *Store) ReconcilableAccounts(ctx context.Context) ([]ReconcilableAccount, error) {
	rows, err := s.q.ListReconcilableAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list reconcilable accounts: %w", err)
	}
	out := make([]ReconcilableAccount, len(rows))
	for i, r := range rows {
		out[i] = ReconcilableAccount{ID: r.ID, DefaultCurrency: r.DefaultCurrency}
	}
	return out, nil
}

// ReconciliationsForAccount returns every reconciliation on an account (both
// currencies, open + finalized), newest statement first -- the recon LIST uses this
// to find the last finalized statement (opening prefill) and any open recon (a
// "continue" link) per currency.
func (s *Store) ReconciliationsForAccount(ctx context.Context, accountID int64) ([]sqlc.Reconciliation, error) {
	rows, err := s.q.ListReconciliationsForAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: reconciliations for account %d: %w", accountID, err)
	}
	return rows, nil
}

// ReconciliationStatementSplit is one INCLUDED (cleared) split of a finalized
// reconciliation, for the p16.4 statement report: the split's financial fields plus
// its txn context (date/subsidiary/description/memo). RAW values (the report formats
// them, rule 10). Mirrors ReconciliationWorkspaceSplit but without the Cleared flag
// (these are all cleared, by definition of the query).
type ReconciliationStatementSplit struct {
	SplitID      int64
	TxnID        int64
	Amount       int64 // net-debit signed minor units (D2)
	FundID       *ids.FundID
	SubsidiaryID ids.SubsidiaryID
	SplitMemo    string
	Description  string // per-split free-text (p26.15); the statement Description cell
	TxnMemo      string
	Date         string // raw YYYY-MM-DD
}

// ReconciliationStatementSplits returns the INCLUDED (cleared) splits of a
// reconciliation -- every split cleared against it on a non-deleted transaction, in
// chronological (date, split id) order. The predicate is byte-identical to
// ReconciliationClearedSum's filter, so Sum(these amounts) == the recon's cleared
// total: the p16.4 statement report re-derives opening + Sigma(these) == statement
// balance from THESE rows. It spans ALL funds and ALL subsidiaries (D13/D20) -- the
// cleared set is fully identified by the reconciliation id, so there is no scope.
func (s *Store) ReconciliationStatementSplits(ctx context.Context, reconID ids.ReconciliationID) ([]ReconciliationStatementSplit, error) {
	rows, err := s.q.ReconciliationClearedSplits(ctx, ids.Null(&reconID))
	if err != nil {
		return nil, fmt.Errorf("store: reconciliation statement splits %d: %w", reconID, err)
	}
	out := make([]ReconciliationStatementSplit, len(rows))
	for i, r := range rows {
		out[i] = ReconciliationStatementSplit{
			SplitID:      r.ID,
			TxnID:        r.TransactionID,
			Amount:       r.Amount,
			FundID:       ids.Ptr[ids.FundID](r.FundID),
			SubsidiaryID: r.SubsidiaryID,
			SplitMemo:    r.Memo,
			Description:  r.Description,
			TxnMemo:      r.TxnMemo,
			Date:         r.Date,
		}
	}
	return out, nil
}

// FinalizedReconciliation is one row of the p16.4 HISTORY: a finalized reconciliation
// on an account -- its id, statement date + balance, currency, and the audited
// finalized-at timestamp (the valid_from of the version row that recorded the
// finalize). FinalizedAt is a raw timestamp string (the change's `at`); the web layer
// renders its date portion (money has no datetime formatter, D-p16.4).
type FinalizedReconciliation struct {
	ID               ids.ReconciliationID
	StatementDate    string
	StatementBalance int64
	Currency         string
	FinalizedAt      string
}

// FinalizedReconciliationsForAccount returns every FINALIZED reconciliation on an
// account (both currencies), newest statement first -- the p16.4 history / audit trail
// of completed reconciliations. Each carries the finalized-at timestamp from its
// version twin (the finalize event's valid_from). An account with none returns an
// empty slice.
func (s *Store) FinalizedReconciliationsForAccount(ctx context.Context, accountID int64) ([]FinalizedReconciliation, error) {
	rows, err := s.q.FinalizedReconciliationsForAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: finalized reconciliations for account %d: %w", accountID, err)
	}
	out := make([]FinalizedReconciliation, len(rows))
	for i, r := range rows {
		out[i] = FinalizedReconciliation{
			ID:               r.ID,
			StatementDate:    r.StatementDate,
			StatementBalance: r.StatementBalance,
			Currency:         r.Currency,
			FinalizedAt:      r.FinalizedAt,
		}
	}
	return out, nil
}

// GetReconciliation returns the current live row for one reconciliation (read; sqlc).
func (s *Store) GetReconciliation(ctx context.Context, id ids.ReconciliationID) (sqlc.Reconciliation, error) {
	row, err := s.q.GetReconciliation(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Reconciliation{}, ErrReconciliationNotFound
		}
		return sqlc.Reconciliation{}, fmt.Errorf("store: get reconciliation %d: %w", id, err)
	}
	return row, nil
}

// --- helpers (unexported) -------------------------------------------------

// insertReconciliationVersion appends the reconciliations snapshot-from-live
// version row, hiding the generated positional-param names (ID=change_id,
// ID_2=entity_id). MUST run after the live write.
func insertReconciliationVersion(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, op string, entityID ids.ReconciliationID) error {
	if err := q.InsertReconciliationVersion(ctx, sqlc.InsertReconciliationVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append reconciliation version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

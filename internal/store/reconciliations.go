package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
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
	// deletion of such a split. Reopen the recon first (memo/payee are allowed).
	ErrSplitReconciled = errors.New("store: split is locked by a finalized reconciliation")
)

// StartReconciliation opens a new reconciliation on (accountID, currency) at the
// given statement date + balance, returning its id. The account must be
// reconcilable + active (ErrNotReconcilable) and the currency active (D13); only
// one OPEN recon may exist per (account, currency) (ErrOpenReconciliationExists).
// The recon spans ALL funds -- splitting by fund is deliberately not a parameter
// (D13/D20). Versioned op='create'. All validation runs inside fn on the tx-bound
// q so a rejection rolls the change back (no audit trace).
func (s *Store) StartReconciliation(ctx context.Context, accountID int64, currency, statementDate string, statementBalance int64) (int64, error) {
	if !validDate(statementDate) {
		return 0, ErrBadDate
	}

	var newID int64
	_, err := s.write(ctx, "reconciliation.start", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
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
func (s *Store) SetSplitReconciled(ctx context.Context, reconID, splitID int64, on bool) error {
	_, err := s.write(ctx, "reconciliation.toggle", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
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
					ReconciliationID: sql.NullInt64{Int64: reconID, Valid: true},
					ID:               splitID,
				})
			}
			// Unclear: NULL the column. Only touch a split actually cleared against
			// THIS recon (clearing off a split cleared elsewhere would silently
			// unclear the wrong recon's split).
			if sp.ReconciliationID.Valid && sp.ReconciliationID.Int64 != reconID {
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
func (s *Store) Finalize(ctx context.Context, reconID int64) error {
	_, err := s.write(ctx, "reconciliation.finalize", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
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
func (s *Store) Reopen(ctx context.Context, reconID int64) error {
	_, err := s.write(ctx, "reconciliation.reopen", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
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

// GetReconciliation returns the current live row for one reconciliation (read; sqlc).
func (s *Store) GetReconciliation(ctx context.Context, id int64) (sqlc.Reconciliation, error) {
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
func insertReconciliationVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertReconciliationVersion(ctx, sqlc.InsertReconciliationVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append reconciliation version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

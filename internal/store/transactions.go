package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Transaction operations (p08.2) -- the CORE financial logic (D2, D18, D20, D21,
// D24). A transaction is a single-currency, single-subsidiary journal entry whose
// splits sum to zero OVERALL and WITHIN EACH FUND GROUP (D2/D20). These COPY the
// versioned-entity discipline (p04.2/p05.2/p07.3): every mutation runs through the
// write funnel as ONE change; the live write happens first, then the
// snapshot-from-live version append; validation lives inside fn on the TX-BOUND q
// so a rejected op rolls the change row back and leaves no audit trace.
//
// CRITICAL (per the step guidance): validation reads accounts/funds/programs on the
// SAME tx-bound q that inserts, NOT on s.q before write(). Reading on the base
// queries then calling write() passes every listed test but is a TOCTOU race -- an
// account deactivated or a fund closed between validate and insert would slip
// through. Load-and-validate on the inserting tx closes that window.

// Typed sentinel errors handlers and tests branch on (errors.Is). Wrapped with %w
// at the call site so errors.Is sees them through the funnel.
var (
	// ErrTooFewSplits: a transaction needs >= 2 splits (double-entry).
	ErrTooFewSplits = errors.New("store: transaction needs at least two splits")
	// ErrBadDate: the date is not a YYYY-MM-DD calendar date.
	ErrBadDate = errors.New("store: transaction date must be YYYY-MM-DD")
	// ErrInactiveCurrency: the txn currency does not exist or is inactive.
	ErrInactiveCurrency = errors.New("store: transaction currency inactive or unknown")
	// ErrInactiveSubsidiary: the txn subsidiary does not exist or is inactive.
	ErrInactiveSubsidiary = errors.New("store: transaction subsidiary inactive or unknown")
	// ErrPlaceholderAccount: a split account is a placeholder (has children), D11.
	ErrPlaceholderAccount = errors.New("store: split account is a placeholder")
	// ErrInactiveAccount: a split account is inactive.
	ErrInactiveAccount = errors.New("store: split account is inactive")
	// ErrAccountNotInSubsidiary: a split account is not mapped to the txn's sub (D18).
	ErrAccountNotInSubsidiary = errors.New("store: split account not mapped to the transaction's subsidiary")
	// ErrInactiveFund: a split's fund is inactive.
	ErrInactiveFund = errors.New("store: split fund is inactive")
	// ErrFundSubsidiaryScope: the txn's sub is not in the split fund's scope (D20/Z13).
	ErrFundSubsidiaryScope = errors.New("store: split fund not scoped to the transaction's subsidiary")
	// ErrInactiveProgram: the resolved program on an R/E split is inactive.
	ErrInactiveProgram = errors.New("store: split program is inactive")
	// ErrProgramOnBalanceSheet: an A/L/E split carries a program (must be NULL, D24).
	ErrProgramOnBalanceSheet = errors.New("store: program set on a balance-sheet split")
	// ErrFundProgramScope: an R/E split's program is outside its fund's program
	// subtree, when the fund has a program scope (D20).
	ErrFundProgramScope = errors.New("store: split program outside the fund's program scope")
	// ErrExpenseNeedsFunction: an expense split has no functional class and the
	// account carries no default (D21). The real content of TestPostExpenseRequiresFunction.
	ErrExpenseNeedsFunction = errors.New("store: expense split needs a functional class")
	// ErrNonExpenseFunction: a non-expense split carries a functional class (D21).
	ErrNonExpenseFunction = errors.New("store: functional class set on a non-expense split")
	// ErrUnbalanced: the splits do not sum to zero overall (D2).
	ErrUnbalanced = errors.New("store: transaction does not balance to zero")
	// ErrFundUnbalanced: some fund group does not sum to zero (D20). Checked AFTER
	// ErrUnbalanced (per-fund-zero implies overall-zero, so overall trips first).
	ErrFundUnbalanced = errors.New("store: transaction does not balance to zero within a fund")
	// ErrTransactionNotFound: the requested transaction does not exist.
	ErrTransactionNotFound = errors.New("store: transaction not found")
	// ErrSplitNotFound: an UpdateTransaction input carried a split id not on this txn.
	ErrSplitNotFound = errors.New("store: split id not on this transaction")
	// ErrDuplicateSplitID: an UpdateTransaction input carried the SAME existing split
	// id twice. Both copies pass the zero-sum check together, but the update loop would
	// apply UpdateSplit twice to one live row (last-write-wins), so the persisted rows
	// would not sum to zero -- an unbalanced commit. Rejected before any write so the
	// change rolls back with no audit trace.
	ErrDuplicateSplitID = errors.New("store: existing split id appears more than once")
	// ErrAccountMissing: a split references a non-existent account.
	ErrAccountMissing = errors.New("store: split account not found")
	// ErrFundMissing: a split references a non-existent fund.
	ErrFundMissing = errors.New("store: split fund not found")
	// ErrProgramMissing: a split references a non-existent program.
	ErrProgramMissing = errors.New("store: split program not found")
)

// SplitInput is one desired split line. FundID/ProgramID/FunctionalClass are
// optional (nil = unset); the store DEFAULTS program (R/E) and functional class
// (expense) from the account before insert so the triggers never fire on the happy
// path. On UpdateTransaction, ID identifies an existing split to diff against (nil
// = a new split to insert).
type SplitInput struct {
	ID              *int64
	AccountID       int64
	Amount          int64
	FundID          *ids.FundID
	ProgramID       *int64
	FunctionalClass *string
	Memo            string
	Description     string // per-split free-text (p26.15; payee->description migration, INERT this step)
	Position        int64
}

// PostTransactionInput is a new transaction and its splits.
type PostTransactionInput struct {
	Date         string
	SubsidiaryID int64
	Memo         string
	Notes        string // longer multiline explanation (p24.2), distinct from split memos
	Currency     string
	Splits       []SplitInput
}

// resolvedSplit is a split after defaulting program/functional class -- the value
// actually written. UpdateTransaction diffs RESOLVED values against the live split
// (not raw input) so an omitted-but-already-defaulted field counts as unchanged.
type resolvedSplit struct {
	id              *int64 // existing split id, if any (update diff)
	accountID       int64
	amount          int64
	fundID          sql.NullInt64
	programID       sql.NullInt64
	functionalClass sql.NullString
	memo            string
	description     string
	position        int64
}

// PostTransaction validates and inserts a transaction + its splits under ONE
// change, returning the new transaction id. All validation runs inside fn on the
// tx-bound q; a rejection rolls the change back (no audit trace).
func (s *Store) PostTransaction(ctx context.Context, in PostTransactionInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "transaction.post", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			id, err := s.postTransactionTx(ctx, q, changeID, in)
			if err != nil {
				return err
			}
			newID = id
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("post transaction: %w", err)
	}
	return newID, nil
}

// postTransactionTx validates and inserts a transaction + its splits on an
// ALREADY-OPEN tx-bound q under changeID, returning the new transaction id. It is
// the body of PostTransaction, extracted so a caller inside the SAME funnel change
// (p17.3 PostImportRow, which then LINKS the staged row to the created txn) can
// create the balanced ledger transaction and link the row atomically -- one change,
// no window in which a posted txn exists with a still-pending import row (which would
// double-post on retry). All validation runs on q, so a rejection rolls the caller's
// change back.
func (s *Store) postTransactionTx(ctx context.Context, q *sqlc.Queries, changeID int64, in PostTransactionInput) (int64, error) {
	resolved, err := s.validateAndResolve(ctx, q, in, nil)
	if err != nil {
		return 0, err
	}

	txnID, err := q.InsertTransaction(ctx, sqlc.InsertTransactionParams{
		Date:         in.Date,
		SubsidiaryID: in.SubsidiaryID,
		Memo:         in.Memo,
		Notes:        in.Notes,
		Currency:     in.Currency,
	})
	if err != nil {
		return 0, fmt.Errorf("insert transaction: %w", err)
	}
	if err := insertTransactionVersion(ctx, q, changeID, "create", txnID); err != nil {
		return 0, err
	}
	for _, r := range resolved {
		if err := insertSplit(ctx, q, changeID, txnID, r); err != nil {
			return 0, err
		}
	}
	return txnID, nil
}

// UpdateTransaction re-validates the whole input and applies a REPLACE-SET diff by
// split id under one change: a present-and-changed split updates live +
// splits_versions op='update'; a present-and-identical split gets NO version row;
// an existing split absent from the input is deleted live + op='delete'; a split
// with no id is inserted + op='create'. The transaction header always gets an
// op='update' version (the change row anchors the edit), keeping the header as-of
// a clean LIMIT-1 lookup.
func (s *Store) UpdateTransaction(ctx context.Context, id int64, in PostTransactionInput) error {
	_, err := s.write(ctx, "transaction.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetTransaction(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrTransactionNotFound
				}
				return fmt.Errorf("load transaction %d: %w", id, err)
			}

			// Current live splits, keyed by id, for the diff -- loaded BEFORE
			// validation so validateAndResolve can tell, per split, whether its
			// account is UNCHANGED (p26.13: an unchanged now-inactive account stays
			// editable).
			live, err := q.SplitsByTransaction(ctx, id)
			if err != nil {
				return fmt.Errorf("load splits of %d: %w", id, err)
			}
			liveByID := make(map[int64]sqlc.Split, len(live))
			liveAccountByID := make(map[int64]int64, len(live))
			for _, sp := range live {
				liveByID[sp.ID] = sp
				liveAccountByID[sp.ID] = sp.AccountID
			}

			resolved, err := s.validateAndResolve(ctx, q, in, liveAccountByID)
			if err != nil {
				return err
			}

			// STORE-LEVEL split lock (D13, TestEditReconciledTxnBlocked). A split
			// cleared in a FINALIZED reconciliation is frozen: the store refuses any
			// financial edit touching it -- BEFORE the DB trigger fires -- with a
			// clean typed ErrSplitReconciled. The trigger is a BEFORE UPDATE backstop
			// on splits and cannot see a header date/currency change or a split
			// DELETE, so the store must cover those too:
			//   - header DATE change -> blocked (D13's "date" lock lives on the txn);
			//   - header CURRENCY change -> blocked (would break Z8 on a cleared split);
			//   - DELETE of a locked split -> blocked (would silently drop it from the
			//     Z9 statement sum);
			//   - amount/account/fund change of a locked split -> blocked (the trigger's
			//     guarded set); memo/payee/position/program stay editable.
			// Reopen the recon first to make any of these edits.
			locked, err := q.FinalizedReconciledSplitIDs(ctx, id)
			if err != nil {
				return fmt.Errorf("locked splits of %d: %w", id, err)
			}
			if len(locked) > 0 {
				lockedSet := make(map[int64]bool, len(locked))
				for _, sid := range locked {
					lockedSet[sid] = true
				}
				// Header date / currency touch every split's statement provability.
				if in.Date != cur.Date || in.Currency != cur.Currency {
					return ErrSplitReconciled
				}
				// A locked split absent from the input would be deleted below.
				inputIDs := make(map[int64]bool, len(resolved))
				for _, r := range resolved {
					if r.id != nil {
						inputIDs[*r.id] = true
					}
				}
				for sid := range lockedSet {
					if !inputIDs[sid] {
						return ErrSplitReconciled
					}
				}
				// A locked split with a changed financial field (amount/account/fund).
				for _, r := range resolved {
					if r.id == nil || !lockedSet[*r.id] {
						continue
					}
					sp := liveByID[*r.id]
					if sp.Amount != r.amount || sp.AccountID != r.accountID || !nullInt64Eq(sp.FundID, r.fundID) {
						return ErrSplitReconciled
					}
				}
			}

			// Track which existing split ids the input keeps, so we can delete the
			// rest. An input split id that is not on this txn is an error.
			kept := make(map[int64]bool, len(resolved))
			for _, r := range resolved {
				if r.id == nil {
					// New split: insert + op='create'.
					if err := insertSplit(ctx, q, changeID, id, r); err != nil {
						return err
					}
					continue
				}
				sp, ok := liveByID[*r.id]
				if !ok {
					return ErrSplitNotFound
				}
				kept[*r.id] = true
				if splitUnchanged(sp, r) {
					continue // present & identical -> NO version row
				}
				if err := q.UpdateSplit(ctx, sqlc.UpdateSplitParams{
					AccountID:       r.accountID,
					Amount:          r.amount,
					FundID:          r.fundID,
					ProgramID:       r.programID,
					FunctionalClass: r.functionalClass,
					Memo:            r.memo,
					Description:     r.description,
					Position:        r.position,
					ID:              *r.id,
				}); err != nil {
					return fmt.Errorf("update split %d: %w", *r.id, err)
				}
				if err := insertSplitVersion(ctx, q, changeID, "update", *r.id); err != nil {
					return err
				}
			}
			// Deletions: existing splits absent from the input. op='delete' is
			// captured BEFORE the live delete (snapshot-from-live removal order).
			for _, sp := range live {
				if kept[sp.ID] {
					continue
				}
				if err := insertSplitVersion(ctx, q, changeID, "delete", sp.ID); err != nil {
					return err
				}
				if err := q.DeleteSplit(ctx, sp.ID); err != nil {
					return fmt.Errorf("delete split %d: %w", sp.ID, err)
				}
			}

			// Header live update (carry deleted through) + always op='update'.
			if err := q.UpdateTransaction(ctx, sqlc.UpdateTransactionParams{
				Date:         in.Date,
				SubsidiaryID: in.SubsidiaryID,
				Memo:         in.Memo,
				Notes:        in.Notes,
				Currency:     in.Currency,
				Deleted:      cur.Deleted,
				ID:           id,
			}); err != nil {
				return fmt.Errorf("update transaction %d: %w", id, err)
			}
			return insertTransactionVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update transaction %d: %w", id, err)
	}
	return nil
}

// DeleteTransaction soft-deletes a transaction (rule 14): set deleted=1 and append
// ONE transactions_versions op='delete'. The splits are left untouched (no split
// delete-versions) -- the as-of query excludes the txn by its own delete row.
func (s *Store) DeleteTransaction(ctx context.Context, id int64) error {
	_, err := s.write(ctx, "transaction.delete", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetTransaction(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrTransactionNotFound
				}
				return fmt.Errorf("load transaction %d: %w", id, err)
			}
			// STORE-LEVEL void lock (D13/p16.5, TestVoidReconciledTransactionBlocked).
			// The split-lock trigger fires on UPDATE of splits only -- it cannot see a
			// soft-delete of the transaction. Voiding a txn whose split is cleared in a
			// FINALIZED recon would silently drop that split from the recon's balance and
			// break Z9. Mirror UpdateTransaction's lock: reject with the clean typed
			// ErrSplitReconciled BEFORE the soft-delete. Reopen the recon first to void.
			locked, err := q.FinalizedReconciledSplitIDs(ctx, id)
			if err != nil {
				return fmt.Errorf("locked splits of %d: %w", id, err)
			}
			if len(locked) > 0 {
				return ErrSplitReconciled
			}
			if err := q.SoftDeleteTransaction(ctx, id); err != nil {
				return fmt.Errorf("soft-delete transaction %d: %w", id, err)
			}
			return insertTransactionVersion(ctx, q, changeID, "delete", id)
		})
	if err != nil {
		return fmt.Errorf("delete transaction %d: %w", id, err)
	}
	return nil
}

// GetTransaction returns the current LIVE header of one transaction (read; sqlc).
// The transaction editor (p12.2) loads this plus TransactionSplits to prefill the
// edit form; a soft-deleted or missing transaction returns ErrTransactionNotFound so
// the handler can 404. Unlike TransactionAsOf this is the denormalized latest state,
// which is what the editor edits.
func (s *Store) GetTransaction(ctx context.Context, id int64) (sqlc.Transaction, error) {
	row, err := s.q.GetTransaction(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Transaction{}, ErrTransactionNotFound
		}
		return sqlc.Transaction{}, fmt.Errorf("store: get transaction %d: %w", id, err)
	}
	if row.Deleted != 0 {
		return sqlc.Transaction{}, ErrTransactionNotFound
	}
	return row, nil
}

// LedgerDateRange returns the oldest (min) and newest (max) posting dates across ALL
// non-deleted transactions, as ISO strings, plus ok=false when the ledger is EMPTY
// (p29.12). The report param resolver uses it so an OMITTED period bound brackets
// everything: an empty From defaults to the day BEFORE min, an empty To to the day
// AFTER max. Global (all subsidiaries) for simplicity (DECISIONS p29.12). Read-only;
// sqlc. On an empty ledger MIN/MAX yield SQL NULL, surfaced here as ok=false so the
// caller keeps its own fallback (the current year-start/today default).
func (s *Store) LedgerDateRange(ctx context.Context) (min, max string, ok bool, err error) {
	row, err := s.q.LedgerDateRange(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("store: ledger date range: %w", err)
	}
	if !row.MinDate.Valid || !row.MaxDate.Valid {
		return "", "", false, nil
	}
	return row.MinDate.String, row.MaxDate.String, true, nil
}

// SubsidiaryTxnCount returns the number of transactions (including soft-deleted)
// posted to a subsidiary. The historical importer uses it as a per-subsidiary
// idempotency guard: a non-zero count means the subsidiary was already imported,
// so an additive re-import is refused (re-import means a fresh scaffold + import,
// D26). Read-only; sqlc.
func (s *Store) SubsidiaryTxnCount(ctx context.Context, subsidiaryID int64) (int64, error) {
	n, err := s.q.CountTransactionsBySubsidiary(ctx, subsidiaryID)
	if err != nil {
		return 0, fmt.Errorf("store: count transactions for subsidiary %d: %w", subsidiaryID, err)
	}
	return n, nil
}

// AccountSplitRef is one live split on an account (its id + its transaction id),
// returned by SplitsByAccountCurrency for callers that need to enumerate an account's
// splits filtered by transaction currency (e.g. the demo reconciliation seam).
type AccountSplitRef struct {
	ID            int64
	TransactionID int64
}

// SplitsByAccountCurrency returns every live split on accountID whose transaction is
// in `currency` and not soft-deleted, ordered by split id (deterministic). Read-only;
// sqlc. It lets a caller enumerate an account's clearable splits without hand-written
// SQL (rule 2): the demo reconciliation seam uses it to pick the splits to clear.
func (s *Store) SplitsByAccountCurrency(ctx context.Context, accountID int64, currency string) ([]AccountSplitRef, error) {
	rows, err := s.q.SplitsByAccountCurrency(ctx, sqlc.SplitsByAccountCurrencyParams{
		AccountID: accountID,
		Currency:  currency,
	})
	if err != nil {
		return nil, fmt.Errorf("store: splits of account %d in %s: %w", accountID, currency, err)
	}
	out := make([]AccountSplitRef, len(rows))
	for i, r := range rows {
		out[i] = AccountSplitRef{ID: r.ID, TransactionID: r.TransactionID}
	}
	return out, nil
}

// NativeTotal is one (currency, account type) net-debit total for a subsidiary.
type NativeTotal struct {
	Currency string
	Type     string
	Total    int64
}

// SubsidiaryNativeTotals returns the net-debit split totals grouped by currency
// and account type for a subsidiary (non-deleted transactions). The importer uses
// it for per-subsidiary reconciliation: the per-type native trial-balance for the
// operator, and (summed per currency) an insurance check that posted splits net to
// zero. Read-only; sqlc.
func (s *Store) SubsidiaryNativeTotals(ctx context.Context, subsidiaryID int64) ([]NativeTotal, error) {
	rows, err := s.q.SubsidiaryNativeTotals(ctx, subsidiaryID)
	if err != nil {
		return nil, fmt.Errorf("store: subsidiary %d native totals: %w", subsidiaryID, err)
	}
	out := make([]NativeTotal, len(rows))
	for i, r := range rows {
		out[i] = NativeTotal{Currency: r.Currency, Type: r.Type, Total: r.Total}
	}
	return out, nil
}

// TransactionState is a transaction reconstructed as of a time (D4/D5): the header
// plus its split set. Present is false when the txn did not exist (or was deleted)
// at that time.
type TransactionState struct {
	Present      bool
	Date         string
	SubsidiaryID int64
	Memo         string
	Notes        string
	Currency     string
	Splits       []SplitState
}

// SplitState is one split as of a time.
type SplitState struct {
	ID              int64
	AccountID       int64
	Amount          int64
	FundID          sql.NullInt64
	ProgramID       sql.NullInt64
	FunctionalClass sql.NullString
	Memo            string
	Position        int64
}

// TransactionAsOf reconstructs a transaction and its split SET as of `at` from the
// versions tables (D4/D5). The header = the latest transactions_versions with
// valid_from <= at (absent/excluded if op='delete'); each split = its own latest
// splits_versions <= at, excluded if op='delete', for this transaction_id. The
// per-entity latest is resolved in Go from an ordered fetch (like Effective990Codes)
// to keep each SQL param single-use.
func (s *Store) TransactionAsOf(ctx context.Context, id int64, at time.Time) (TransactionState, error) {
	atStr := at.Format(time.RFC3339Nano)

	hdr, err := s.q.TransactionVersionAsOf(ctx, sqlc.TransactionVersionAsOfParams{EntityID: id, ValidFrom: atStr})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TransactionState{Present: false}, nil // did not exist yet
		}
		return TransactionState{}, fmt.Errorf("store: transaction %d as of %s: %w", id, atStr, err)
	}
	if hdr.Op == "delete" {
		return TransactionState{Present: false}, nil
	}

	rows, err := s.q.SplitVersionsAsOf(ctx, sqlc.SplitVersionsAsOfParams{TransactionID: id, ValidFrom: atStr})
	if err != nil {
		return TransactionState{}, fmt.Errorf("store: splits of %d as of %s: %w", id, atStr, err)
	}
	// rows are ordered (entity_id, valid_from DESC, id DESC): the FIRST row per
	// entity_id is that split's latest snapshot as of `at`. Skip op='delete'.
	var splits []SplitState
	seen := make(map[int64]bool)
	for _, r := range rows {
		if seen[r.EntityID] {
			continue
		}
		seen[r.EntityID] = true
		if r.Op == "delete" {
			continue
		}
		splits = append(splits, SplitState{
			ID:              r.EntityID,
			AccountID:       r.AccountID,
			Amount:          r.Amount,
			FundID:          r.FundID,
			ProgramID:       r.ProgramID,
			FunctionalClass: r.FunctionalClass,
			Memo:            r.Memo,
			Position:        r.Position,
		})
	}

	return TransactionState{
		Present:      true,
		Date:         hdr.Date,
		SubsidiaryID: hdr.SubsidiaryID,
		Memo:         hdr.Memo,
		Notes:        hdr.Notes,
		Currency:     hdr.Currency,
		Splits:       splits,
	}, nil
}

// --- validation + resolution (unexported) --------------------------------

// validateAndResolve runs EVERY transaction-level and split-level validation on
// the tx-bound q (no TOCTOU window) and returns the resolved splits (program /
// functional class defaulted). It is shared by Post and Update so their validation
// -- and their defaulting, which the update diff depends on -- is identical.
//
// liveAccountByID maps an EXISTING split id to its persisted account id; Update
// passes it (Post passes nil). A split whose id is present AND whose incoming
// account_id equals the persisted account_id keeps a now-inactive account (p26.13);
// every other split still requires an active account. A map-miss (bogus/absent id)
// -> false: the active-account check applies, and the missing id is caught later as
// ErrSplitNotFound.
func (s *Store) validateAndResolve(ctx context.Context, q *sqlc.Queries, in PostTransactionInput, liveAccountByID map[int64]int64) ([]resolvedSplit, error) {
	if len(in.Splits) < 2 {
		return nil, ErrTooFewSplits
	}
	if !validDate(in.Date) {
		return nil, ErrBadDate
	}

	// Currency exists + active.
	ccy, err := q.GetCurrency(ctx, in.Currency)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInactiveCurrency
		}
		return nil, fmt.Errorf("load currency %q: %w", in.Currency, err)
	}
	if ccy.Active == 0 {
		return nil, ErrInactiveCurrency
	}

	// Subsidiary exists + active (build text requires active; no listed test).
	sub, err := q.GetSubsidiary(ctx, in.SubsidiaryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInactiveSubsidiary
		}
		return nil, fmt.Errorf("load subsidiary %d: %w", in.SubsidiaryID, err)
	}
	if sub.Active == 0 {
		return nil, ErrInactiveSubsidiary
	}

	// Root program, looked up once (the program-defaulting fallback, D24).
	root, err := q.RootProgram(ctx)
	if err != nil {
		return nil, fmt.Errorf("load root program: %w", err)
	}

	resolved := make([]resolvedSplit, 0, len(in.Splits))
	// On an UPDATE (liveAccountByID != nil) reject a repeated EXISTING split id.
	// Both copies count toward the zero-sum below, but the update loop applies
	// UpdateSplit twice to the one live row (last-write-wins), so the persisted
	// rows would not sum to zero -- an unbalanced commit that the zero-sum check
	// cannot catch. Rejecting here, before any write, rolls the change back with no
	// audit trace.
	var seenID map[int64]bool
	if liveAccountByID != nil {
		seenID = make(map[int64]bool, len(in.Splits))
	}
	for i := range in.Splits {
		sp := in.Splits[i]
		if seenID != nil && sp.ID != nil {
			if seenID[*sp.ID] {
				return nil, ErrDuplicateSplitID
			}
			seenID[*sp.ID] = true
		}
		// A pre-existing split whose account is unchanged may keep a now-inactive
		// account (p26.13); a new or account-changed split still requires active.
		allowInactive := sp.ID != nil && liveAccountByID[*sp.ID] == sp.AccountID
		r, err := s.resolveSplit(ctx, q, in.SubsidiaryID, root, sp, allowInactive)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, r)
	}

	// Zero-sum: OVERALL first (per-fund-zero implies overall-zero, so checking
	// overall first keeps the two errors distinguishable), then per FUND GROUP.
	var total int64
	byFund := make(map[int64]int64) // fund_id (0 == unrestricted group) -> sum
	for _, r := range resolved {
		total += r.amount
		key := int64(0)
		if r.fundID.Valid {
			key = r.fundID.Int64
		}
		byFund[key] += r.amount
	}
	if total != 0 {
		return nil, ErrUnbalanced
	}
	for _, sum := range byFund {
		if sum != 0 {
			return nil, ErrFundUnbalanced
		}
	}

	return resolved, nil
}

// resolveSplit validates one split against the tx-bound q and defaults its program
// (R/E) and functional class (expense). The account is loaded ONCE and reused.
//
// allowInactiveAccount relaxes ONLY the inactive-account rejection: it is set by the
// caller for an UPDATE of a pre-existing split whose account is UNCHANGED from the
// persisted split (p26.13), so a historical transaction on a since-deactivated
// account stays editable without reactivating. It is false on create and on any new
// or account-changed split, which still require an active account. No other check
// (missing, placeholder, subsidiary-map, fund, program, functional class) is relaxed.
func (s *Store) resolveSplit(ctx context.Context, q *sqlc.Queries, subID int64, root sqlc.Program, in SplitInput, allowInactiveAccount bool) (resolvedSplit, error) {
	acct, err := q.GetAccount(ctx, in.AccountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resolvedSplit{}, ErrAccountMissing
		}
		return resolvedSplit{}, fmt.Errorf("load account %d: %w", in.AccountID, err)
	}

	// Account is a leaf (D11) -- clean typed error before the trigger fires.
	leaf, err := q.AccountIsLeaf(ctx, sql.NullInt64{Int64: in.AccountID, Valid: true})
	if err != nil {
		return resolvedSplit{}, fmt.Errorf("leaf check %d: %w", in.AccountID, err)
	}
	if !leaf {
		return resolvedSplit{}, ErrPlaceholderAccount
	}
	if acct.Active == 0 && !allowInactiveAccount {
		return resolvedSplit{}, ErrInactiveAccount
	}

	// Account mapped to the txn's subsidiary (D18).
	mapped, err := q.HasAccountSubsidiaryMap(ctx, sqlc.HasAccountSubsidiaryMapParams{AccountID: in.AccountID, SubsidiaryID: subID})
	if err != nil {
		return resolvedSplit{}, fmt.Errorf("account-sub map %d: %w", in.AccountID, err)
	}
	if !mapped {
		return resolvedSplit{}, ErrAccountNotInSubsidiary
	}

	isRE := acct.Type == "revenue" || acct.Type == "expense"
	isExpense := acct.Type == "expense"

	// Fund (if set): active + scoped to the txn's subsidiary (D20). Load once so
	// the fund-program subtree check below can reuse the program scope.
	var fund *sqlc.Fund
	if in.FundID != nil {
		f, err := q.GetFund(ctx, *in.FundID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return resolvedSplit{}, ErrFundMissing
			}
			return resolvedSplit{}, fmt.Errorf("load fund %d: %w", *in.FundID, err)
		}
		if f.Active == 0 {
			return resolvedSplit{}, ErrInactiveFund
		}
		scoped, err := q.HasFundSubsidiaryMap(ctx, sqlc.HasFundSubsidiaryMapParams{FundID: *in.FundID, SubsidiaryID: subID})
		if err != nil {
			return resolvedSplit{}, fmt.Errorf("fund-sub scope %d: %w", *in.FundID, err)
		}
		if !scoped {
			return resolvedSplit{}, ErrFundSubsidiaryScope
		}
		fund = &f
	}

	// Program defaulting (always resolves for R/E; must be NULL for A/L/E).
	var programID sql.NullInt64
	if isRE {
		var pid int64
		switch {
		case in.ProgramID != nil:
			pid = *in.ProgramID
		case acct.DefaultProgramID.Valid:
			pid = acct.DefaultProgramID.Int64
		default:
			pid = root.ID
		}
		prog, err := q.GetProgram(ctx, pid)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return resolvedSplit{}, ErrProgramMissing
			}
			return resolvedSplit{}, fmt.Errorf("load program %d: %w", pid, err)
		}
		if prog.Active == 0 {
			return resolvedSplit{}, ErrInactiveProgram
		}
		// Fund-program scope (R/E only): if the fund has a program scope, the
		// resolved program must be inside that subtree (D20).
		if fund != nil && fund.ProgramID.Valid {
			ids, err := q.ProgramSubtreeIDs(ctx, fund.ProgramID.Int64)
			if err != nil {
				return resolvedSplit{}, fmt.Errorf("fund program subtree %d: %w", fund.ProgramID.Int64, err)
			}
			inScope := false
			for _, sid := range ids {
				if sid == pid {
					inScope = true
					break
				}
			}
			if !inScope {
				return resolvedSplit{}, ErrFundProgramScope
			}
		}
		programID = sql.NullInt64{Int64: pid, Valid: true}
	} else if in.ProgramID != nil {
		return resolvedSplit{}, ErrProgramOnBalanceSheet
	}

	// Functional class defaulting (expense only; CAN fail).
	var functionalClass sql.NullString
	if isExpense {
		switch {
		case in.FunctionalClass != nil && *in.FunctionalClass != "":
			functionalClass = sql.NullString{String: *in.FunctionalClass, Valid: true}
		case acct.FunctionalClass.Valid:
			functionalClass = acct.FunctionalClass
		default:
			return resolvedSplit{}, ErrExpenseNeedsFunction
		}
	} else if in.FunctionalClass != nil && *in.FunctionalClass != "" {
		return resolvedSplit{}, ErrNonExpenseFunction
	}

	return resolvedSplit{
		id:              in.ID,
		accountID:       in.AccountID,
		amount:          in.Amount,
		fundID:          ids.Null(in.FundID),
		programID:       programID,
		functionalClass: functionalClass,
		memo:            in.Memo,
		description:     in.Description,
		position:        in.Position,
	}, nil
}

// --- guard helpers for the deferred p05.2 / p07.3 split checks -----------

// accountSplitInSubsidiary reports whether a non-deleted split on accountID lives
// in a transaction of subsidiary S (completes SetAccountSubsidiaries' p08 guard).
func accountSplitInSubsidiary(ctx context.Context, q *sqlc.Queries, accountID, subID int64) (bool, error) {
	inUse, err := q.SplitUsesAccountInSubsidiary(ctx, sqlc.SplitUsesAccountInSubsidiaryParams{AccountID: accountID, SubsidiaryID: subID})
	if err != nil {
		return false, fmt.Errorf("split-use of account %d in sub %d: %w", accountID, subID, err)
	}
	return inUse, nil
}

// fundSplitInSubsidiary reports whether a non-deleted split tagged fundID lives in
// a transaction of subsidiary S (completes UpdateFund's p08 guard).
func fundSplitInSubsidiary(ctx context.Context, q *sqlc.Queries, fundID ids.FundID, subID int64) (bool, error) {
	inUse, err := q.SplitUsesFundInSubsidiary(ctx, sqlc.SplitUsesFundInSubsidiaryParams{
		FundID: sql.NullInt64{Int64: int64(fundID), Valid: true}, SubsidiaryID: subID,
	})
	if err != nil {
		return false, fmt.Errorf("split-use of fund %d in sub %d: %w", fundID, subID, err)
	}
	return inUse, nil
}

// --- small helpers -------------------------------------------------------

// insertSplit inserts one resolved split live then appends its op='create' version.
func insertSplit(ctx context.Context, q *sqlc.Queries, changeID, txnID int64, r resolvedSplit) error {
	sid, err := q.InsertSplit(ctx, sqlc.InsertSplitParams{
		TransactionID:   txnID,
		AccountID:       r.accountID,
		Amount:          r.amount,
		FundID:          r.fundID,
		ProgramID:       r.programID,
		FunctionalClass: r.functionalClass,
		Memo:            r.memo,
		Description:     r.description,
		Position:        r.position,
	})
	if err != nil {
		return fmt.Errorf("insert split: %w", err)
	}
	return insertSplitVersion(ctx, q, changeID, "create", sid)
}

// splitUnchanged reports whether a live split equals a resolved desired split
// across EVERY business column (NULL-aware). Used by the update diff so an
// untouched split gets no version row.
func splitUnchanged(live sqlc.Split, r resolvedSplit) bool {
	return live.AccountID == r.accountID &&
		live.Amount == r.amount &&
		nullInt64Eq(live.FundID, r.fundID) &&
		nullInt64Eq(live.ProgramID, r.programID) &&
		nullStringEq(live.FunctionalClass, r.functionalClass) &&
		live.Memo == r.memo &&
		live.Description == r.description &&
		live.Position == r.position
}

func nullInt64Eq(a, b sql.NullInt64) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Int64 == b.Int64
}

func nullStringEq(a, b sql.NullString) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.String == b.String
}

// insertTransactionVersion appends the transactions snapshot-from-live version row,
// hiding the generated positional-param names (ID=change_id, ID_2=entity_id).
func insertTransactionVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertTransactionVersion(ctx, sqlc.InsertTransactionVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append transaction version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// insertSplitVersion appends the splits snapshot-from-live version row. For
// op='delete' the caller runs this BEFORE the live DELETE.
func insertSplitVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertSplitVersion(ctx, sqlc.InsertSplitVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append split version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// validDate reports whether date is a real YYYY-MM-DD calendar date. The schema
// GLOB is shape-only; the store validates the actual date so 2025-13-99 is rejected
// with ErrBadDate before the insert.
func validDate(date string) bool {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return false
	}
	return t.Format("2006-01-02") == date
}

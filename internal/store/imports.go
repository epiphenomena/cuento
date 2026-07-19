package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/bankimport"
	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Bank-CSV-import store operations (p17.2): saving a reusable column-mapping
// profile, creating an upload batch (validating the account maps to the chosen
// subsidiary), and staging parsed rows with a precomputed dedupe_hash and a
// duplicate flag. These three tables are NON-VERSIONED operational/staging tables
// (DECISIONS p17.1) -- no *_versions twin -- so, exactly like PutRates and
// SetSplitReconciled, every mutation runs through the write funnel (rule 2: one
// changes row for the tx boundary + actor) but SKIPS the snapshot-from-live version
// append (there is no twin to write). The whole staging of a batch happens in ONE
// write() call so the batch and all its rows are one atomic change.

var (
	// ErrBatchSubsidiaryMismatch: CreateImportBatch was asked to bind an account to
	// a subsidiary the account does not map to (TestBatchSubValidated). The batch's
	// account+subsidiary must be a real account_subsidiaries membership (rule 7:
	// the cross-table check lives in the store, not a trigger).
	ErrBatchSubsidiaryMismatch = errors.New("store: account does not map to that subsidiary")
	// ErrMappingProfileNotFound: the requested mapping profile does not exist.
	ErrMappingProfileNotFound = errors.New("store: mapping profile not found")
	// ErrImportRowNotFound: the requested import row does not exist.
	ErrImportRowNotFound = errors.New("store: import row not found")
	// ErrImportRowNotPending: post/discard was asked of a row that is no longer
	// pending (already posted or discarded). Re-read on the tx-bound q inside the
	// funnel, so a double-submit cannot double-post/re-discard.
	ErrImportRowNotPending = errors.New("store: import row is not pending")
	// ErrDiscardReasonRequired: DiscardImportRow was called with an empty reason.
	// The reason IS the discard's audit (the changes.note), so it is mandatory
	// (TestDiscardRequiresReason). Checked before the funnel opens: nothing written.
	ErrDiscardReasonRequired = errors.New("store: discard requires a reason")
)

// CreateMappingProfile saves a reusable CSV column-mapping and returns its id. The
// bankimport.Config is JSON-encoded into mapping_profiles.config (the store owns the
// shape; the schema stores opaque TEXT). Non-versioned: funnel, no version append.
func (s *Store) CreateMappingProfile(ctx context.Context, name string, cfg bankimport.Config) (ids.MappingProfileID, error) {
	blob, err := json.Marshal(cfg)
	if err != nil {
		return 0, fmt.Errorf("store: marshal mapping config: %w", err)
	}
	var newID ids.MappingProfileID
	_, err = s.write(ctx, "import.profile.create", "",
		func(ctx context.Context, q *sqlc.Queries, _ ids.ChangeID) error {
			id, err := q.InsertMappingProfile(ctx, sqlc.InsertMappingProfileParams{
				Name:   name,
				Config: string(blob),
			})
			if err != nil {
				return fmt.Errorf("insert mapping profile: %w", err)
			}
			newID = id
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("create mapping profile: %w", err)
	}
	return newID, nil
}

// MappingProfile is one saved profile: its id, name, and decoded Config. The web
// layer offers these for reuse in the mapping UI.
type MappingProfile struct {
	ID     ids.MappingProfileID
	Name   string
	Config bankimport.Config
}

// ListMappingProfiles returns the saved profiles (newest first) with their configs
// decoded. A profile whose stored config fails to decode is skipped rather than
// failing the whole list (a hand-corrupted row must not brick the mapping UI).
func (s *Store) ListMappingProfiles(ctx context.Context) ([]MappingProfile, error) {
	rows, err := s.q.ListMappingProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list mapping profiles: %w", err)
	}
	out := make([]MappingProfile, 0, len(rows))
	for _, r := range rows {
		var cfg bankimport.Config
		if err := json.Unmarshal([]byte(r.Config), &cfg); err != nil {
			continue
		}
		out = append(out, MappingProfile{ID: r.ID, Name: r.Name, Config: cfg})
	}
	return out, nil
}

// GetMappingProfile returns one saved profile with its config decoded.
func (s *Store) GetMappingProfile(ctx context.Context, id ids.MappingProfileID) (MappingProfile, error) {
	row, err := s.q.GetMappingProfile(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MappingProfile{}, ErrMappingProfileNotFound
		}
		return MappingProfile{}, fmt.Errorf("store: get mapping profile %d: %w", id, err)
	}
	var cfg bankimport.Config
	if err := json.Unmarshal([]byte(row.Config), &cfg); err != nil {
		return MappingProfile{}, fmt.Errorf("store: decode mapping profile %d: %w", id, err)
	}
	return MappingProfile{ID: row.ID, Name: row.Name, Config: cfg}, nil
}

// DeactivateMappingProfile soft-deletes a saved profile so it stops being offered in
// the load list. It is a DEACTIVATE, not a hard DELETE, because
// import_batches.profile_id is a NOT NULL FK into mapping_profiles and every batch
// references a profile at birth (rule 14 spirit: the mapping that produced a batch is
// its audit and must survive). A missing or already-deactivated id is
// ErrMappingProfileNotFound. Non-versioned: funnel, no version append.
func (s *Store) DeactivateMappingProfile(ctx context.Context, id ids.MappingProfileID) error {
	_, err := s.write(ctx, "import.profile.deactivate", "",
		func(ctx context.Context, q *sqlc.Queries, _ ids.ChangeID) error {
			n, err := q.DeactivateMappingProfile(ctx, id)
			if err != nil {
				return fmt.Errorf("deactivate mapping profile: %w", err)
			}
			if n == 0 {
				return ErrMappingProfileNotFound
			}
			return nil
		})
	if err != nil {
		// The sentinel is preserved through %w so a handler can branch on it.
		return fmt.Errorf("deactivate mapping profile %d: %w", id, err)
	}
	return nil
}

// CreateImportBatch creates an upload batch bound to (accountID, subsidiaryID) and
// returns its id. It VALIDATES that the account MAPS TO the subsidiary
// (ErrBatchSubsidiaryMismatch, TestBatchSubValidated) inside the funnel fn so a
// rejection rolls the change row back and leaves no audit trace. Non-versioned:
// funnel, no version append. uploadedAt is an RFC3339 timestamp string.
func (s *Store) CreateImportBatch(ctx context.Context, filename string, accountID ids.AccountID, subsidiaryID ids.SubsidiaryID, profileID ids.MappingProfileID, uploadedAt string) (ids.ImportBatchID, error) {
	actor, ok := ActorFrom(ctx)
	if !ok {
		return 0, ErrNoActor
	}
	var newID ids.ImportBatchID
	_, err := s.write(ctx, "import.batch.create", "",
		func(ctx context.Context, q *sqlc.Queries, _ ids.ChangeID) error {
			maps, err := q.HasAccountSubsidiaryMap(ctx, sqlc.HasAccountSubsidiaryMapParams{
				AccountID:    accountID,
				SubsidiaryID: subsidiaryID,
			})
			if err != nil {
				return fmt.Errorf("check account-subsidiary map: %w", err)
			}
			if !maps {
				return ErrBatchSubsidiaryMismatch
			}
			id, err := q.InsertImportBatch(ctx, sqlc.InsertImportBatchParams{
				Filename:     filename,
				AccountID:    accountID,
				SubsidiaryID: subsidiaryID,
				ProfileID:    profileID,
				UploadedBy:   actor.ID,
				UploadedAt:   uploadedAt,
			})
			if err != nil {
				return fmt.Errorf("insert import batch: %w", err)
			}
			newID = id
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("create import batch: %w", err)
	}
	return newID, nil
}

// StagedRow is one row staged into a batch: its persisted id and whether it was
// flagged a duplicate. Duplicate is ADVISORY (DECISIONS p17.1) -- the row still
// stages as pending for the p17.3 reviewer; the flag just warns it matches an
// existing pending/posted import row OR an already-posted ledger split on the
// account.
type StagedRow struct {
	ID          ids.ImportRowID
	Duplicate   bool
	DedupeHash  string
	AmountMinor int64
	Date        string
	Description string // bank line descriptive text (was payee; feeds split description)
	Memo        string
}

// StageImportRows stages every parsed row of a batch in ONE write() call (one
// atomic change). For each row it computes dedupe_hash via bankimport.DedupeHash
// (the SAME function the ledger-split key uses) and FLAGS the row a duplicate if its
// (account, hash) matches EITHER an existing pending/posted import row (any prior
// batch) OR an already-posted ledger split on the account. The flag is advisory: a
// duplicate row still stages as pending. The batch's account is authoritative (the
// dedupe scope); the caller passes it so the denormalized import_rows.account_id
// matches the batch.
//
// rows are the bankimport.ParsedRow values from a SUCCESSFUL parse -- callers reject
// the whole upload upstream if ANY row has a parse error (there is no 'error' status
// in the schema; a staged row always carries a parsed date+amount).
func (s *Store) StageImportRows(ctx context.Context, batchID ids.ImportBatchID, accountID ids.AccountID, rows []bankimport.ParsedRow) ([]StagedRow, error) {
	// Build the two duplicate-lookup sets ONCE, outside the write (reads).
	existing, err := s.existingDedupeSet(ctx, accountID)
	if err != nil {
		return nil, err
	}

	out := make([]StagedRow, 0, len(rows))
	_, err = s.write(ctx, "import.rows.stage", "",
		func(ctx context.Context, q *sqlc.Queries, _ ids.ChangeID) error {
			out = out[:0]
			// Within-batch duplicates also flag (a file listing the same line twice).
			seen := make(map[string]bool)
			for _, row := range rows {
				hash := bankimport.DedupeHash(accountID, row.Date, row.AmountMinor, row.Description, row.Memo)
				dup := existing[hash] || seen[hash]
				seen[hash] = true

				rawJSON, err := json.Marshal(row.Raw)
				if err != nil {
					return fmt.Errorf("marshal raw row: %w", err)
				}
				id, err := q.InsertImportRow(ctx, sqlc.InsertImportRowParams{
					BatchID:      batchID,
					AccountID:    accountID,
					RawJson:      string(rawJSON),
					ParsedDate:   nullString(row.Date),
					ParsedAmount: sql.NullInt64{Int64: row.AmountMinor, Valid: true},
					ParsedPayee:  nullString(row.Description),
					ParsedMemo:   nullString(row.Memo),
					DedupeHash:   hash,
				})
				if err != nil {
					return fmt.Errorf("insert import row: %w", err)
				}
				out = append(out, StagedRow{
					ID:          id,
					Duplicate:   dup,
					DedupeHash:  hash,
					AmountMinor: row.AmountMinor,
					Date:        row.Date,
					Description: row.Description,
					Memo:        row.Memo,
				})
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("stage import rows: %w", err)
	}
	return out, nil
}

// existingDedupeSet builds the set of dedupe hashes that already exist on the
// account, from BOTH lookup sources: (1) the hashes of pending/posted import rows
// (across batches), and (2) the natural keys of posted ledger splits on the account,
// re-hashed in Go with the SAME bankimport.DedupeHash. The ledger-split key uses the
// split memo, falling back to the transaction memo when the split memo is empty, and
// the split's per-line description (empty when absent) -- documented in DECISIONS p17.2
// (the description replaces the retired payee name as of p26.20).
func (s *Store) existingDedupeSet(ctx context.Context, accountID ids.AccountID) (map[string]bool, error) {
	set := make(map[string]bool)

	hashes, err := s.q.PendingOrPostedDedupeHashes(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: load pending/posted dedupe hashes: %w", err)
	}
	for _, h := range hashes {
		set[h] = true
	}

	splits, err := s.q.LedgerSplitDedupeKeys(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: load ledger split dedupe keys: %w", err)
	}
	for _, sp := range splits {
		memo := sp.SplitMemo
		if memo == "" {
			memo = sp.TxnMemo
		}
		set[bankimport.DedupeHash(accountID, sp.Date, sp.Amount, sp.Description, memo)] = true
	}
	return set, nil
}

// ImportBatchRow is one persisted staged row as read back for the batch review
// (p17.2 preview-after-stage / p17.3 queue). RAW values; the web layer formats them.
type ImportBatchRow struct {
	ID          ids.ImportRowID
	AmountMinor *int64
	Date        string
	Description string // bank line descriptive text (was payee); parsed_payee column
	Memo        string
	Status      string
	DedupeHash  string
	Duplicate   bool // computed against the batch's account, filled by the caller
	PostedTxnID *ids.TransactionID
}

// ImportRowsForBatch returns every staged row of a batch in stage order. Duplicate
// is NOT set here (it is a cross-account/cross-batch derivation the staging pass
// computed); callers that need it recompute against existingDedupeSet.
func (s *Store) ImportRowsForBatch(ctx context.Context, batchID ids.ImportBatchID) ([]ImportBatchRow, error) {
	rows, err := s.q.ImportRowsByBatch(ctx, batchID)
	if err != nil {
		return nil, fmt.Errorf("store: import rows for batch %d: %w", batchID, err)
	}
	out := make([]ImportBatchRow, len(rows))
	for i, r := range rows {
		out[i] = ImportBatchRow{
			ID:          r.ID,
			AmountMinor: nullInt64ToPtr(r.ParsedAmount),
			Date:        r.ParsedDate.String,
			Description: r.ParsedPayee.String,
			Memo:        r.ParsedMemo.String,
			Status:      r.Status,
			DedupeHash:  r.DedupeHash,
			PostedTxnID: ids.Ptr[ids.TransactionID](r.PostedTransactionID),
		}
	}
	return out, nil
}

// ImportBatch is one upload batch, read for the review queue header (p17.3).
type ImportBatch struct {
	ID           ids.ImportBatchID
	Filename     string
	AccountID    ids.AccountID
	SubsidiaryID ids.SubsidiaryID
}

// GetImportBatch returns one batch. ErrImportRowNotFound is reused for a missing
// batch (the review queue 404s either way).
func (s *Store) GetImportBatch(ctx context.Context, batchID ids.ImportBatchID) (ImportBatch, error) {
	b, err := s.q.GetImportBatch(ctx, batchID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImportBatch{}, ErrImportRowNotFound
		}
		return ImportBatch{}, fmt.Errorf("store: get import batch %d: %w", batchID, err)
	}
	return ImportBatch{ID: b.ID, Filename: b.Filename, AccountID: b.AccountID, SubsidiaryID: b.SubsidiaryID}, nil
}

// ImportRowsForBatchFlagged is ImportRowsForBatch with the advisory Duplicate flag
// recomputed for PENDING rows against the account's existing-dedupe set (the SAME
// two-source set the staging pass used: pending/posted import rows across batches +
// posted ledger splits). A pending row is flagged when its hash appears MORE THAN
// ONCE in that set-with-counts (i.e. besides its own staged occurrence) OR matches a
// posted ledger split -- so a re-uploaded duplicate keeps showing flagged in the
// queue even though the flag was not persisted at stage time. Posted/discarded rows
// are never flagged (their status is decided).
func (s *Store) ImportRowsForBatchFlagged(ctx context.Context, batchID ids.ImportBatchID) ([]ImportBatchRow, error) {
	rows, err := s.ImportRowsForBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}

	batch, err := s.GetImportBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}

	// Count occurrences of each hash among pending/posted import rows on the account,
	// plus the posted-ledger-split keys (each counts as one). A pending row is a
	// duplicate if its hash appears more than once here (another staged/posted row) or
	// at least once from a ledger split.
	counts := make(map[string]int)
	hashes, err := s.q.PendingOrPostedDedupeHashes(ctx, batch.AccountID)
	if err != nil {
		return nil, fmt.Errorf("store: pending/posted dedupe hashes: %w", err)
	}
	for _, h := range hashes {
		counts[h]++
	}
	ledger := make(map[string]bool)
	splits, err := s.q.LedgerSplitDedupeKeys(ctx, batch.AccountID)
	if err != nil {
		return nil, fmt.Errorf("store: ledger split dedupe keys: %w", err)
	}
	for _, sp := range splits {
		memo := sp.SplitMemo
		if memo == "" {
			memo = sp.TxnMemo
		}
		ledger[bankimport.DedupeHash(batch.AccountID, sp.Date, sp.Amount, sp.Description, memo)] = true
	}

	for i := range rows {
		if rows[i].Status != "pending" {
			continue
		}
		h := rows[i].DedupeHash
		if counts[h] > 1 || ledger[h] {
			rows[i].Duplicate = true
		}
	}
	return rows, nil
}

// ImportRow is one staged row read for the review queue (p17.3): the review needs the
// batch's subsidiary (the sub the edit&post editor LOCKS) alongside the row's own
// fields. RAW values; the web layer formats them.
type ImportRow struct {
	ID           ids.ImportRowID
	BatchID      ids.ImportBatchID
	AccountID    ids.AccountID
	SubsidiaryID ids.SubsidiaryID // the batch's subsidiary (locked in the editor)
	AmountMinor  int64
	Date         string
	Description  string // bank line descriptive text (was payee); parsed_payee column
	Memo         string
	Status       string
	PostedTxnID  *ids.TransactionID
}

// GetImportRow returns one staged row joined to its batch (for the batch subsidiary +
// account). ErrImportRowNotFound when the row does not exist.
func (s *Store) GetImportRow(ctx context.Context, rowID ids.ImportRowID) (ImportRow, error) {
	r, err := s.q.GetImportRow(ctx, rowID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImportRow{}, ErrImportRowNotFound
		}
		return ImportRow{}, fmt.Errorf("store: get import row %d: %w", rowID, err)
	}
	row := ImportRow{
		ID:           r.ID,
		BatchID:      r.BatchID,
		AccountID:    r.AccountID,
		SubsidiaryID: r.SubsidiaryID,
		Date:         r.ParsedDate.String,
		Description:  r.ParsedPayee.String,
		Memo:         r.ParsedMemo.String,
		Status:       r.Status,
		PostedTxnID:  ids.Ptr[ids.TransactionID](r.PostedTransactionID),
	}
	if r.ParsedAmount.Valid {
		row.AmountMinor = r.ParsedAmount.Int64
	}
	return row, nil
}

// PostImportRow posts a balanced ledger transaction from a staged row and LINKS the
// row to it, IN ONE change (atomic). in carries the user-adjusted splits from the
// phase-12 editor -- one side the batch account, the counter-splits prefilled via the
// payee template (fund + functional class included); the store's normal
// validateAndResolve enforces the double-entry + per-fund zero-sum (the server is the
// sole validator). The row is re-read on the tx-bound q and must still be 'pending'
// (ErrImportRowNotPending) -- this, not atomicity alone, is what stops a double-submit
// double-posting. Returns the created transaction id.
func (s *Store) PostImportRow(ctx context.Context, rowID ids.ImportRowID, in PostTransactionInput) (ids.TransactionID, error) {
	var txnID ids.TransactionID
	_, err := s.write(ctx, "import.row.post", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			row, err := q.GetImportRow(ctx, rowID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrImportRowNotFound
				}
				return fmt.Errorf("load import row %d: %w", rowID, err)
			}
			if row.Status != "pending" {
				return ErrImportRowNotPending
			}
			id, err := s.postTransactionTx(ctx, q, changeID, in)
			if err != nil {
				return err
			}
			txnID = id
			if err := q.MarkImportRowPosted(ctx, sqlc.MarkImportRowPostedParams{
				PostedTransactionID: ids.Null(&id),
				ID:                  rowID,
			}); err != nil {
				return fmt.Errorf("link import row %d: %w", rowID, err)
			}
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("post import row %d: %w", rowID, err)
	}
	return txnID, nil
}

// DiscardImportRow marks a staged row discarded, recording the REASON as the change's
// note (DECISIONS p17.1: a discarded row's audit is that changes row -- there is no
// discard_reason column). An empty reason is rejected before the funnel opens
// (ErrDiscardReasonRequired: nothing written). The row is re-read on the tx-bound q
// and must still be 'pending' (ErrImportRowNotPending).
func (s *Store) DiscardImportRow(ctx context.Context, rowID ids.ImportRowID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrDiscardReasonRequired
	}
	_, err := s.write(ctx, "import.row.discard", reason,
		func(ctx context.Context, q *sqlc.Queries, _ ids.ChangeID) error {
			row, err := q.GetImportRow(ctx, rowID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrImportRowNotFound
				}
				return fmt.Errorf("load import row %d: %w", rowID, err)
			}
			if row.Status != "pending" {
				return ErrImportRowNotPending
			}
			if err := q.MarkImportRowDiscarded(ctx, rowID); err != nil {
				return fmt.Errorf("discard import row %d: %w", rowID, err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("discard import row %d: %w", rowID, err)
	}
	return nil
}

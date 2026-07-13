package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"cuento/internal/bankimport"
	"cuento/internal/db/sqlc"
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
)

// CreateMappingProfile saves a reusable CSV column-mapping and returns its id. The
// bankimport.Config is JSON-encoded into mapping_profiles.config (the store owns the
// shape; the schema stores opaque TEXT). Non-versioned: funnel, no version append.
func (s *Store) CreateMappingProfile(ctx context.Context, name string, cfg bankimport.Config) (int64, error) {
	blob, err := json.Marshal(cfg)
	if err != nil {
		return 0, fmt.Errorf("store: marshal mapping config: %w", err)
	}
	var newID int64
	_, err = s.write(ctx, "import.profile.create", "",
		func(ctx context.Context, q *sqlc.Queries, _ int64) error {
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
	ID     int64
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
func (s *Store) GetMappingProfile(ctx context.Context, id int64) (MappingProfile, error) {
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

// CreateImportBatch creates an upload batch bound to (accountID, subsidiaryID) and
// returns its id. It VALIDATES that the account MAPS TO the subsidiary
// (ErrBatchSubsidiaryMismatch, TestBatchSubValidated) inside the funnel fn so a
// rejection rolls the change row back and leaves no audit trace. Non-versioned:
// funnel, no version append. uploadedAt is an RFC3339 timestamp string.
func (s *Store) CreateImportBatch(ctx context.Context, filename string, accountID, subsidiaryID, profileID int64, uploadedAt string) (int64, error) {
	actor, ok := ActorFrom(ctx)
	if !ok {
		return 0, ErrNoActor
	}
	var newID int64
	_, err := s.write(ctx, "import.batch.create", "",
		func(ctx context.Context, q *sqlc.Queries, _ int64) error {
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
	ID          int64
	Duplicate   bool
	DedupeHash  string
	AmountMinor int64
	Date        string
	Payee       string
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
func (s *Store) StageImportRows(ctx context.Context, batchID, accountID int64, rows []bankimport.ParsedRow) ([]StagedRow, error) {
	// Build the two duplicate-lookup sets ONCE, outside the write (reads).
	existing, err := s.existingDedupeSet(ctx, accountID)
	if err != nil {
		return nil, err
	}

	out := make([]StagedRow, 0, len(rows))
	_, err = s.write(ctx, "import.rows.stage", "",
		func(ctx context.Context, q *sqlc.Queries, _ int64) error {
			out = out[:0]
			// Within-batch duplicates also flag (a file listing the same line twice).
			seen := make(map[string]bool)
			for _, row := range rows {
				hash := bankimport.DedupeHash(accountID, row.Date, row.AmountMinor, row.Payee, row.Memo)
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
					ParsedPayee:  nullString(row.Payee),
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
					Payee:       row.Payee,
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
// the transaction's payee name (empty when absent) -- documented in DECISIONS p17.2.
func (s *Store) existingDedupeSet(ctx context.Context, accountID int64) (map[string]bool, error) {
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
		set[bankimport.DedupeHash(accountID, sp.Date, sp.Amount, sp.Payee, memo)] = true
	}
	return set, nil
}

// ImportBatchRow is one persisted staged row as read back for the batch review
// (p17.2 preview-after-stage / p17.3 queue). RAW values; the web layer formats them.
type ImportBatchRow struct {
	ID          int64
	AmountMinor *int64
	Date        string
	Payee       string
	Memo        string
	Status      string
	DedupeHash  string
	Duplicate   bool // computed against the batch's account, filled by the caller
	PostedTxnID *int64
}

// ImportRowsForBatch returns every staged row of a batch in stage order. Duplicate
// is NOT set here (it is a cross-account/cross-batch derivation the staging pass
// computed); callers that need it recompute against existingDedupeSet.
func (s *Store) ImportRowsForBatch(ctx context.Context, batchID int64) ([]ImportBatchRow, error) {
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
			Payee:       r.ParsedPayee.String,
			Memo:        r.ParsedMemo.String,
			Status:      r.Status,
			DedupeHash:  r.DedupeHash,
			PostedTxnID: nullInt64ToPtr(r.PostedTransactionID),
		}
	}
	return out, nil
}

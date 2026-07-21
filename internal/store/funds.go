package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Fund operations (p07.3) -- funds are the restricted-fund SPLIT DIMENSION (D20).
// A fund documents a grant/restricted gift and scopes to ONE OR MORE subsidiaries
// via fund_subsidiaries, optionally to a program subtree. These COPY the versioned
// -entity discipline the subsidiary ops (p04.2) established and the COMPOSITE-
// membership versioning the account ops (p05.2) added: every public mutation runs
// through the write funnel as ONE change; inside fn the live write happens first,
// then the snapshot-from-live version append; validation lives inside fn so a
// rejected op rolls the change row back and leaves no audit trace.
//
// Funds are SIMPLER than accounts, and the difference is load-bearing: the
// fund_subsidiaries set is a FLAT set (>=1, store-enforced) -- there is NO
// ancestor propagation and NO parent-set-superset-of-children invariant. So
// memberships are added/removed DIRECTLY (no addSubWithPropagation analogue).
//
// The one place this departs from the funnel's live-write-first rule is a
// membership REMOVAL: the version append is snapshot-FROM-LIVE, so for op='delete'
// the version row MUST be captured BEFORE the live membership is deleted (the row
// must still exist to snapshot). Additions keep the normal live-first order.
//
// Error and method names are DELIBERATELY DISTINCT from the account/subsidiary
// ones (package store is shared).

// Typed sentinel errors handlers and tests branch on (AGENTS Style). Wrapped with
// %w at the call site so errors.Is sees them through the funnel.
var (
	// ErrFundNoSubsidiary: a fund must scope to at least one subsidiary (D20).
	// Checked before the tx on create; enforced after the diff on update.
	ErrFundNoSubsidiary = errors.New("store: fund needs at least one subsidiary")
	// ErrFundNotFound: the requested fund does not exist.
	ErrFundNotFound = errors.New("store: fund not found")
	// ErrFundProgramMissing: an optional program scope was given but the program
	// does not exist (D20 -- the program must exist if set).
	ErrFundProgramMissing = errors.New("store: fund program scope not found")
	// ErrFundSubInUseBySplit: a subsidiary cannot be removed from a fund while a
	// split tagged that fund lives in a non-deleted txn of that subsidiary (p08).
	ErrFundSubInUseBySplit = errors.New("store: fund subsidiary still used by a split")
)

// CreateFundInput is the desired state of a NEW fund. Restriction is one of
// 'purpose' | 'time' | 'perpetual' (schema CHECK). ProgramID is an OPTIONAL
// program-subtree scope (nil = none; must exist if set). StartDate/EndDate are
// optional YYYY-MM-DD strings (nil = none; the schema GLOB validates shape).
// Subsidiaries is the scope set (>=1, else ErrFundNoSubsidiary) -- a FLAT set,
// not propagated.
type CreateFundInput struct {
	Name         string
	NameES       string // optional Spanish name ("" = none; en-fallback at display)
	Funder       string
	Purpose      string
	Restriction  string
	ProgramID    *ids.ProgramID
	StartDate    *string
	EndDate      *string
	Notes        string
	Subsidiaries []ids.SubsidiaryID
}

// UpdateFundInput carries only fields to change (nil = leave as-is). ProgramID
// follows the accounts convention: nil leaves it unchanged, a non-nil 0 CLEARS
// it (NULL), a positive value sets it (validated to exist). Subsidiaries is the
// full DESIRED set: nil leaves the set unchanged, a non-nil slice replaces it
// (diffed against the current set -- adds versioned op='create', removes op=
// 'delete'); the resulting set must still be >=1 (else ErrFundNoSubsidiary).
// StartDate/EndDate follow the same nil/""-clears convention as strings.
type UpdateFundInput struct {
	Name         *string
	NameES       *string // nil = leave as-is
	Funder       *string
	Purpose      *string
	Restriction  *string
	ProgramID    *ids.ProgramID
	StartDate    *string
	EndDate      *string
	Notes        *string
	Subsidiaries []ids.SubsidiaryID
}

// CreateFund creates a fund (+ its subsidiary memberships) under ONE change and
// returns the new id. The fund's create version and each membership's create
// version share that change. A missing subsidiary set (ErrFundNoSubsidiary) is
// rejected before the tx; a missing program scope (ErrFundProgramMissing) inside
// fn so it rolls back cleanly.
func (s *Store) CreateFund(ctx context.Context, in CreateFundInput) (ids.FundID, error) {
	if len(in.Subsidiaries) == 0 {
		return 0, ErrFundNoSubsidiary
	}

	var newID ids.FundID
	_, err := s.write(ctx, "fund.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			// Validate an optional program scope exists (D20). Runs inside fn so a
			// rejection rolls the change back.
			if in.ProgramID != nil {
				if err := checkFundProgram(ctx, q, *in.ProgramID); err != nil {
					return err
				}
			}

			id, err := q.InsertFund(ctx, sqlc.InsertFundParams{
				Name:        in.Name,
				NameEs:      in.NameES,
				Funder:      in.Funder,
				Purpose:     in.Purpose,
				Restriction: in.Restriction,
				ProgramID:   ids.Null(in.ProgramID),
				StartDate:   nullStringPtr(in.StartDate),
				EndDate:     nullStringPtr(in.EndDate),
				Notes:       in.Notes,
				Active:      1,
			})
			if err != nil {
				return fmt.Errorf("insert fund: %w", err)
			}
			newID = id

			if err := insertFundVersion(ctx, q, changeID, "create", id); err != nil {
				return err
			}

			// Flat set: add each membership directly (no propagation). Dedup so a
			// caller passing a repeated sub does not hit the PK twice.
			seen := make(map[ids.SubsidiaryID]bool, len(in.Subsidiaries))
			for _, sid := range in.Subsidiaries {
				if seen[sid] {
					continue
				}
				seen[sid] = true
				if err := addFundSub(ctx, q, changeID, id, sid); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("create fund: %w", err)
	}
	return newID, nil
}

// UpdateFund changes a fund's fields and/or its subsidiary set + program scope
// under one change. The subsidiary-set diff adds new memberships (op='create')
// and removes dropped ones (op='delete', version-before-delete). The set must
// still be >=1 after the change (ErrFundNoSubsidiary). The fund version append
// reflects the NEW field values (it runs after the live update).
func (s *Store) UpdateFund(ctx context.Context, id ids.FundID, in UpdateFundInput) error {
	_, err := s.write(ctx, "fund.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			cur, err := q.GetFund(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrFundNotFound
				}
				return fmt.Errorf("load fund %d: %w", id, err)
			}

			// Read-modify-write: start from current, override provided fields.
			next := cur
			if in.Name != nil {
				next.Name = *in.Name
			}
			if in.NameES != nil {
				next.NameEs = *in.NameES
			}
			if in.Funder != nil {
				next.Funder = *in.Funder
			}
			if in.Purpose != nil {
				next.Purpose = *in.Purpose
			}
			if in.Restriction != nil {
				next.Restriction = *in.Restriction
			}
			if in.Notes != nil {
				next.Notes = *in.Notes
			}
			if in.StartDate != nil {
				next.StartDate = nullString(*in.StartDate)
			}
			if in.EndDate != nil {
				next.EndDate = nullString(*in.EndDate)
			}
			if in.ProgramID != nil {
				// A non-nil 0 clears the scope; a positive value sets it (must
				// exist, D20). Same convention as UpdateAccount's default program.
				if *in.ProgramID == 0 {
					next.ProgramID = sql.NullInt64{}
				} else {
					if err := checkFundProgram(ctx, q, *in.ProgramID); err != nil {
						return err
					}
					next.ProgramID = sql.NullInt64{Int64: int64(*in.ProgramID), Valid: true}
				}
			}

			// Subsidiary-set diff (only when a desired set is supplied). Reject an
			// empty desired set BEFORE any write so the change rolls back.
			if in.Subsidiaries != nil {
				want := make(map[ids.SubsidiaryID]bool, len(in.Subsidiaries))
				for _, sid := range in.Subsidiaries {
					want[sid] = true
				}
				if len(want) == 0 {
					return ErrFundNoSubsidiary
				}
				have, err := fundSubSetTx(ctx, q, id)
				if err != nil {
					return err
				}

				// Removals: drop each sub in have but not want. p08 (completed):
				// block a removal while a split tagged this fund lives in a
				// non-deleted txn of subsidiary S (ErrFundSubInUseBySplit).
				for sid := range have {
					if want[sid] {
						continue
					}
					used, err := fundSplitInSubsidiary(ctx, q, id, sid)
					if err != nil {
						return err
					}
					if used {
						return ErrFundSubInUseBySplit
					}
					if err := removeFundSub(ctx, q, changeID, id, sid); err != nil {
						return err
					}
				}
				// Additions: add each sub in want but not have.
				for sid := range want {
					if have[sid] {
						continue
					}
					if err := addFundSub(ctx, q, changeID, id, sid); err != nil {
						return err
					}
				}
			}

			if err := q.UpdateFund(ctx, sqlc.UpdateFundParams{
				Name:        next.Name,
				NameEs:      next.NameEs,
				Funder:      next.Funder,
				Purpose:     next.Purpose,
				Restriction: next.Restriction,
				ProgramID:   next.ProgramID,
				StartDate:   next.StartDate,
				EndDate:     next.EndDate,
				Notes:       next.Notes,
				Active:      next.Active,
				ID:          id,
			}); err != nil {
				return fmt.Errorf("update fund %d: %w", id, err)
			}
			return insertFundVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update fund %d: %w", id, err)
	}
	return nil
}

// CloseFund sets active=0 (op='update', versioned/audited). It "blocks new use"
// -- but that block is enforced at transaction-post time (p08); here it only
// records the close. Deactivation is NEVER op='delete' (the entity persists, rule
// 14; delete-op is reserved for transaction soft-delete).
func (s *Store) CloseFund(ctx context.Context, id ids.FundID) error {
	return s.setFundActive(ctx, id, 0, "fund.close")
}

// ReopenFund sets active=1 (op='update', audited) -- the inverse of CloseFund.
func (s *Store) ReopenFund(ctx context.Context, id ids.FundID) error {
	return s.setFundActive(ctx, id, 1, "fund.reopen")
}

// setFundActive is the shared close/reopen body: read-modify-write the active
// flag, then append an op='update' version row.
func (s *Store) setFundActive(ctx context.Context, id ids.FundID, active int64, kind string) error {
	_, err := s.write(ctx, kind, "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			cur, err := q.GetFund(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrFundNotFound
				}
				return fmt.Errorf("load fund %d: %w", id, err)
			}
			if err := q.UpdateFund(ctx, sqlc.UpdateFundParams{
				Name:        cur.Name,
				NameEs:      cur.NameEs,
				Funder:      cur.Funder,
				Purpose:     cur.Purpose,
				Restriction: cur.Restriction,
				ProgramID:   cur.ProgramID,
				StartDate:   cur.StartDate,
				EndDate:     cur.EndDate,
				Notes:       cur.Notes,
				Active:      active,
				ID:          id,
			}); err != nil {
				return fmt.Errorf("set fund %d active=%d: %w", id, active, err)
			}
			return insertFundVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("%s %d: %w", kind, id, err)
	}
	return nil
}

// GetFund returns the current live row for one fund (read; sqlc).
func (s *Store) GetFund(ctx context.Context, id ids.FundID) (sqlc.Fund, error) {
	row, err := s.q.GetFund(ctx, id)
	if err != nil {
		return sqlc.Fund{}, fmt.Errorf("store: get fund %d: %w", id, err)
	}
	return row, nil
}

// FundSubsidiaryIDs returns the subsidiary id set a fund currently scopes to (D20),
// subsidiary-id ordered. It is the read the funds workspace (p12.5) uses for the
// list's scope column and to pre-check the edit form's subsidiary checklist --
// mirroring AccountSubsidiaryIDs. Read; sqlc.
func (s *Store) FundSubsidiaryIDs(ctx context.Context, id ids.FundID) ([]ids.SubsidiaryID, error) {
	subs, err := s.q.FundSubsidiaries(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: fund %d subsidiaries: %w", id, err)
	}
	return subs, nil
}

// ActiveFunds returns the ACTIVE funds whose subsidiary scope contains
// subsidiaryID (D20/Q1) -- the transaction editor's fund-option source. Ordered
// by id for a deterministic option list. Read; sqlc.
func (s *Store) ActiveFunds(ctx context.Context, subsidiaryID int64) ([]sqlc.Fund, error) {
	rows, err := s.q.ActiveFunds(ctx, subsidiaryID)
	if err != nil {
		return nil, fmt.Errorf("store: active funds for subsidiary %d: %w", subsidiaryID, err)
	}
	return rows, nil
}

// --- helpers (unexported) -------------------------------------------------

// checkFundProgram validates an optional program scope exists (D20 -- the program
// must exist if set). It runs inside fn on the tx-bound queries so a rejection
// rolls the change back. Existence only: the task requires "must exist if set",
// not active.
func checkFundProgram(ctx context.Context, q *sqlc.Queries, programID ids.ProgramID) error {
	if _, err := q.GetProgram(ctx, programID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFundProgramMissing
		}
		return fmt.Errorf("load fund program %d: %w", programID, err)
	}
	return nil
}

// fundSubSetTx returns a fund's current subsidiary id set on the tx-bound queries.
func fundSubSetTx(ctx context.Context, q *sqlc.Queries, fundID ids.FundID) (map[ids.SubsidiaryID]bool, error) {
	subs, err := q.FundSubsidiaries(ctx, fundID)
	if err != nil {
		return nil, fmt.Errorf("load subsidiaries of fund %d: %w", fundID, err)
	}
	set := make(map[ids.SubsidiaryID]bool, len(subs))
	for _, sid := range subs {
		set[sid] = true
	}
	return set, nil
}

// addFundSub adds membership (fundID, sid) live then versions it op='create'.
// The set is FLAT: no propagation. Live-write-FIRST, then snapshot-from-live.
func addFundSub(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, fundID ids.FundID, sid ids.SubsidiaryID) error {
	if err := q.InsertFundSubsidiary(ctx, sqlc.InsertFundSubsidiaryParams{FundID: fundID, SubsidiaryID: sid}); err != nil {
		return fmt.Errorf("add fund membership (%d,%d): %w", fundID, sid, err)
	}
	if err := q.InsertFundSubsidiaryVersion(ctx, sqlc.InsertFundSubsidiaryVersionParams{
		Op: "create", ID: changeID, FundID: fundID, SubsidiaryID: sid,
	}); err != nil {
		return fmt.Errorf("version fund membership add (%d,%d): %w", fundID, sid, err)
	}
	return nil
}

// removeFundSub deletes membership (fundID, sid) and versions it op='delete'.
//
// REMOVAL INVERTS the live-write-first convention: the version append is
// snapshot-FROM-LIVE, so the version row (the last-known membership) MUST be
// captured BEFORE the live row is deleted, or there is nothing left to snapshot.
// This mirrors accounts.go's removeSub ordering.
func removeFundSub(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, fundID ids.FundID, sid ids.SubsidiaryID) error {
	if err := q.InsertFundSubsidiaryVersion(ctx, sqlc.InsertFundSubsidiaryVersionParams{
		Op: "delete", ID: changeID, FundID: fundID, SubsidiaryID: sid,
	}); err != nil {
		return fmt.Errorf("version fund membership remove (%d,%d): %w", fundID, sid, err)
	}
	if err := q.DeleteFundSubsidiary(ctx, sqlc.DeleteFundSubsidiaryParams{FundID: fundID, SubsidiaryID: sid}); err != nil {
		return fmt.Errorf("remove fund membership (%d,%d): %w", fundID, sid, err)
	}
	return nil
}

// insertFundVersion appends the funds snapshot-from-live version row. It hides
// the generated positional-param names (ID=change_id, ID_2=entity_id) behind one
// call site. MUST run after the live write.
func insertFundVersion(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, op string, entityID ids.FundID) error {
	if err := q.InsertFundVersion(ctx, sqlc.InsertFundVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append fund version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

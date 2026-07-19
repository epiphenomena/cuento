package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
)

// Subsidiary operations (p04.2) — the PATTERN-SETTER for every versioned entity
// op in cuento. Accounts (p05), programs/funds (p07), transactions/splits (p08)
// copy the shape established here:
//
//   1. All mutations go through the write funnel (rule 2). The funnel inserts the
//      changes row FIRST, then runs fn; a validation error returned from fn rolls
//      that change row back, so a rejected op leaves NO audit trace (the "no-trace"
//      tests prove this — validation lives INSIDE fn, never before write()).
//   2. Inside fn, the LIVE write happens first, THEN the version append via the
//      snapshot-from-live query (insertSubsidiaryVersion). Reading the live row
//      after it is written makes the version snapshot byte-identical to the live
//      row (Z3 can never diverge) and valid_from == changes.at by construction —
//      both structural, never asserted.
//   3. Validation reads (cycle/child checks) run on the SAME tx-bound *sqlc.Queries
//      passed into fn, so they see the in-flight write and are transaction-
//      consistent. The public read methods (SubTree/Descendants/GetSubsidiary)
//      wrap the identical sqlc queries on the base (non-tx) Queries.
//
// Composite-key versioned tables reuse insertSubsidiaryVersion's shape verbatim,
// changing only the version query's entity_id/WHERE expression — the store-side
// funnel/order/no-trace discipline is identical.

// Typed sentinel errors handlers and tests branch on (AGENTS Style). Wrapped with
// %w at the call site so errors.Is sees them through the funnel.
var (
	// ErrSecondRoot: a subsidiary must have a parent — a second root is rejected
	// in the store (not merely by the schema trigger) per the p04.2 step.
	ErrSecondRoot = errors.New("store: a subsidiary must have a parent (second root not allowed)")
	// ErrParentMissing: the requested parent subsidiary does not exist.
	ErrParentMissing = errors.New("store: parent subsidiary not found")
	// ErrCycle: a move would make a subsidiary its own ancestor.
	ErrCycle = errors.New("store: move would create a cycle")
	// ErrRootImmovable: the root subsidiary cannot be given a parent.
	ErrRootImmovable = errors.New("store: the root subsidiary cannot be moved")
	// ErrHasActiveChildren: deactivation is blocked while active children remain.
	ErrHasActiveChildren = errors.New("store: subsidiary has active children")
)

// CreateSubsidiaryInput is the desired state of a NEW child subsidiary. ParentID
// is required (> 0): the single root exists from the seed, so every created
// subsidiary is a child (a second root is ErrSecondRoot).
type CreateSubsidiaryInput struct {
	ParentID     ids.SubsidiaryID
	Name         string
	BaseCurrency string
	SortOrder    int64
}

// UpdateSubsidiaryInput carries only the fields to change (nil = leave as-is), so
// a rename need not re-supply parent/currency. This desired-state-diff shape is
// the pattern later entity updates copy. A non-nil ParentID moves the subsidiary
// (validated against cycles and root-immovability).
type UpdateSubsidiaryInput struct {
	ParentID     *ids.SubsidiaryID
	Name         *string
	BaseCurrency *string
	SortOrder    *int64
}

// CreateSubsidiary creates a child subsidiary and returns its new id. It rejects
// a missing/invalid parent with a typed error (not leaning on the trigger) and
// versions the create under one change.
func (s *Store) CreateSubsidiary(ctx context.Context, in CreateSubsidiaryInput) (ids.SubsidiaryID, error) {
	if in.ParentID <= 0 {
		return 0, ErrSecondRoot
	}

	var newID ids.SubsidiaryID
	_, err := s.write(ctx, "subsidiary.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			// Validate the parent exists inside the tx (transaction-consistent).
			if _, err := q.GetSubsidiary(ctx, in.ParentID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrParentMissing
				}
				return fmt.Errorf("load parent %d: %w", in.ParentID, err)
			}

			id, err := q.InsertSubsidiary(ctx, sqlc.InsertSubsidiaryParams{
				ParentID:     sql.NullInt64{Int64: int64(in.ParentID), Valid: true},
				Name:         in.Name,
				BaseCurrency: in.BaseCurrency,
				Active:       1,
				SortOrder:    in.SortOrder,
			})
			if err != nil {
				return fmt.Errorf("insert subsidiary: %w", err)
			}
			newID = id

			return insertSubsidiaryVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create subsidiary: %w", err)
	}
	return newID, nil
}

// UpdateSubsidiary renames / moves / rebases a subsidiary. Base-currency change is
// allowed (it only affects report defaults, D18). A move (non-nil ParentID) is
// rejected if it would give the root a parent (ErrRootImmovable) or create a cycle
// (ErrCycle: the new parent must not be the subsidiary itself nor any descendant).
// The version append reflects the NEW values (it runs after the live update).
func (s *Store) UpdateSubsidiary(ctx context.Context, id ids.SubsidiaryID, in UpdateSubsidiaryInput) error {
	_, err := s.write(ctx, "subsidiary.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			cur, err := q.GetSubsidiary(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("subsidiary %d: %w", id, sql.ErrNoRows)
				}
				return fmt.Errorf("load subsidiary %d: %w", id, err)
			}

			// Read-modify-write: start from the current row, override provided
			// fields, write the full desired state (keeps snapshot-from-live trivial).
			next := cur
			if in.Name != nil {
				next.Name = *in.Name
			}
			if in.BaseCurrency != nil {
				next.BaseCurrency = *in.BaseCurrency
			}
			if in.SortOrder != nil {
				next.SortOrder = *in.SortOrder
			}
			if in.ParentID != nil {
				// The root (NULL parent) can never be given a parent.
				if !cur.ParentID.Valid {
					return ErrRootImmovable
				}
				// A move must not target the subsidiary itself nor any descendant.
				// Descendants includes self as its base case, so this one membership
				// test covers move-under-self and move-under-descendant alike.
				desc, err := q.Descendants(ctx, id)
				if err != nil {
					return fmt.Errorf("load descendants of %d: %w", id, err)
				}
				for _, dRow := range desc {
					if dRow.ID == *in.ParentID {
						return ErrCycle
					}
				}
				next.ParentID = sql.NullInt64{Int64: int64(*in.ParentID), Valid: true}
			}

			if err := q.UpdateSubsidiary(ctx, sqlc.UpdateSubsidiaryParams{
				ParentID:     next.ParentID,
				Name:         next.Name,
				BaseCurrency: next.BaseCurrency,
				Active:       next.Active,
				SortOrder:    next.SortOrder,
				ID:           id,
			}); err != nil {
				return fmt.Errorf("update subsidiary %d: %w", id, err)
			}

			return insertSubsidiaryVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update subsidiary %d: %w", id, err)
	}
	return nil
}

// DeactivateSubsidiary sets active=0. It is blocked while the subsidiary has any
// active children (ErrHasActiveChildren) — an inactive parent must not orphan
// active descendants. Deactivation is op='update', NOT 'delete': the entity still
// exists and keeps its history; delete-op is reserved for transaction soft-delete
// (p08).
func (s *Store) DeactivateSubsidiary(ctx context.Context, id ids.SubsidiaryID) error {
	_, err := s.write(ctx, "subsidiary.deactivate", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			cur, err := q.GetSubsidiary(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("subsidiary %d: %w", id, sql.ErrNoRows)
				}
				return fmt.Errorf("load subsidiary %d: %w", id, err)
			}

			n, err := q.CountActiveChildren(ctx, sql.NullInt64{Int64: int64(id), Valid: true})
			if err != nil {
				return fmt.Errorf("count active children of %d: %w", id, err)
			}
			if n > 0 {
				return ErrHasActiveChildren
			}

			if err := q.UpdateSubsidiary(ctx, sqlc.UpdateSubsidiaryParams{
				ParentID:     cur.ParentID,
				Name:         cur.Name,
				BaseCurrency: cur.BaseCurrency,
				Active:       0,
				SortOrder:    cur.SortOrder,
				ID:           id,
			}); err != nil {
				return fmt.Errorf("deactivate subsidiary %d: %w", id, err)
			}

			return insertSubsidiaryVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("deactivate subsidiary %d: %w", id, err)
	}
	return nil
}

// insertSubsidiaryVersion appends the snapshot-from-live version row. It hides the
// generated positional-param names (ID=change_id, Op, ID_2=entity_id — see the
// query comment) behind one call site so every entity op reads the same way. It
// MUST run after the live write so the snapshot captures the new values.
func insertSubsidiaryVersion(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, op string, entityID ids.SubsidiaryID) error {
	if err := q.InsertSubsidiaryVersion(ctx, sqlc.InsertSubsidiaryVersionParams{
		ID:   changeID,
		Op:   op,
		ID_2: entityID,
	}); err != nil {
		return fmt.Errorf("append subsidiary version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// GetSubsidiary returns the current live row for one subsidiary (read; sqlc).
func (s *Store) GetSubsidiary(ctx context.Context, id ids.SubsidiaryID) (sqlc.Subsidiary, error) {
	row, err := s.q.GetSubsidiary(ctx, id)
	if err != nil {
		return sqlc.Subsidiary{}, fmt.Errorf("store: get subsidiary %d: %w", id, err)
	}
	return row, nil
}

// SubTree returns all subsidiaries in depth-first (pre-order) traversal, children
// ordered by sort_order then id (read; recursive CTE via sqlc).
func (s *Store) SubTree(ctx context.Context) ([]sqlc.SubTreeRow, error) {
	rows, err := s.q.SubTree(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: subtree: %w", err)
	}
	return rows, nil
}

// RootSubsidiaryName returns the name of the root subsidiary (the single node
// whose parent_id is NULL) — the canonical organization display name (p30.14).
// The root is the consolidating entity (D18/p09.1) and ErrSecondRoot guarantees
// exactly one root exists, so SubTree's pre-order first row (its base case is
// `parent_id IS NULL`) is always the root; no extra query is needed. A missing
// root (an unmigrated/empty db) is an error rather than a silent empty name.
func (s *Store) RootSubsidiaryName(ctx context.Context) (string, error) {
	rows, err := s.q.SubTree(ctx)
	if err != nil {
		return "", fmt.Errorf("store: root subsidiary name: %w", err)
	}
	if len(rows) == 0 {
		return "", errors.New("store: no root subsidiary")
	}
	return rows[0].Name, nil
}

// Descendants returns a subsidiary plus its transitive closure (self included) —
// the primitive report scoping uses (D18). Read; recursive CTE via sqlc.
func (s *Store) Descendants(ctx context.Context, id ids.SubsidiaryID) ([]sqlc.DescendantsRow, error) {
	rows, err := s.q.Descendants(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: descendants of %d: %w", id, err)
	}
	return rows, nil
}

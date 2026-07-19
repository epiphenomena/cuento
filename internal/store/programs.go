package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/db/sqlc"
)

// Program operations (p07.1) -- programs are a dimension (D24): a single-root tree
// with a seeded root ("General", the unallocated default). These COPY the
// versioned-entity discipline the subsidiary ops (p04.2) established, MINUS
// base_currency: every public mutation runs through the write funnel as ONE
// change; inside fn the live write happens first, then the snapshot-from-live
// version append; validation lives inside fn so a rejected op rolls the change row
// back and leaves no audit trace.
//
// Error and method names are DELIBERATELY DISTINCT from the subsidiary ones
// (package store is shared): ErrProgramSecondRoot / ErrProgramRootImmovable /
// ErrProgramHasActiveChildren, ProgramTree / ProgramDescendants. ErrCycle is
// reused (same meaning: a move would make a node its own ancestor), as accounts.go
// already does.

// Typed sentinel errors handlers and tests branch on (AGENTS Style). Wrapped with
// %w at the call site so errors.Is sees them through the funnel.
var (
	// ErrProgramSecondRoot: a program must have a parent -- the single root exists
	// from the seed, so a second root is rejected in the store (not merely by the
	// schema trigger).
	ErrProgramSecondRoot = errors.New("store: a program must have a parent (second root not allowed)")
	// ErrProgramParentMissing: the requested parent program does not exist.
	ErrProgramParentMissing = errors.New("store: parent program not found")
	// ErrProgramRootImmovable: the root program cannot be given a parent.
	ErrProgramRootImmovable = errors.New("store: the root program cannot be moved")
	// ErrProgramHasActiveChildren: deactivation is blocked while active children
	// remain (an inactive parent must not orphan active descendants).
	ErrProgramHasActiveChildren = errors.New("store: program has active children")
	// ErrProgramNotFound: the requested program does not exist.
	ErrProgramNotFound = errors.New("store: program not found")

	// ErrDefaultProgramNotRE: a default_program_id is meaningful only on
	// revenue/expense accounts (D24). Rejected cleanly before the FK/trigger layer.
	ErrDefaultProgramNotRE = errors.New("store: default program allowed only on revenue/expense accounts")
	// ErrDefaultProgramMissing: the referenced default program does not exist.
	ErrDefaultProgramMissing = errors.New("store: default program not found")
	// ErrDefaultProgramInactive: the referenced default program is inactive; a
	// default must point at an active program (D24).
	ErrDefaultProgramInactive = errors.New("store: default program is inactive")
)

// CreateProgramInput is the desired state of a NEW child program. ParentID is
// required (> 0): the single root exists from the seed, so every created program
// is a child (a second root is ErrProgramSecondRoot).
type CreateProgramInput struct {
	ParentID  int64
	Name      string
	SortOrder int64
}

// UpdateProgramInput carries only the fields to change (nil = leave as-is). A
// non-nil ParentID moves the program (validated against cycles and
// root-immovability).
type UpdateProgramInput struct {
	ParentID  *int64
	Name      *string
	SortOrder *int64
}

// CreateProgram creates a child program and returns its new id. It rejects a
// missing/invalid parent with a typed error (not leaning on the trigger) and
// versions the create under one change.
func (s *Store) CreateProgram(ctx context.Context, in CreateProgramInput) (int64, error) {
	if in.ParentID <= 0 {
		return 0, ErrProgramSecondRoot
	}

	var newID int64
	_, err := s.write(ctx, "program.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			// Validate the parent exists inside the tx (transaction-consistent).
			if _, err := q.GetProgram(ctx, in.ParentID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrProgramParentMissing
				}
				return fmt.Errorf("load parent %d: %w", in.ParentID, err)
			}

			id, err := q.InsertProgram(ctx, sqlc.InsertProgramParams{
				ParentID:  sql.NullInt64{Int64: in.ParentID, Valid: true},
				Name:      in.Name,
				Active:    1,
				SortOrder: in.SortOrder,
			})
			if err != nil {
				return fmt.Errorf("insert program: %w", err)
			}
			newID = id

			return insertProgramVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create program: %w", err)
	}
	return newID, nil
}

// UpdateProgram renames / moves a program. A move (non-nil ParentID) is rejected
// if it would give the root a parent (ErrProgramRootImmovable) or create a cycle
// (ErrCycle: the new parent must not be the program itself nor any descendant).
// The version append reflects the NEW values (it runs after the live update).
func (s *Store) UpdateProgram(ctx context.Context, id int64, in UpdateProgramInput) error {
	_, err := s.write(ctx, "program.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetProgram(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrProgramNotFound
				}
				return fmt.Errorf("load program %d: %w", id, err)
			}

			// Read-modify-write: start from the current row, override provided
			// fields, write the full desired state (keeps snapshot-from-live trivial).
			next := cur
			if in.Name != nil {
				next.Name = *in.Name
			}
			if in.SortOrder != nil {
				next.SortOrder = *in.SortOrder
			}
			if in.ParentID != nil {
				// The root (NULL parent) can never be given a parent.
				if !cur.ParentID.Valid {
					return ErrProgramRootImmovable
				}
				// A move must not target the program itself nor any descendant.
				// Descendants includes self as its base case, so this one membership
				// test covers move-under-self and move-under-descendant alike.
				desc, err := q.ProgramDescendants(ctx, id)
				if err != nil {
					return fmt.Errorf("load descendants of %d: %w", id, err)
				}
				for _, dRow := range desc {
					if dRow.ID == *in.ParentID {
						return ErrCycle
					}
				}
				next.ParentID = sql.NullInt64{Int64: *in.ParentID, Valid: true}
			}

			if err := q.UpdateProgram(ctx, sqlc.UpdateProgramParams{
				ParentID:  next.ParentID,
				Name:      next.Name,
				Active:    next.Active,
				SortOrder: next.SortOrder,
				ID:        id,
			}); err != nil {
				return fmt.Errorf("update program %d: %w", id, err)
			}

			return insertProgramVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update program %d: %w", id, err)
	}
	return nil
}

// DeactivateProgram sets active=0. It is blocked while the program has any active
// children (ErrProgramHasActiveChildren). Deactivation is op='update', NOT
// 'delete': the entity still exists and keeps its history -- it only blocks NEW
// use (the full split-based blocks-new-use assertion lands in p08).
func (s *Store) DeactivateProgram(ctx context.Context, id int64) error {
	_, err := s.write(ctx, "program.deactivate", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetProgram(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrProgramNotFound
				}
				return fmt.Errorf("load program %d: %w", id, err)
			}

			n, err := q.CountActiveProgramChildren(ctx, sql.NullInt64{Int64: id, Valid: true})
			if err != nil {
				return fmt.Errorf("count active children of %d: %w", id, err)
			}
			if n > 0 {
				return ErrProgramHasActiveChildren
			}

			if err := q.UpdateProgram(ctx, sqlc.UpdateProgramParams{
				ParentID:  cur.ParentID,
				Name:      cur.Name,
				Active:    0,
				SortOrder: cur.SortOrder,
				ID:        id,
			}); err != nil {
				return fmt.Errorf("deactivate program %d: %w", id, err)
			}

			return insertProgramVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("deactivate program %d: %w", id, err)
	}
	return nil
}

// insertProgramVersion appends the snapshot-from-live version row. It hides the
// generated positional-param names (ID=change_id, Op, ID_2=entity_id) behind one
// call site. It MUST run after the live write so the snapshot captures the new
// values.
func insertProgramVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertProgramVersion(ctx, sqlc.InsertProgramVersionParams{
		ID:   changeID,
		Op:   op,
		ID_2: entityID,
	}); err != nil {
		return fmt.Errorf("append program version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// GetProgram returns the current live row for one program (read; sqlc).
func (s *Store) GetProgram(ctx context.Context, id int64) (sqlc.Program, error) {
	row, err := s.q.GetProgram(ctx, id)
	if err != nil {
		return sqlc.Program{}, fmt.Errorf("store: get program %d: %w", id, err)
	}
	return row, nil
}

// ProgramTree returns all programs in depth-first (pre-order) traversal, children
// ordered by sort_order then id (read; recursive CTE via sqlc).
func (s *Store) ProgramTree(ctx context.Context) ([]sqlc.ProgramTreeRow, error) {
	rows, err := s.q.ProgramTree(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: program tree: %w", err)
	}
	return rows, nil
}

// ProgramPaths returns id -> dotted-ancestor-path for every program in the tree
// (p29.13), mirroring AccountPaths (p28.2). The web layer stamps this path onto every
// program-select option's data-path so the shared fuzzy combobox (combofilter.js) shows
// and RANKS by the hierarchy -- a query like "gen.ed" lines up with "General.Education".
// A top-level program's path is just its name (the seeded root "General"). No lang
// param: program names are single stored proper nouns (no per-language variant), unlike
// account names. Read via ProgramTree (rule 2).
func (s *Store) ProgramPaths(ctx context.Context) (map[int64]string, error) {
	rows, err := s.ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	parentOf := make(map[int64]sql.NullInt64, len(rows))
	nameOf := make(map[int64]string, len(rows))
	for _, r := range rows {
		parentOf[r.ID] = r.ParentID
		nameOf[r.ID] = r.Name
	}
	pathOf := func(id int64) string {
		var seg []string
		for n, valid := id, true; valid; {
			seg = append(seg, nameOf[n])
			p := parentOf[n]
			if !p.Valid || p.Int64 == n {
				break
			}
			n = p.Int64
		}
		for i, j := 0, len(seg)-1; i < j; i, j = i+1, j-1 {
			seg[i], seg[j] = seg[j], seg[i]
		}
		return strings.Join(seg, ".")
	}
	out := make(map[int64]string, len(rows))
	for _, r := range rows {
		out[r.ID] = pathOf(r.ID)
	}
	return out, nil
}

// ProgramDescendants returns a program plus its transitive closure (self
// included) -- the primitive report scoping uses (D24). Read; recursive CTE.
func (s *Store) ProgramDescendants(ctx context.Context, id int64) ([]sqlc.ProgramDescendantsRow, error) {
	rows, err := s.q.ProgramDescendants(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: program descendants of %d: %w", id, err)
	}
	return rows, nil
}

// checkDefaultProgram validates a default_program_id for an account of accountType:
// it is allowed only on revenue/expense accounts (ErrDefaultProgramNotRE, D24), the
// program must exist (ErrDefaultProgramMissing) and be active (ErrDefaultProgramInactive).
// It runs inside fn on the tx-bound queries so a rejection rolls the change back.
func checkDefaultProgram(ctx context.Context, q *sqlc.Queries, programID int64, accountType string) error {
	if accountType != "revenue" && accountType != "expense" {
		return ErrDefaultProgramNotRE
	}
	prog, err := q.GetProgram(ctx, programID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDefaultProgramMissing
		}
		return fmt.Errorf("load default program %d: %w", programID, err)
	}
	if prog.Active == 0 {
		return ErrDefaultProgramInactive
	}
	return nil
}

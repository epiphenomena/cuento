package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
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

	// ErrProgramMergeSelf: src and dst are the same program (p11.5b).
	ErrProgramMergeSelf = errors.New("store: cannot merge a program into itself")
	// ErrProgramMergeRoot: the root program cannot be the merge SOURCE -- folding the
	// single root away would leave the program tree rootless (Z16). The account merge
	// analogue is that a root/placeholder cannot be a leaf source; here the root is
	// simply immovable (mirrors ErrProgramRootImmovable). dst may be any program.
	ErrProgramMergeRoot = errors.New("store: the root program cannot be merged away")
	// ErrProgramMergeIntoInactive: the DESTINATION program is inactive -- it cannot
	// take the moved references (a split/grant/child must point at an ACTIVE program,
	// mirroring the account merge's into-inactive guard).
	ErrProgramMergeIntoInactive = errors.New("store: merge destination program is inactive")
	// ErrProgramMergeFundScoped: at least one split on the SOURCE program is tagged a
	// FUND whose program scope would NOT contain the DESTINATION (p11.5b Z15b
	// block-guard, D20). Repointing that split's program to dst would leave it outside
	// its fund's program subtree -- a Z15b hole the trigger layer does not catch. So
	// MergeProgram REFUSES the merge (a clean typed rejection), mirroring the account
	// merge's reconciled-source block-guard. Full fund-scope repointing stays backlog.
	ErrProgramMergeFundScoped = errors.New("store: a source split's fund program scope does not cover the destination")
)

// CreateProgramInput is the desired state of a NEW child program. ParentID is
// required (> 0): the single root exists from the seed, so every created program
// is a child (a second root is ErrProgramSecondRoot).
type CreateProgramInput struct {
	ParentID  ids.ProgramID
	Name      string
	SortOrder int64
}

// UpdateProgramInput carries only the fields to change (nil = leave as-is). A
// non-nil ParentID moves the program (validated against cycles and
// root-immovability).
type UpdateProgramInput struct {
	ParentID  *ids.ProgramID
	Name      *string
	SortOrder *int64
}

// CreateProgram creates a child program and returns its new id. It rejects a
// missing/invalid parent with a typed error (not leaning on the trigger) and
// versions the create under one change.
func (s *Store) CreateProgram(ctx context.Context, in CreateProgramInput) (ids.ProgramID, error) {
	if in.ParentID <= 0 {
		return 0, ErrProgramSecondRoot
	}

	var newID ids.ProgramID
	_, err := s.write(ctx, "program.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			// Validate the parent exists inside the tx (transaction-consistent).
			if _, err := q.GetProgram(ctx, in.ParentID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrProgramParentMissing
				}
				return fmt.Errorf("load parent %d: %w", in.ParentID, err)
			}

			id, err := q.InsertProgram(ctx, sqlc.InsertProgramParams{
				ParentID:  sql.NullInt64{Int64: int64(in.ParentID), Valid: true},
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
func (s *Store) UpdateProgram(ctx context.Context, id ids.ProgramID, in UpdateProgramInput) error {
	_, err := s.write(ctx, "program.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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
				next.ParentID = sql.NullInt64{Int64: int64(*in.ParentID), Valid: true}
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
func (s *Store) DeactivateProgram(ctx context.Context, id ids.ProgramID) error {
	_, err := s.write(ctx, "program.deactivate", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			cur, err := q.GetProgram(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrProgramNotFound
				}
				return fmt.Errorf("load program %d: %w", id, err)
			}

			n, err := q.CountActiveProgramChildren(ctx, sql.NullInt64{Int64: int64(id), Valid: true})
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

// MergeProgram folds a SOURCE program into a DESTINATION program under ONE change
// (p11.5b), mirroring MergeAccount (p08.5). Every reference from src is repointed to
// dst -- each transaction split's program_id (versioned op='update', snapshot-from-
// live so the new snapshot carries program_id = dst), each program-subtree report
// grant scoped to src (re-scoped to dst; the scope is a mutable attribute so this is
// a delete+create version pair, no 'update' op, mirroring GrantReportGroup), and each
// DIRECT child program's parent_id (reparented under dst, versioned op='update') --
// then src is deactivated (active=0, op='update'). dst is NEVER written: it only
// receives the moved references, exactly as MergeAccount leaves dst untouched.
//
// AUDIT DISCIPLINE (rule 5/14): pre-merge snapshots that carry program_id/parent_id =
// src are left UNTOUCHED. Appending an op='update' snapshot per moved row keeps as-of
// history intact -- a pre-merge reconstruction still resolves src (kept, deactivated),
// after the merge it resolves dst.
//
// SAFETY (validated inside fn on the tx-bound q, TOCTOU discipline, before any write):
//   - src == dst is refused (ErrProgramMergeSelf).
//   - src == root is refused (ErrProgramMergeRoot): the single root is immovable and
//     folding it away would leave the tree rootless (Z16).
//   - dst must not be src NOR any descendant of src (ErrCycle): reparenting src's
//     children under dst would otherwise form a cycle. ProgramDescendants(src) includes
//     src, so the one membership test covers self and descendant alike.
//   - dst must be active (ErrProgramMergeIntoInactive): a split/grant/child must point
//     at an active program.
//
// FUND-SCOPE block-guard (Z15b, D20): a split tagged a FUND whose program scope R is
// set must carry a program inside R's subtree. Repointing such a split's program to dst
// would land it OUTSIDE that subtree when dst is not in R -- a Z15b hole the trigger
// layer does NOT catch. So MergeProgram REFUSES the merge (ErrProgramMergeFundScoped)
// when any src split's fund program scope would not cover dst, mirroring the account
// merge's reconciled-source block-guard. Full fund-scope repointing stays backlog.
func (s *Store) MergeProgram(ctx context.Context, src, dst ids.ProgramID) error {
	if src == dst {
		return ErrProgramMergeSelf
	}
	_, err := s.write(ctx, "program.merge", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			srcProg, err := q.GetProgram(ctx, src)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("load source %d: %w", src, ErrProgramNotFound)
				}
				return fmt.Errorf("load source %d: %w", src, err)
			}
			dstProg, err := q.GetProgram(ctx, dst)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("load destination %d: %w", dst, ErrProgramNotFound)
				}
				return fmt.Errorf("load destination %d: %w", dst, err)
			}

			// The root (NULL parent) can never be the source: folding it away leaves
			// the tree rootless (Z16). dst may be any program (incl. the root).
			if !srcProg.ParentID.Valid {
				return ErrProgramMergeRoot
			}
			// dst must be active -- it will receive the moved splits/grants/children.
			if dstProg.Active == 0 {
				return ErrProgramMergeIntoInactive
			}
			// dst must not be src nor any descendant of src, else reparenting src's
			// children under dst forms a cycle. ProgramDescendants includes self as its
			// base case, so this one membership test covers self + descendant alike.
			desc, err := q.ProgramDescendants(ctx, src)
			if err != nil {
				return fmt.Errorf("load descendants of %d: %w", src, err)
			}
			for _, dRow := range desc {
				if dRow.ID == dst {
					return ErrCycle
				}
			}

			// Fund-scope block-guard (Z15b, D20): refuse the merge when any split on src
			// is tagged a fund whose program scope would NOT cover dst. Repointing such a
			// split to dst would leave it outside its fund's program subtree -- a Z15b hole
			// the trigger layer does not catch. Checked BEFORE any write (TOCTOU).
			blocking, err := q.CountFundScopeBlockingSplits(ctx, sqlc.CountFundScopeBlockingSplitsParams{Src: src, Dst: dst})
			if err != nil {
				return fmt.Errorf("count fund-scope blocking splits (src %d, dst %d): %w", src, dst, err)
			}
			if blocking > 0 {
				return ErrProgramMergeFundScoped
			}

			// Repoint every split on src to dst, versioning each op='update'. Capture
			// the ids FIRST -- after the first repoint a WHERE program_id lookup would
			// confuse moved splits with dst's pre-existing ones (the MergeAccount rule).
			splitIDs, err := q.SplitIdsByProgram(ctx, src)
			if err != nil {
				return fmt.Errorf("list source splits %d: %w", src, err)
			}
			for _, sid := range splitIDs {
				if err := q.RepointSplitProgram(ctx, sqlc.RepointSplitProgramParams{ProgramID: dst, ID: sid}); err != nil {
					return fmt.Errorf("repoint split %d -> program %d: %w", sid, dst, err)
				}
				// Snapshot-from-live AFTER the live update so the op='update' snapshot
				// records program_id = dst.
				if err := insertSplitVersion(ctx, q, changeID, "update", sid); err != nil {
					return err
				}
			}

			// Re-scope every report grant scoped to src onto dst. The program scope is a
			// mutable ATTRIBUTE (not part of the (user, group) key), so a re-scope is a
			// delete+create version pair (no 'update' op, mirroring GrantReportGroup):
			// snapshot op='delete' (old scope) BEFORE the live update, then op='create'
			// (new scope) AFTER. Capture the affected keys FIRST (same reason as splits).
			grants, err := q.ReportGrantsByProgram(ctx, src)
			if err != nil {
				return fmt.Errorf("list source grants %d: %w", src, err)
			}
			for _, g := range grants {
				if err := q.InsertReportGrantVersion(ctx, sqlc.InsertReportGrantVersionParams{
					Op: "delete", ID: changeID, UserID: g.UserID, GroupName: g.GroupName,
				}); err != nil {
					return fmt.Errorf("version grant rescope-remove (%d,%s): %w", g.UserID, g.GroupName, err)
				}
				if err := q.RepointReportGrantProgram(ctx, sqlc.RepointReportGrantProgramParams{
					ProgramID: dst, UserID: g.UserID, GroupName: g.GroupName,
				}); err != nil {
					return fmt.Errorf("repoint grant (%d,%s) -> program %d: %w", g.UserID, g.GroupName, dst, err)
				}
				if err := q.InsertReportGrantVersion(ctx, sqlc.InsertReportGrantVersionParams{
					Op: "create", ID: changeID, UserID: g.UserID, GroupName: g.GroupName,
				}); err != nil {
					return fmt.Errorf("version grant rescope-add (%d,%s): %w", g.UserID, g.GroupName, err)
				}
			}

			// Reparent every DIRECT child of src under dst, versioning each op='update'.
			// The cycle guard above guarantees dst is neither src nor a descendant, so no
			// child move can form a cycle. Capture the ids FIRST.
			childIDs, err := q.ProgramChildIDs(ctx, src)
			if err != nil {
				return fmt.Errorf("list source children %d: %w", src, err)
			}
			for _, cid := range childIDs {
				if err := q.RepointProgramParent(ctx, sqlc.RepointProgramParentParams{ParentID: dst, ID: cid}); err != nil {
					return fmt.Errorf("reparent child %d -> program %d: %w", cid, dst, err)
				}
				if err := insertProgramVersion(ctx, q, changeID, "update", cid); err != nil {
					return err
				}
			}

			// Deactivate src (active=0, op='update'). Inlined rather than calling the
			// public DeactivateProgram so it rides the SAME change as the repoints (and
			// bypasses the active-children guard -- src's children have already moved).
			// Every other column is carried through from srcProg.
			if err := q.UpdateProgram(ctx, sqlc.UpdateProgramParams{
				ParentID:  srcProg.ParentID,
				Name:      srcProg.Name,
				Active:    0,
				SortOrder: srcProg.SortOrder,
				ID:        src,
			}); err != nil {
				return fmt.Errorf("deactivate source %d: %w", src, err)
			}
			return insertProgramVersion(ctx, q, changeID, "update", src)
		})
	if err != nil {
		return fmt.Errorf("merge program %d into %d: %w", src, dst, err)
	}
	return nil
}

// SplitCountForProgram returns how many splits currently carry a program_id (p11.5b).
// The merge preview reports it as the number of transaction lines that will move; it
// reuses the SAME predicate MergeProgram repoints from so the preview never lies. Read.
func (s *Store) SplitCountForProgram(ctx context.Context, id ids.ProgramID) (int, error) {
	n, err := s.q.CountSplitsForProgram(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("store: count splits for program %d: %w", id, err)
	}
	return int(n), nil
}

// insertProgramVersion appends the snapshot-from-live version row. It hides the
// generated positional-param names (ID=change_id, Op, ID_2=entity_id) behind one
// call site. It MUST run after the live write so the snapshot captures the new
// values.
func insertProgramVersion(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, op string, entityID ids.ProgramID) error {
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
func (s *Store) GetProgram(ctx context.Context, id ids.ProgramID) (sqlc.Program, error) {
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
func (s *Store) ProgramPaths(ctx context.Context) (map[ids.ProgramID]string, error) {
	rows, err := s.ProgramTree(ctx)
	if err != nil {
		return nil, err
	}
	parentOf := make(map[ids.ProgramID]sql.NullInt64, len(rows))
	nameOf := make(map[ids.ProgramID]string, len(rows))
	for _, r := range rows {
		parentOf[r.ID] = r.ParentID
		nameOf[r.ID] = r.Name
	}
	pathOf := func(id ids.ProgramID) string {
		var seg []string
		for n, valid := id, true; valid; {
			seg = append(seg, nameOf[n])
			p := parentOf[n]
			if !p.Valid || p.Int64 == int64(n) {
				break
			}
			n = ids.ProgramID(p.Int64)
		}
		for i, j := 0, len(seg)-1; i < j; i, j = i+1, j-1 {
			seg[i], seg[j] = seg[j], seg[i]
		}
		return strings.Join(seg, ".")
	}
	out := make(map[ids.ProgramID]string, len(rows))
	for _, r := range rows {
		out[r.ID] = pathOf(r.ID)
	}
	return out, nil
}

// ProgramDescendants returns a program plus its transitive closure (self
// included) -- the primitive report scoping uses (D24). Read; recursive CTE.
func (s *Store) ProgramDescendants(ctx context.Context, id ids.ProgramID) ([]sqlc.ProgramDescendantsRow, error) {
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
func checkDefaultProgram(ctx context.Context, q *sqlc.Queries, programID ids.ProgramID, accountType string) error {
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

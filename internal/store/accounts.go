package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cuento/internal/db/sqlc"
)

// Account operations (p05.2). These COPY the versioned-entity discipline the
// subsidiary ops (p04.2) established: every public mutation runs through the write
// funnel as ONE change; inside fn the live write happens first, then the
// snapshot-from-live version append; validation lives inside fn so a rejected op
// rolls the change row back and leaves no audit trace.
//
// Two things depart from the subsidiary pattern and are called out at their call
// sites:
//
//  1. Composite keys. account_names and account_subsidiaries version rows key on
//     (account_id, lang) and (account_id, subsidiary_id); their InsertXVersion
//     queries copy InsertSubsidiaryVersion's shape verbatim, changing only the
//     entity_id/WHERE (per 00005). AssertVersionedName/AssertVersionedSub filter
//     on the composite key.
//
//  2. Subsidiary REMOVAL inverts the live-write-first rule. The version append is
//     snapshot-FROM-LIVE, so for op='delete' the version row MUST be captured
//     BEFORE the live membership is deleted (the row must still exist to snapshot
//     the last-known membership). Additions keep the normal live-first order.
//
// Subsidiary set semantics (D18) are deliberately asymmetric and are NOT unified:
//   - assign/add (CreateAccount, SetAccountSubsidiaries additions): auto-propagate
//     UP the ancestor chain (each ancestor missing the sub gains it), preserving
//     parent-set superset-of union-of-children; an ancestor already holding the
//     sub is a no-op with no version row;
//   - remove (SetAccountSubsidiaries removals): LOCAL, never cascades up; blocked
//     by ErrSubInUseByChild while a direct child still maps the sub (the split-
//     usage half of that guard is p08 -- see the TODO below);
//   - move (UpdateAccount parent change): REJECTS with ErrSubMismatch when the new
//     parent's set does not cover the mover's -- it does not propagate.

// Typed sentinel errors handlers and tests branch on. ErrCycle is shared with the
// subsidiary ops (same meaning: a move would make a node its own ancestor).
var (
	// ErrNoSubsidiary: CreateAccount requires at least one subsidiary (D18).
	ErrNoSubsidiary = errors.New("store: account needs at least one subsidiary")
	// ErrNameRequired: at least one name, and en is required.
	ErrNameRequired = errors.New("store: account needs an English (en) name")
	// ErrCrossTypeClass: a move violates the tree type rules (D11).
	ErrCrossTypeClass = errors.New("store: parent type incompatible with account type")
	// ErrSubMismatch: the new parent's subsidiary set must cover the moving
	// account's set (D18 superset invariant).
	ErrSubMismatch = errors.New("store: new parent's subsidiary set does not cover the account's")
	// ErrSubInUseByChild: a subsidiary cannot be removed while a child account
	// still maps it (superset invariant).
	ErrSubInUseByChild = errors.New("store: subsidiary still mapped by a child account")
	// Err990TypeMismatch: a form990_code whose allowed account_types (CSV) does
	// not include the account's type (D25).
	Err990TypeMismatch = errors.New("store: 990 line not valid for this account type")
	// ErrFunctionalClassNotExpense: a default functional_class is allowed only on
	// expense accounts (D21). Validated cleanly here; the trigger is the backstop.
	ErrFunctionalClassNotExpense = errors.New("store: functional_class allowed only on expense accounts")
	// ErrAccountNotFound: the requested account does not exist.
	ErrAccountNotFound = errors.New("store: account not found")
)

// CreateAccountInput is the desired state of a NEW account. ParentID is a pointer:
// nil (or its zero) = top-level (accounts have NO single-root constraint, D11).
// Names maps lang->name (en required, >=1). Subsidiaries is the set to map
// (>=1); memberships propagate up the ancestor chain. FunctionalClass and
// Form990Code are optional (nil = none).
type CreateAccountInput struct {
	ParentID        *int64
	Type            string
	DefaultCurrency string
	Names           map[string]string
	Subsidiaries    []int64
	FunctionalClass *string
	Form990Code     *string
	// DefaultProgramID is optional (nil = none). It is meaningful ONLY on
	// revenue/expense accounts (D24, ErrDefaultProgramNotRE); it must reference an
	// existing, active program. It prefills a split's required program_id (p08).
	DefaultProgramID *int64
	Intercompany     bool
	Reconcilable     bool
	SortOrder        int64
}

// UpdateAccountInput carries only fields to change (nil = leave as-is). A non-nil
// ParentID moves the account (validated against cycle / cross-type-class /
// sub-mismatch). A non-nil Form990Code is validated against the account's type.
type UpdateAccountInput struct {
	ParentID        *int64
	DefaultCurrency *string
	FunctionalClass *string
	Form990Code     *string
	// DefaultProgramID: a non-nil, positive value sets the default program (R/E
	// only, active; D24). A non-nil zero (0) clears it. nil leaves it unchanged.
	DefaultProgramID *int64
	Intercompany     *bool
	Reconcilable     *bool
	SortOrder        *int64
}

// CreateAccount creates an account (+ its names + its subsidiary memberships,
// propagated up the ancestor chain) under ONE change, and returns the new id.
// All the version rows (account, each name, each membership incl. propagated
// ancestors) share that change.
func (s *Store) CreateAccount(ctx context.Context, in CreateAccountInput) (int64, error) {
	if len(in.Subsidiaries) == 0 {
		return 0, ErrNoSubsidiary
	}
	if _, ok := in.Names["en"]; !ok {
		return 0, ErrNameRequired
	}
	if in.FunctionalClass != nil && in.Type != "expense" {
		return 0, ErrFunctionalClassNotExpense
	}
	// A default program is R/E-only (D24). Reject early on a non-R/E account
	// before opening the tx (its existence/active check runs inside fn).
	if in.DefaultProgramID != nil && in.Type != "revenue" && in.Type != "expense" {
		return 0, ErrDefaultProgramNotRE
	}

	var newID int64
	_, err := s.write(ctx, "account.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			// Validate the parent (if any): it must exist and be type-compatible
			// (D11). No sub-mismatch check here: create AUTO-PROPAGATES every
			// assigned subsidiary up the ancestor chain (each ancestor gains it if
			// missing), so a parent need not already hold the sub. ErrSubMismatch
			// is a MOVE-only guard (validateMove); the create/move asymmetry
			// (create grows ancestors, move rejects) is intentional -- see the
			// DECISIONS p05.2 note.
			if in.ParentID != nil {
				parent, err := q.GetAccount(ctx, *in.ParentID)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return ErrAccountNotFound
					}
					return fmt.Errorf("load parent %d: %w", *in.ParentID, err)
				}
				if !typeCompatible(parent.Type, in.Type) {
					return ErrCrossTypeClass
				}
			}

			// Validate a 990 code against the account's type (D25).
			if in.Form990Code != nil {
				if err := check990Type(ctx, q, *in.Form990Code, in.Type); err != nil {
					return err
				}
			}

			// Validate a default program: R/E-only (already checked above),
			// existing and active (D24). Runs inside fn so a rejection rolls back.
			if in.DefaultProgramID != nil {
				if err := checkDefaultProgram(ctx, q, *in.DefaultProgramID, in.Type); err != nil {
					return err
				}
			}

			id, err := q.InsertAccount(ctx, sqlc.InsertAccountParams{
				ParentID:         nullInt64Ptr(in.ParentID),
				Type:             in.Type,
				DefaultCurrency:  in.DefaultCurrency,
				FunctionalClass:  nullStringPtr(in.FunctionalClass),
				Form990Code:      nullStringPtr(in.Form990Code),
				DefaultProgramID: nullInt64Ptr(in.DefaultProgramID),
				Intercompany:     boolToInt(in.Intercompany),
				Reconcilable:     boolToInt(in.Reconcilable),
				Active:           1,
				SortOrder:        in.SortOrder,
				CreatedAt:        s.now().Format(time.RFC3339Nano),
			})
			if err != nil {
				return fmt.Errorf("insert account: %w", err)
			}
			newID = id

			if err := insertAccountVersion(ctx, q, changeID, "create", id); err != nil {
				return err
			}

			// Names (en guaranteed present). Deterministic order not required for
			// correctness; the map iteration is fine (each is its own version row).
			for lang, name := range in.Names {
				if err := upsertAccountName(ctx, q, changeID, id, lang, name, "create"); err != nil {
					return err
				}
			}

			// Subsidiary memberships + ancestor propagation. Each assigned sub is
			// added to this account and to every strict ancestor missing it.
			for _, sid := range in.Subsidiaries {
				if err := addSubWithPropagation(ctx, q, changeID, id, sid); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("create account: %w", err)
	}
	return newID, nil
}

// UpdateAccount moves the account and/or changes its flags/currency/functional
// default/990 code/sort under one change. A move is rejected on cycle (ErrCycle),
// cross-type-class (ErrCrossTypeClass), or sub-mismatch (ErrSubMismatch); a 990
// code is rejected on type mismatch (Err990TypeMismatch). The version append
// reflects the NEW values (it runs after the live update).
func (s *Store) UpdateAccount(ctx context.Context, id int64, in UpdateAccountInput) error {
	_, err := s.write(ctx, "account.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetAccount(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAccountNotFound
				}
				return fmt.Errorf("load account %d: %w", id, err)
			}

			next := cur
			if in.DefaultCurrency != nil {
				next.DefaultCurrency = *in.DefaultCurrency
			}
			if in.FunctionalClass != nil {
				// A default functional class is expense-only (D21). *in may be an
				// empty string to CLEAR it; treat "" as NULL.
				if *in.FunctionalClass != "" && next.Type != "expense" {
					return ErrFunctionalClassNotExpense
				}
				next.FunctionalClass = nullString(*in.FunctionalClass)
			}
			if in.Form990Code != nil {
				if *in.Form990Code != "" {
					if err := check990Type(ctx, q, *in.Form990Code, next.Type); err != nil {
						return err
					}
				}
				next.Form990Code = nullString(*in.Form990Code)
			}
			if in.DefaultProgramID != nil {
				// A non-nil zero clears it; a positive value sets it (R/E-only,
				// active; D24). Validated against next.Type so a same-call type
				// change is honored (types don't change here, but be explicit).
				if *in.DefaultProgramID == 0 {
					next.DefaultProgramID = sql.NullInt64{}
				} else {
					if err := checkDefaultProgram(ctx, q, *in.DefaultProgramID, next.Type); err != nil {
						return err
					}
					next.DefaultProgramID = sql.NullInt64{Int64: *in.DefaultProgramID, Valid: true}
				}
			}
			if in.Intercompany != nil {
				next.Intercompany = boolToInt(*in.Intercompany)
			}
			if in.Reconcilable != nil {
				next.Reconcilable = boolToInt(*in.Reconcilable)
			}
			if in.SortOrder != nil {
				next.SortOrder = *in.SortOrder
			}
			if in.ParentID != nil {
				if err := s.validateMove(ctx, q, id, cur, *in.ParentID); err != nil {
					return err
				}
				next.ParentID = sql.NullInt64{Int64: *in.ParentID, Valid: true}
			}

			// next := cur copied DefaultProgramID, so an unrelated update carries
			// it through unchanged -- it is never silently NULLed (the ripple).
			if err := q.UpdateAccount(ctx, sqlc.UpdateAccountParams{
				ParentID:         next.ParentID,
				Type:             next.Type,
				DefaultCurrency:  next.DefaultCurrency,
				FunctionalClass:  next.FunctionalClass,
				Form990Code:      next.Form990Code,
				DefaultProgramID: next.DefaultProgramID,
				Intercompany:     next.Intercompany,
				Reconcilable:     next.Reconcilable,
				Active:           next.Active,
				SortOrder:        next.SortOrder,
				CreatedAt:        next.CreatedAt,
				ID:               id,
			}); err != nil {
				return fmt.Errorf("update account %d: %w", id, err)
			}
			return insertAccountVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update account %d: %w", id, err)
	}
	return nil
}

// validateMove checks a reparent: not root-self/descendant (ErrCycle), parent
// type-compatible (ErrCrossTypeClass), and the new parent's subsidiary set covers
// the mover's (ErrSubMismatch). A move never propagates subs -- it only rejects.
func (s *Store) validateMove(ctx context.Context, q *sqlc.Queries, id int64, cur sqlc.Account, newParent int64) error {
	// Descendants includes self as its base case, so this one membership test
	// covers move-under-self and move-under-descendant alike.
	desc, err := q.AccountDescendants(ctx, id)
	if err != nil {
		return fmt.Errorf("load descendants of %d: %w", id, err)
	}
	for _, dID := range desc {
		if dID == newParent {
			return ErrCycle
		}
	}

	parent, err := q.GetAccount(ctx, newParent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("load new parent %d: %w", newParent, err)
	}
	if !typeCompatible(parent.Type, cur.Type) {
		return ErrCrossTypeClass
	}

	// New parent's set must cover the mover's set (D18 superset).
	moverSubs, err := subSet(ctx, q, id)
	if err != nil {
		return err
	}
	parentSubs, err := subSet(ctx, q, newParent)
	if err != nil {
		return err
	}
	for sid := range moverSubs {
		if !parentSubs[sid] {
			return ErrSubMismatch
		}
	}
	return nil
}

// SetAccountName upserts one (account_id, lang) name under one change. The op is
// create the first time that (account, lang) is written, update thereafter.
func (s *Store) SetAccountName(ctx context.Context, id int64, lang, name string) error {
	_, err := s.write(ctx, "account.name", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetAccount(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAccountNotFound
				}
				return fmt.Errorf("load account %d: %w", id, err)
			}
			// Detect create vs update from whether the name already exists.
			op := "create"
			if _, err := q.GetAccountName(ctx, sqlc.GetAccountNameParams{AccountID: id, Lang: lang}); err == nil {
				op = "update"
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("load name (%d,%s): %w", id, lang, err)
			}
			return upsertAccountName(ctx, q, changeID, id, lang, name, op)
		})
	if err != nil {
		return fmt.Errorf("set account name (%d,%s): %w", id, lang, err)
	}
	return nil
}

// SetAccountSubsidiaries sets the account's subsidiary set to exactly `subs`
// under one change, honoring the superset invariant + ancestor auto-propagation
// (D18). Additions cascade UP (each ancestor missing the sub gains it); removals
// are local and blocked while a child still maps the sub (ErrSubInUseByChild).
func (s *Store) SetAccountSubsidiaries(ctx context.Context, id int64, subs []int64) error {
	_, err := s.write(ctx, "account.subsidiaries", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetAccount(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAccountNotFound
				}
				return fmt.Errorf("load account %d: %w", id, err)
			}

			want := make(map[int64]bool, len(subs))
			for _, sid := range subs {
				want[sid] = true
			}
			have, err := subSet(ctx, q, id)
			if err != nil {
				return err
			}

			// Removals first: block if a child still maps the sub, OR (p08) if a
			// split on this account belongs to a non-deleted txn in that sub.
			for sid := range have {
				if want[sid] {
					continue
				}
				n, err := q.CountChildAccountsWithSub(ctx, sqlc.CountChildAccountsWithSubParams{
					ParentID:     sql.NullInt64{Int64: id, Valid: true},
					SubsidiaryID: sid,
				})
				if err != nil {
					return fmt.Errorf("count child use of sub %d: %w", sid, err)
				}
				if n > 0 {
					return ErrSubInUseByChild
				}
				// p08 (completed): a split on this account in a non-deleted txn of
				// subsidiary sid also blocks removal (ErrSubInUseByChild extended).
				used, err := accountSplitInSubsidiary(ctx, q, id, sid)
				if err != nil {
					return err
				}
				if used {
					return ErrSubInUseByChild
				}
				if err := removeSub(ctx, q, changeID, id, sid); err != nil {
					return err
				}
			}

			// Additions: add to this account + propagate up the ancestor chain.
			for sid := range want {
				if have[sid] {
					continue
				}
				if err := addSubWithPropagation(ctx, q, changeID, id, sid); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("set account subsidiaries (%d): %w", id, err)
	}
	return nil
}

// DeactivateAccount sets active=0, op='update' (NOT 'delete' -- the entity
// persists; delete-op is reserved for transaction soft-delete, rule 14).
func (s *Store) DeactivateAccount(ctx context.Context, id int64) error {
	_, err := s.write(ctx, "account.deactivate", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetAccount(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAccountNotFound
				}
				return fmt.Errorf("load account %d: %w", id, err)
			}
			if err := q.UpdateAccount(ctx, sqlc.UpdateAccountParams{
				ParentID:         cur.ParentID,
				Type:             cur.Type,
				DefaultCurrency:  cur.DefaultCurrency,
				FunctionalClass:  cur.FunctionalClass,
				Form990Code:      cur.Form990Code,
				DefaultProgramID: cur.DefaultProgramID,
				Intercompany:     cur.Intercompany,
				Reconcilable:     cur.Reconcilable,
				Active:           0,
				SortOrder:        cur.SortOrder,
				CreatedAt:        cur.CreatedAt,
				ID:               id,
			}); err != nil {
				return fmt.Errorf("deactivate account %d: %w", id, err)
			}
			return insertAccountVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("deactivate account %d: %w", id, err)
	}
	return nil
}

// AccountName returns an account's name in EXACTLY the given language, or "" when
// that (account, lang) name is unset. Unlike Tree (which applies the p05.3 fallback
// en -> any), this reports the raw per-language row, so the account form's edit
// prefill shows an empty box for a language that has no name yet rather than
// echoing the en name into a foreign-language input (p11.4). Read; sqlc.
func (s *Store) AccountName(ctx context.Context, id int64, lang string) (string, error) {
	row, err := s.q.GetAccountName(ctx, sqlc.GetAccountNameParams{AccountID: id, Lang: lang})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: account name (%d,%s): %w", id, lang, err)
	}
	return row.Name, nil
}

// GetAccount returns the current live row for one account (read; sqlc).
func (s *Store) GetAccount(ctx context.Context, id int64) (sqlc.Account, error) {
	row, err := s.q.GetAccount(ctx, id)
	if err != nil {
		return sqlc.Account{}, fmt.Errorf("store: get account %d: %w", id, err)
	}
	return row, nil
}

// SplitIDsForAccount returns the ids of every split currently on an account, in
// id order. Read; sqlc. It reuses the SAME SplitIdsByAccount query MergeAccount
// repoints from, so the merge-UI consequences preview counts EXACTLY the splits
// the merge will move (including soft-deleted-txn splits -- see the query
// comment); a count is len() of this, guaranteeing preview == effect by
// construction rather than a second COUNT query that could drift (p11.2).
func (s *Store) SplitIDsForAccount(ctx context.Context, accountID int64) ([]int64, error) {
	ids, err := s.q.SplitIdsByAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: split ids for account %d: %w", accountID, err)
	}
	return ids, nil
}

// ReconciledSplitCount returns how many splits on an account are cleared against a
// reconciliation (non-NULL reconciliation_id). The merge UI uses it to show, in the
// consequences preview, how many reconciled splits would BLOCK the merge (the p22.5
// block-guard rejects the merge when this is > 0); the store enforces the same guard
// on write (ErrMergeSourceReconciled). Read; sqlc.
func (s *Store) ReconciledSplitCount(ctx context.Context, accountID int64) (int, error) {
	n, err := s.q.CountReconciledSplitsForAccount(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("store: reconciled split count for account %d: %w", accountID, err)
	}
	return int(n), nil
}

// TreeRow is one account in tree order with its name resolved for the requested
// lang (empty when that lang has no name -- the en->any fallback is p05.3).
type TreeRow struct {
	ID        int64
	ParentID  sql.NullInt64
	Type      string
	Active    int64
	SortOrder int64
	Name      string
}

// Tree returns accounts in pre-order (recursive CTE), names resolved for `lang`
// via a plain LEFT JOIN (empty when absent). When subFilter is non-nil, only
// accounts mapped to that subsidiary are returned. Read; sqlc.
func (s *Store) Tree(ctx context.Context, lang string, subFilter *int64) ([]TreeRow, error) {
	if subFilter != nil {
		rows, err := s.q.AccountTreeBySubsidiary(ctx, sqlc.AccountTreeBySubsidiaryParams{
			SubsidiaryID: *subFilter,
			Lang:         lang,
		})
		if err != nil {
			return nil, fmt.Errorf("store: account tree (sub %d): %w", *subFilter, err)
		}
		out := make([]TreeRow, len(rows))
		for i, r := range rows {
			out[i] = TreeRow{ID: r.ID, ParentID: r.ParentID, Type: r.Type, Active: r.Active, SortOrder: r.SortOrder, Name: r.Name}
		}
		return out, nil
	}
	rows, err := s.q.AccountTree(ctx, lang)
	if err != nil {
		return nil, fmt.Errorf("store: account tree: %w", err)
	}
	out := make([]TreeRow, len(rows))
	for i, r := range rows {
		out[i] = TreeRow{ID: r.ID, ParentID: r.ParentID, Type: r.Type, Active: r.Active, SortOrder: r.SortOrder, Name: r.Name}
	}
	return out, nil
}

// Effective990Codes maps accountID -> effective 990 code = own code, else the
// nearest ancestor's (D25). It resolves the nearest-ancestor code IN GO from a
// flat (id, parent_id, own-code) fetch: sqlc's sqlite analyzer cannot resolve a
// carried recursive-CTE column in the output projection (it reports the carried
// column "does not exist"), so the walk lives here. The chart is small; the walk
// is O(n * depth). Accounts with no code anywhere on the chain are absent from
// the map (unmapped, D25).
func (s *Store) Effective990Codes(ctx context.Context) (map[int64]string, error) {
	rows, err := s.q.AllAccountCodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: effective 990 codes: %w", err)
	}
	own := make(map[int64]sql.NullString, len(rows))
	parent := make(map[int64]sql.NullInt64, len(rows))
	for _, r := range rows {
		own[r.ID] = r.Form990Code
		parent[r.ID] = r.ParentID
	}
	eff := make(map[int64]string, len(rows))
	for id := range own {
		// Walk id -> parent -> ... until a node has an own code (nearest wins).
		for n, valid := id, true; valid; {
			if c := own[n]; c.Valid {
				eff[id] = c.String
				break
			}
			p := parent[n]
			if !p.Valid {
				break // reached a root with no code on the chain
			}
			n = p.Int64
			// Guard against a malformed cycle (should never happen; moves reject
			// cycles): stop if we somehow revisit the origin.
			if n == id {
				break
			}
		}
	}
	return eff, nil
}

// --- helpers (unexported) -------------------------------------------------

// typeCompatible reports whether a child of type childType may sit under a parent
// of type parentType (D11): A/L/E children must match the parent's type exactly;
// under an R/E parent, revenue and expense interleave freely.
func typeCompatible(parentType, childType string) bool {
	switch parentType {
	case "asset", "liability", "equity":
		return childType == parentType
	case "revenue", "expense":
		return childType == "revenue" || childType == "expense"
	default:
		return false
	}
}

// subSet returns an account's current subsidiary id set.
func subSet(ctx context.Context, q *sqlc.Queries, accountID int64) (map[int64]bool, error) {
	ids, err := q.AccountSubsidiaries(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("load subsidiaries of %d: %w", accountID, err)
	}
	set := make(map[int64]bool, len(ids))
	for _, sid := range ids {
		set[sid] = true
	}
	return set, nil
}

// check990Type rejects a form990_code whose allowed account_types CSV does not
// include accountType (D25, Err990TypeMismatch). Membership, not equality: some
// lines legitimately allow more than one type.
func check990Type(ctx context.Context, q *sqlc.Queries, code, accountType string) error {
	line, err := q.GetForm990Line(ctx, code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Err990TypeMismatch
		}
		return fmt.Errorf("load 990 line %s: %w", code, err)
	}
	if csvContains(line.AccountTypes, accountType) {
		return nil
	}
	return Err990TypeMismatch
}

// addSubWithPropagation adds subsidiary sid to accountID and to every strict
// ancestor missing it (D18 auto-propagation). Each newly-added membership (self
// or ancestor) gets its own op='create' version row; an account already holding
// the sub is a no-op with no version row (the PK forbids a duplicate).
func addSubWithPropagation(ctx context.Context, q *sqlc.Queries, changeID, accountID, sid int64) error {
	anc, err := q.AccountAncestors(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load ancestors of %d: %w", accountID, err)
	}
	// AccountAncestors includes self; adding to self + each ancestor is exactly
	// the superset-preserving propagation.
	for _, aID := range anc {
		has, err := q.HasAccountSubsidiary(ctx, sqlc.HasAccountSubsidiaryParams{AccountID: aID, SubsidiaryID: sid})
		if err != nil {
			return fmt.Errorf("check membership (%d,%d): %w", aID, sid, err)
		}
		if has > 0 {
			continue // already mapped -- no-op, no version row
		}
		if err := q.InsertAccountSubsidiary(ctx, sqlc.InsertAccountSubsidiaryParams{AccountID: aID, SubsidiaryID: sid}); err != nil {
			return fmt.Errorf("add membership (%d,%d): %w", aID, sid, err)
		}
		// Live-write-FIRST, then snapshot-from-live: normal order for an add.
		if err := q.InsertAccountSubsidiaryVersion(ctx, sqlc.InsertAccountSubsidiaryVersionParams{
			Op: "create", ID: changeID, AccountID: aID, SubsidiaryID: sid,
		}); err != nil {
			return fmt.Errorf("version membership add (%d,%d): %w", aID, sid, err)
		}
	}
	return nil
}

// removeSub deletes membership (accountID, sid) and versions it op='delete'.
//
// REMOVAL INVERTS the live-write-first convention: the version append is
// snapshot-FROM-LIVE, so the version row (the last-known membership) MUST be
// captured BEFORE the live row is deleted, or there is nothing left to snapshot.
// This is the one place the account ops depart from subsidiaries.go's order; the
// comment is deliberate.
func removeSub(ctx context.Context, q *sqlc.Queries, changeID, accountID, sid int64) error {
	if err := q.InsertAccountSubsidiaryVersion(ctx, sqlc.InsertAccountSubsidiaryVersionParams{
		Op: "delete", ID: changeID, AccountID: accountID, SubsidiaryID: sid,
	}); err != nil {
		return fmt.Errorf("version membership remove (%d,%d): %w", accountID, sid, err)
	}
	if err := q.DeleteAccountSubsidiary(ctx, sqlc.DeleteAccountSubsidiaryParams{AccountID: accountID, SubsidiaryID: sid}); err != nil {
		return fmt.Errorf("remove membership (%d,%d): %w", accountID, sid, err)
	}
	return nil
}

// upsertAccountName writes one (account_id, lang) name live then versions it.
func upsertAccountName(ctx context.Context, q *sqlc.Queries, changeID, accountID int64, lang, name, op string) error {
	if err := q.UpsertAccountName(ctx, sqlc.UpsertAccountNameParams{AccountID: accountID, Lang: lang, Name: name}); err != nil {
		return fmt.Errorf("upsert name (%d,%s): %w", accountID, lang, err)
	}
	if err := q.InsertAccountNameVersion(ctx, sqlc.InsertAccountNameVersionParams{
		Op: op, ID: changeID, AccountID: accountID, Lang: lang,
	}); err != nil {
		return fmt.Errorf("version name (%d,%s): %w", accountID, lang, err)
	}
	return nil
}

// insertAccountVersion appends the accounts snapshot-from-live version row. It
// hides the generated positional-param names (ID=change_id, ID_2=entity_id)
// behind one call site. MUST run after the live write.
func insertAccountVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertAccountVersion(ctx, sqlc.InsertAccountVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append account version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// nullInt64Ptr maps a *int64 to sql.NullInt64 (nil -> NULL).
func nullInt64Ptr(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// nullStringPtr maps a *string to sql.NullString (nil or "" -> NULL).
func nullStringPtr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return nullString(*p)
}

// nullStringToPtr maps a sql.NullString back to *string (invalid -> nil). It is
// the inverse of nullStringPtr, used by read projections that surface a nullable
// column (e.g. a user's optional password_hash) as an optional Go value.
func nullStringToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// boolToInt maps a Go bool to the 0/1 SQLite integer flag columns use.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

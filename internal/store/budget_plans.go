package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/db/sqlc"
)

// Budget-PLAN + budget-SPLIT operations (p27.2) -- the NEW split-derived budget
// model (DECISIONS "Budget redesign", 2026-07-17). ADDITIVE: the old schedule/
// budget CRUD in budgets.go is UNTOUCHED and stays the shipped build until p27.3.
//
// A budget = a set of PROJECTED, dated splits stored in a budget-scoped table (NOT
// the real ledger). Each split is SINGLE-legged: its balancing "cash available"
// counter-leg is IMPLICIT and UNNAMED, so a budget-split is NOT a zero-sum
// transaction and is NOT subject to the ledger's per-transaction / per-fund zero-sum
// invariants -- budget-splits are a separate projection plane.
//
// These COPY the versioned-entity discipline the fund/budget ops established: every
// public mutation runs through the write funnel as ONE change; the live write
// happens FIRST, then the snapshot-from-live version append; validation lives inside
// fn so a rejected op rolls the change row back and leaves no audit trace. A split
// can be op='delete' -- it HARD-deletes with a delete version (rule 14; version-
// BEFORE-live-delete). The account/leaf/fund/program checks REUSE the same tx-bound
// sqlc queries the ledger split validator uses (transactions.go resolveSplit), so
// budget-splits obey D18/D20 exactly as real splits do.

// Typed sentinel errors handlers and tests branch on. Wrapped with %w at the call
// site so errors.Is sees them through the funnel.
var (
	// ErrBudgetPlanNotFound: the requested budget plan does not exist.
	ErrBudgetPlanNotFound = errors.New("store: budget plan not found")
	// ErrBudgetSplitNotFound: the requested budget split does not exist.
	ErrBudgetSplitNotFound = errors.New("store: budget split not found")
	// ErrBudgetSplitRefMissing: a referenced plan/subsidiary/account/fund/program/
	// currency does not exist.
	ErrBudgetSplitRefMissing = errors.New("store: budget split reference not found")
	// ErrBudgetSplitAccountType: the split's account is neither revenue/expense nor
	// an open_item receivable/payable -- a budget-split projects an R/E flow or an
	// open-item A/R-A/P line, never a plain balance-sheet position (DECISIONS design).
	ErrBudgetSplitAccountType = errors.New("store: budget split account must be revenue/expense or an open-item receivable/payable")
	// ErrBudgetSplitAccountNotLeaf: the split's account is a placeholder (non-leaf).
	ErrBudgetSplitAccountNotLeaf = errors.New("store: budget split account must be a leaf account")
	// ErrBudgetSplitAccountSub: the split's account is not mapped to the plan's
	// subsidiary (D18).
	ErrBudgetSplitAccountSub = errors.New("store: budget split account not in the plan's subsidiary")
	// ErrBudgetSplitFundScope: the split's fund is inactive or not scoped to the
	// plan's subsidiary (D20).
	ErrBudgetSplitFundScope = errors.New("store: budget split fund inactive or out of subsidiary scope")
	// ErrBudgetSplitProgramRequired: an R/E-categorized split has no program and the
	// account carries no default_program (DECISIONS tension 3: program is required on
	// R/E budget-splits so variance rows line up with actuals).
	ErrBudgetSplitProgramRequired = errors.New("store: budget split program required on revenue/expense splits")
	// ErrBudgetSplitProgramForbidden: an A/L-categorized (open-item) split carries a
	// program -- forbidden, mirroring the ledger's program-on-balance-sheet rule.
	ErrBudgetSplitProgramForbidden = errors.New("store: budget split program forbidden on receivable/payable splits")
	// ErrBudgetSplitProgramScope: the resolved program is inactive or outside the
	// fund's program subtree (D20).
	ErrBudgetSplitProgramScope = errors.New("store: budget split program inactive or out of fund program scope")
)

// --- budget plans ----------------------------------------------------------

// BudgetPlanInput is the desired state of a budget plan (name + subsidiary + notes).
type BudgetPlanInput struct {
	Name         string
	SubsidiaryID int64
	Notes        string
}

// CreateBudgetPlan creates a plan under one change and returns the new id.
func (s *Store) CreateBudgetPlan(ctx context.Context, in BudgetPlanInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "budget_plan.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetSubsidiary(ctx, in.SubsidiaryID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetSplitRefMissing
				}
				return fmt.Errorf("load plan subsidiary %d: %w", in.SubsidiaryID, err)
			}
			id, err := q.InsertBudgetPlan(ctx, sqlc.InsertBudgetPlanParams{
				Name:         in.Name,
				SubsidiaryID: in.SubsidiaryID,
				Notes:        in.Notes,
			})
			if err != nil {
				return fmt.Errorf("insert budget plan: %w", err)
			}
			newID = id
			return insertBudgetPlanVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create budget plan: %w", err)
	}
	return newID, nil
}

// UpdateBudgetPlan replaces a plan's fields under one change.
func (s *Store) UpdateBudgetPlan(ctx context.Context, id int64, in BudgetPlanInput) error {
	_, err := s.write(ctx, "budget_plan.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetPlan(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetPlanNotFound
				}
				return fmt.Errorf("load budget plan %d: %w", id, err)
			}
			if _, err := q.GetSubsidiary(ctx, in.SubsidiaryID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetSplitRefMissing
				}
				return fmt.Errorf("load plan subsidiary %d: %w", in.SubsidiaryID, err)
			}
			if err := q.UpdateBudgetPlan(ctx, sqlc.UpdateBudgetPlanParams{
				Name:         in.Name,
				SubsidiaryID: in.SubsidiaryID,
				Notes:        in.Notes,
				ID:           id,
			}); err != nil {
				return fmt.Errorf("update budget plan %d: %w", id, err)
			}
			return insertBudgetPlanVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update budget plan %d: %w", id, err)
	}
	return nil
}

// DeleteBudgetPlan HARD-deletes a plan and ALL its splits under ONE change (p27.3c).
// It appends an op='delete' version FIRST for every split, then for the plan
// (snapshot-before-delete, rule 14), then removes the splits and the plan inside the
// same write funnel transaction -- so a failure rolls the WHOLE cascade back and the
// audit trail is complete (a delete version exists for each removed row).
func (s *Store) DeleteBudgetPlan(ctx context.Context, id int64) error {
	_, err := s.write(ctx, "budget_plan.delete", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetPlan(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetPlanNotFound
				}
				return fmt.Errorf("load budget plan %d: %w", id, err)
			}
			splits, err := q.ListBudgetSplits(ctx, id)
			if err != nil {
				return fmt.Errorf("list budget splits: %w", err)
			}
			for _, sp := range splits {
				if err := insertBudgetSplitVersion(ctx, q, changeID, "delete", sp.ID); err != nil {
					return err
				}
				if err := q.DeleteBudgetSplit(ctx, sp.ID); err != nil {
					return fmt.Errorf("delete budget split %d: %w", sp.ID, err)
				}
			}
			if err := insertBudgetPlanVersion(ctx, q, changeID, "delete", id); err != nil {
				return err
			}
			if err := q.DeleteBudgetPlan(ctx, id); err != nil {
				return fmt.Errorf("delete budget plan %d: %w", id, err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("delete budget plan %d: %w", id, err)
	}
	return nil
}

// GetBudgetPlan returns a plan's current live row (read; sqlc).
func (s *Store) GetBudgetPlan(ctx context.Context, id int64) (sqlc.BudgetPlan, error) {
	row, err := s.q.GetBudgetPlan(ctx, id)
	if err != nil {
		return sqlc.BudgetPlan{}, fmt.Errorf("store: get budget plan %d: %w", id, err)
	}
	return row, nil
}

// ListBudgetPlans returns every plan, id-ordered (read; sqlc).
func (s *Store) ListBudgetPlans(ctx context.Context) ([]sqlc.BudgetPlan, error) {
	rows, err := s.q.ListBudgetPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list budget plans: %w", err)
	}
	return rows, nil
}

// --- budget splits ---------------------------------------------------------

// BudgetSplitInput is the desired state of a projected budget-split. FundID nil =
// unrestricted; ProgramID nil = unset (defaulted from the account on R/E; must stay
// nil on A/L). Amount is a signed projection (rule 3). Currency is the leg currency.
type BudgetSplitInput struct {
	Description string
	Date        string
	AccountID   int64
	FundID      *int64
	ProgramID   *int64
	Amount      int64
	Currency    string
}

// CreateBudgetSplit adds a projected split to a plan under one change and returns
// the new id. Validates the account (leaf in the plan's subsidiary; R/E or open_item
// A/L), the fund/program refs+scope, and the program-required/forbidden rule inside
// fn (all roll the change back on rejection). The resolved program (prefilled from
// the account default on R/E) is what gets stored.
func (s *Store) CreateBudgetSplit(ctx context.Context, planID int64, in BudgetSplitInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "budget_split.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			resolved, err := resolveBudgetSplit(ctx, q, planID, in)
			if err != nil {
				return err
			}
			id, err := q.InsertBudgetSplit(ctx, sqlc.InsertBudgetSplitParams{
				PlanID:      planID,
				Description: in.Description,
				Date:        in.Date,
				AccountID:   in.AccountID,
				FundID:      nullInt64Ptr(in.FundID),
				ProgramID:   resolved.programID,
				Amount:      in.Amount,
				Currency:    in.Currency,
			})
			if err != nil {
				return fmt.Errorf("insert budget split: %w", err)
			}
			newID = id
			return insertBudgetSplitVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create budget split: %w", err)
	}
	return newID, nil
}

// UpdateBudgetSplit replaces a split's fields under one change (same validation and
// program defaulting as create). The plan is fixed (a split belongs to its plan).
func (s *Store) UpdateBudgetSplit(ctx context.Context, id int64, in BudgetSplitInput) error {
	_, err := s.write(ctx, "budget_split.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetBudgetSplit(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetSplitNotFound
				}
				return fmt.Errorf("load budget split %d: %w", id, err)
			}
			resolved, err := resolveBudgetSplit(ctx, q, cur.PlanID, in)
			if err != nil {
				return err
			}
			if err := q.UpdateBudgetSplit(ctx, sqlc.UpdateBudgetSplitParams{
				Description: in.Description,
				Date:        in.Date,
				AccountID:   in.AccountID,
				FundID:      nullInt64Ptr(in.FundID),
				ProgramID:   resolved.programID,
				Amount:      in.Amount,
				Currency:    in.Currency,
				ID:          id,
			}); err != nil {
				return fmt.Errorf("update budget split %d: %w", id, err)
			}
			return insertBudgetSplitVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update budget split %d: %w", id, err)
	}
	return nil
}

// DeleteBudgetSplit HARD-deletes a split under one change, appending an op='delete'
// version FIRST (snapshot-before-delete, rule 14).
func (s *Store) DeleteBudgetSplit(ctx context.Context, id int64) error {
	_, err := s.write(ctx, "budget_split.delete", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetSplit(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetSplitNotFound
				}
				return fmt.Errorf("load budget split %d: %w", id, err)
			}
			if err := insertBudgetSplitVersion(ctx, q, changeID, "delete", id); err != nil {
				return err
			}
			if err := q.DeleteBudgetSplit(ctx, id); err != nil {
				return fmt.Errorf("delete budget split %d: %w", id, err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("delete budget split %d: %w", id, err)
	}
	return nil
}

// ReplaceBudgetSplits saves a plan's WHOLE split set in ONE change (p27.2c: the
// auto-row grid's bulk submit) -- the ATOMIC replace the grid needs. It deletes every
// existing split (version 'delete' BEFORE the live delete, rule 14) and inserts each
// desired split (validated via resolveBudgetSplit; version 'create'), all inside the
// SAME write funnel transaction, so a rejection rolls the ENTIRE change back and leaves
// the prior splits intact (a per-call delete-then-insert would permanently lose the
// existing splits on a mid-loop failure). On a desired-split rejection it returns the
// FAILING desired index (0-based) so the handler can attach the error to that row; the
// index is -1 for a non-row error (a bad plan id). This is the MIRROR of
// ReplaceExpenseReportLines, minus the by-id diff (a budget-split grid has no per-row
// id round-trip -- the whole set is replaced).
func (s *Store) ReplaceBudgetSplits(ctx context.Context, planID int64, desired []BudgetSplitInput) (int, error) {
	failedIdx := -1
	_, err := s.write(ctx, "budget_plan.replace_splits", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetPlan(ctx, planID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetPlanNotFound
				}
				return fmt.Errorf("load budget plan %d: %w", planID, err)
			}
			// Snapshot the OLD split set (the delete-set) BEFORE any insert, so the delete
			// targets exactly the pre-existing rows regardless of the new inserts' ids.
			existing, err := q.ListBudgetSplits(ctx, planID)
			if err != nil {
				return fmt.Errorf("list budget splits: %w", err)
			}
			// Validate + insert each desired split. A rejection sets failedIdx and returns,
			// rolling the WHOLE change back (one tx) -- the old splits are never deleted.
			for i, in := range desired {
				resolved, err := resolveBudgetSplit(ctx, q, planID, in)
				if err != nil {
					failedIdx = i
					return err
				}
				id, err := q.InsertBudgetSplit(ctx, sqlc.InsertBudgetSplitParams{
					PlanID:      planID,
					Description: in.Description,
					Date:        in.Date,
					AccountID:   in.AccountID,
					FundID:      nullInt64Ptr(in.FundID),
					ProgramID:   resolved.programID,
					Amount:      in.Amount,
					Currency:    in.Currency,
				})
				if err != nil {
					failedIdx = i
					return fmt.Errorf("insert budget split: %w", err)
				}
				if err := insertBudgetSplitVersion(ctx, q, changeID, "create", id); err != nil {
					return err
				}
			}
			// All desired inserts succeeded: delete the pre-existing splits (version BEFORE
			// the live delete, rule 14).
			for _, sp := range existing {
				if err := insertBudgetSplitVersion(ctx, q, changeID, "delete", sp.ID); err != nil {
					return err
				}
				if err := q.DeleteBudgetSplit(ctx, sp.ID); err != nil {
					return fmt.Errorf("delete budget split %d: %w", sp.ID, err)
				}
			}
			return nil
		})
	if err != nil {
		return failedIdx, fmt.Errorf("replace budget splits (plan %d): %w", planID, err)
	}
	return -1, nil
}

// AppendBudgetSplits inserts a batch of splits (the CSV import) in ONE change: every
// row is validated + inserted, and any rejection rolls the WHOLE batch back (so a
// mid-batch failure never leaves a partial append that a retry would then duplicate).
// On a rejection it returns the FAILING desired index (0-based) so the handler can name
// the offending CSV row; -1 for a non-row error.
func (s *Store) AppendBudgetSplits(ctx context.Context, planID int64, splits []BudgetSplitInput) (int, error) {
	failedIdx := -1
	_, err := s.write(ctx, "budget_plan.append_splits", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetPlan(ctx, planID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetPlanNotFound
				}
				return fmt.Errorf("load budget plan %d: %w", planID, err)
			}
			for i, in := range splits {
				resolved, err := resolveBudgetSplit(ctx, q, planID, in)
				if err != nil {
					failedIdx = i
					return err
				}
				id, err := q.InsertBudgetSplit(ctx, sqlc.InsertBudgetSplitParams{
					PlanID:      planID,
					Description: in.Description,
					Date:        in.Date,
					AccountID:   in.AccountID,
					FundID:      nullInt64Ptr(in.FundID),
					ProgramID:   resolved.programID,
					Amount:      in.Amount,
					Currency:    in.Currency,
				})
				if err != nil {
					failedIdx = i
					return fmt.Errorf("insert budget split: %w", err)
				}
				if err := insertBudgetSplitVersion(ctx, q, changeID, "create", id); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return failedIdx, fmt.Errorf("append budget splits (plan %d): %w", planID, err)
	}
	return -1, nil
}

// GetBudgetSplit returns a split's current live row (read; sqlc).
func (s *Store) GetBudgetSplit(ctx context.Context, id int64) (sqlc.BudgetSplit, error) {
	row, err := s.q.GetBudgetSplit(ctx, id)
	if err != nil {
		return sqlc.BudgetSplit{}, fmt.Errorf("store: get budget split %d: %w", id, err)
	}
	return row, nil
}

// BudgetSplits returns a plan's splits, date-then-id-ordered (read; sqlc).
func (s *Store) BudgetSplits(ctx context.Context, planID int64) ([]sqlc.BudgetSplit, error) {
	rows, err := s.q.ListBudgetSplits(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("store: budget plan %d splits: %w", planID, err)
	}
	return rows, nil
}

// --- validation (unexported) -----------------------------------------------

// resolvedBudgetSplit is a split after program defaulting -- the value the store
// persists (currently only the resolved program is derived; everything else is
// taken from the input verbatim).
type resolvedBudgetSplit struct {
	programID sql.NullInt64
}

// resolveBudgetSplit enforces the p27.2 budget-split rules on the tx-bound queries
// and returns the resolved program (prefilled from the account default on R/E). It
// REUSES the ledger split validator's sqlc queries (AccountIsLeaf,
// HasAccountSubsidiaryMap, GetFund/HasFundSubsidiaryMap, GetProgram/
// ProgramSubtreeIDs) so budget-splits obey D18/D20 exactly as real splits do, minus
// the zero-sum/currency invariants (a budget-split is single-legged). Runs inside fn
// so a rejection rolls the change back.
func resolveBudgetSplit(ctx context.Context, q *sqlc.Queries, planID int64, in BudgetSplitInput) (resolvedBudgetSplit, error) {
	plan, err := q.GetBudgetPlan(ctx, planID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resolvedBudgetSplit{}, ErrBudgetPlanNotFound
		}
		return resolvedBudgetSplit{}, fmt.Errorf("load budget plan %d: %w", planID, err)
	}

	acct, err := q.GetAccount(ctx, in.AccountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resolvedBudgetSplit{}, ErrBudgetSplitRefMissing
		}
		return resolvedBudgetSplit{}, fmt.Errorf("load budget split account %d: %w", in.AccountID, err)
	}

	// Account is a leaf (D11).
	leaf, err := q.AccountIsLeaf(ctx, sql.NullInt64{Int64: in.AccountID, Valid: true})
	if err != nil {
		return resolvedBudgetSplit{}, fmt.Errorf("budget split leaf check %d: %w", in.AccountID, err)
	}
	if !leaf {
		return resolvedBudgetSplit{}, ErrBudgetSplitAccountNotLeaf
	}

	// Account category: revenue/expense OR an open_item receivable/payable (asset/
	// liability). Plain balance-sheet accounts and non-open_item A/L are rejected.
	isRE := acct.Type == "revenue" || acct.Type == "expense"
	isOpenItemAL := acct.OpenItem == 1 && (acct.Type == "asset" || acct.Type == "liability")
	if !isRE && !isOpenItemAL {
		return resolvedBudgetSplit{}, ErrBudgetSplitAccountType
	}

	// Account mapped to the plan's subsidiary (D18).
	mapped, err := q.HasAccountSubsidiaryMap(ctx, sqlc.HasAccountSubsidiaryMapParams{AccountID: in.AccountID, SubsidiaryID: plan.SubsidiaryID})
	if err != nil {
		return resolvedBudgetSplit{}, fmt.Errorf("budget split account-sub map %d: %w", in.AccountID, err)
	}
	if !mapped {
		return resolvedBudgetSplit{}, ErrBudgetSplitAccountSub
	}

	// Currency exists.
	if _, err := q.GetCurrency(ctx, in.Currency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resolvedBudgetSplit{}, ErrBudgetSplitRefMissing
		}
		return resolvedBudgetSplit{}, fmt.Errorf("load budget split currency %q: %w", in.Currency, err)
	}

	// Fund (if set): active + scoped to the plan's subsidiary (D20).
	var fund *sqlc.Fund
	if in.FundID != nil {
		f, err := q.GetFund(ctx, *in.FundID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return resolvedBudgetSplit{}, ErrBudgetSplitRefMissing
			}
			return resolvedBudgetSplit{}, fmt.Errorf("load budget split fund %d: %w", *in.FundID, err)
		}
		if f.Active == 0 {
			return resolvedBudgetSplit{}, ErrBudgetSplitFundScope
		}
		scoped, err := q.HasFundSubsidiaryMap(ctx, sqlc.HasFundSubsidiaryMapParams{FundID: *in.FundID, SubsidiaryID: plan.SubsidiaryID})
		if err != nil {
			return resolvedBudgetSplit{}, fmt.Errorf("budget split fund-sub scope %d: %w", *in.FundID, err)
		}
		if !scoped {
			return resolvedBudgetSplit{}, ErrBudgetSplitFundScope
		}
		fund = &f
	}

	// Program: REQUIRED on R/E (prefill from account.default_program like the ledger
	// does; reject if still unset), FORBIDDEN on A/L (DECISIONS tension 3).
	var programID sql.NullInt64
	if isRE {
		var pid int64
		switch {
		case in.ProgramID != nil:
			pid = *in.ProgramID
		case acct.DefaultProgramID.Valid:
			pid = acct.DefaultProgramID.Int64
		default:
			return resolvedBudgetSplit{}, ErrBudgetSplitProgramRequired
		}
		prog, err := q.GetProgram(ctx, pid)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return resolvedBudgetSplit{}, ErrBudgetSplitRefMissing
			}
			return resolvedBudgetSplit{}, fmt.Errorf("load budget split program %d: %w", pid, err)
		}
		if prog.Active == 0 {
			return resolvedBudgetSplit{}, ErrBudgetSplitProgramScope
		}
		// Fund-program scope (R/E only): the resolved program must be inside the
		// fund's program subtree if the fund has one (D20).
		if fund != nil && fund.ProgramID.Valid {
			ids, err := q.ProgramSubtreeIDs(ctx, fund.ProgramID.Int64)
			if err != nil {
				return resolvedBudgetSplit{}, fmt.Errorf("budget split fund program subtree %d: %w", fund.ProgramID.Int64, err)
			}
			inScope := false
			for _, sid := range ids {
				if sid == pid {
					inScope = true
					break
				}
			}
			if !inScope {
				return resolvedBudgetSplit{}, ErrBudgetSplitProgramScope
			}
		}
		programID = sql.NullInt64{Int64: pid, Valid: true}
	} else if in.ProgramID != nil {
		return resolvedBudgetSplit{}, ErrBudgetSplitProgramForbidden
	}

	return resolvedBudgetSplit{programID: programID}, nil
}

// insertBudgetPlanVersion appends the budget_plans snapshot-from-live version row.
func insertBudgetPlanVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertBudgetPlanVersion(ctx, sqlc.InsertBudgetPlanVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append budget plan version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// insertBudgetSplitVersion appends the budget_splits snapshot-from-live version row.
// For op='delete' it MUST run BEFORE the live delete (see DeleteBudgetSplit).
func insertBudgetSplitVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertBudgetSplitVersion(ctx, sqlc.InsertBudgetSplitVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append budget split version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

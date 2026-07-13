package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cuento/internal/budget"
	"cuento/internal/db/sqlc"
)

// Budget operations (p19.1) -- named, reusable SCHEDULES + budgets + budget LINES,
// the DISCRETE-dated-occurrence budgeting model (PLAN Phase 19; NO pro-rata). These
// COPY the versioned-entity discipline the fund ops (p07.3) established: every
// public mutation runs through the write funnel as ONE change; inside fn the live
// write happens FIRST, then the snapshot-from-live version append; validation lives
// inside fn so a rejected op rolls the change row back and leaves no audit trace.
//
// A schedule's custom date list (budget_schedule_dates) is a COMPOSITE (schedule
// _id, occurs_on) SET, versioned exactly like fund_subsidiaries: an import is
// op='create', a removal op='delete' (version-BEFORE-live-delete). A budget line
// can be op='delete' -- it HARD-deletes with a delete version (rule 14: soft-delete
// is transactions-only; everything else audits its delete via a snapshot-before-
// delete). The pure date math lives in internal/budget (ExpandSchedule); the store
// only persists + validates, and reuses ExpandSchedule to validate a schedule's
// field consistency.

// Typed sentinel errors handlers and tests branch on (AGENTS Style). Wrapped with
// %w at the call site so errors.Is sees them through the funnel.
var (
	// ErrScheduleInvalid: a schedule's fields are inconsistent for its kind (e.g.
	// monthly with neither day-of-month nor ordinal, biweekly without an anchor,
	// custom with no dates). The pure engine is the arbiter.
	ErrScheduleInvalid = errors.New("store: budget schedule fields invalid for its kind")
	// ErrScheduleNotFound: the requested schedule does not exist.
	ErrScheduleNotFound = errors.New("store: budget schedule not found")
	// ErrBudgetNotFound: the requested budget does not exist.
	ErrBudgetNotFound = errors.New("store: budget not found")
	// ErrBudgetLineNotFound: the requested budget line does not exist.
	ErrBudgetLineNotFound = errors.New("store: budget line not found")
	// ErrBudgetLineAccountNotRE: a budget line's account is not revenue/expense --
	// a budget is of R/E FLOWS, so a balance-sheet (asset/liability/equity) account
	// is rejected (p19.1).
	ErrBudgetLineAccountNotRE = errors.New("store: budget line account must be revenue or expense")
	// ErrBudgetRefMissing: a referenced budget/subsidiary/fund/program/schedule/
	// currency does not exist.
	ErrBudgetRefMissing = errors.New("store: budget line reference not found")
	// ErrBudgetAmount: a budget-line amount is not a positive magnitude (per
	// occurrence; direction comes from the account type at report time).
	ErrBudgetAmount = errors.New("store: budget line amount must be positive")
)

// ScheduleInput is the desired state of a NAMED schedule. Nullable ints use *int
// (nil = unset); the month-end sentinel is -1 (DayOfMonth/DayOfMonth2) and the
// "last" ordinal is -1. CustomDates is the imported list (only for kind='custom').
// WeekendAdjust "" defaults to prev_business_day (the engine + schema agree).
type ScheduleInput struct {
	Name          string
	Kind          string
	DayOfMonth    *int
	DayOfMonth2   *int
	Ordinal       *int
	Weekday       *int
	AnchorDate    *string
	WeekendAdjust string
	Notes         string
	CustomDates   []string
}

// CreateSchedule creates a named schedule (+ its custom date list, if any) under
// ONE change and returns the new id. The schedule's create version and each date's
// create version share that change. Field consistency is validated via the pure
// engine (ErrScheduleInvalid) inside fn so a rejection rolls back cleanly.
func (s *Store) CreateSchedule(ctx context.Context, in ScheduleInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "budget_schedule.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := validateScheduleFields(in); err != nil {
				return err
			}
			id, err := q.InsertBudgetSchedule(ctx, sqlc.InsertBudgetScheduleParams{
				Name:          in.Name,
				Kind:          in.Kind,
				DayOfMonth:    nullIntPtr(in.DayOfMonth),
				DayOfMonth2:   nullIntPtr(in.DayOfMonth2),
				Ordinal:       nullIntPtr(in.Ordinal),
				Weekday:       nullIntPtr(in.Weekday),
				AnchorDate:    nullStringPtr(in.AnchorDate),
				WeekendAdjust: weekendOrDefault(in.WeekendAdjust),
				Notes:         in.Notes,
			})
			if err != nil {
				return fmt.Errorf("insert budget schedule: %w", err)
			}
			newID = id
			if err := insertScheduleVersion(ctx, q, changeID, "create", id); err != nil {
				return err
			}
			// Import the custom date list (deduped), each a versioned 'create'.
			return importScheduleDates(ctx, q, changeID, id, in.CustomDates)
		})
	if err != nil {
		return 0, fmt.Errorf("create schedule: %w", err)
	}
	return newID, nil
}

// UpdateSchedule replaces a schedule's fields and (when CustomDates is non-nil) its
// imported date set under one change. A nil CustomDates leaves the date set alone;
// a non-nil slice diffs it (imports new dates op='create', removes dropped ones
// op='delete', version-before-delete). The schedule version reflects the NEW
// fields. Field consistency is re-validated.
func (s *Store) UpdateSchedule(ctx context.Context, id int64, in ScheduleInput) error {
	_, err := s.write(ctx, "budget_schedule.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetSchedule(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrScheduleNotFound
				}
				return fmt.Errorf("load schedule %d: %w", id, err)
			}
			if err := validateScheduleFields(in); err != nil {
				return err
			}
			if err := q.UpdateBudgetSchedule(ctx, sqlc.UpdateBudgetScheduleParams{
				Name:          in.Name,
				Kind:          in.Kind,
				DayOfMonth:    nullIntPtr(in.DayOfMonth),
				DayOfMonth2:   nullIntPtr(in.DayOfMonth2),
				Ordinal:       nullIntPtr(in.Ordinal),
				Weekday:       nullIntPtr(in.Weekday),
				AnchorDate:    nullStringPtr(in.AnchorDate),
				WeekendAdjust: weekendOrDefault(in.WeekendAdjust),
				Notes:         in.Notes,
				ID:            id,
			}); err != nil {
				return fmt.Errorf("update schedule %d: %w", id, err)
			}
			if err := insertScheduleVersion(ctx, q, changeID, "update", id); err != nil {
				return err
			}
			if in.CustomDates != nil {
				return diffScheduleDates(ctx, q, changeID, id, in.CustomDates)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("update schedule %d: %w", id, err)
	}
	return nil
}

// GetSchedule returns a schedule's current live row (read; sqlc).
func (s *Store) GetSchedule(ctx context.Context, id int64) (sqlc.BudgetSchedule, error) {
	row, err := s.q.GetBudgetSchedule(ctx, id)
	if err != nil {
		return sqlc.BudgetSchedule{}, fmt.Errorf("store: get schedule %d: %w", id, err)
	}
	return row, nil
}

// ListSchedules returns every named schedule, id-ordered (read; sqlc). The schedule
// LIBRARY list (p19.3) and the budget-line editor's schedule picker both source
// their options here. A thin wrapper over the ListBudgetSchedules query so the web
// handler renders what the store returns.
func (s *Store) ListSchedules(ctx context.Context) ([]sqlc.BudgetSchedule, error) {
	rows, err := s.q.ListBudgetSchedules(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list schedules: %w", err)
	}
	return rows, nil
}

// ListBudgets returns every budget, id-ordered (read; sqlc). The budget LIST page
// (p19.3) sources its rows here.
func (s *Store) ListBudgets(ctx context.Context) ([]sqlc.Budget, error) {
	rows, err := s.q.ListBudgets(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list budgets: %w", err)
	}
	return rows, nil
}

// ScheduleDates returns a schedule's imported custom date list, sorted (read).
func (s *Store) ScheduleDates(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.q.BudgetScheduleDates(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: schedule %d dates: %w", id, err)
	}
	return rows, nil
}

// --- budgets ---------------------------------------------------------------

// BudgetInput is the desired state of a budget (the period bounds + name/notes).
type BudgetInput struct {
	Name        string
	PeriodStart string
	PeriodEnd   string
	Notes       string
}

// CreateBudget creates a budget under one change and returns the new id.
func (s *Store) CreateBudget(ctx context.Context, in BudgetInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "budget.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			id, err := q.InsertBudget(ctx, sqlc.InsertBudgetParams{
				Name:        in.Name,
				PeriodStart: in.PeriodStart,
				PeriodEnd:   in.PeriodEnd,
				Notes:       in.Notes,
			})
			if err != nil {
				return fmt.Errorf("insert budget: %w", err)
			}
			newID = id
			return insertBudgetVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create budget: %w", err)
	}
	return newID, nil
}

// UpdateBudget replaces a budget's fields under one change.
func (s *Store) UpdateBudget(ctx context.Context, id int64, in BudgetInput) error {
	_, err := s.write(ctx, "budget.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudget(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetNotFound
				}
				return fmt.Errorf("load budget %d: %w", id, err)
			}
			if err := q.UpdateBudget(ctx, sqlc.UpdateBudgetParams{
				Name:        in.Name,
				PeriodStart: in.PeriodStart,
				PeriodEnd:   in.PeriodEnd,
				Notes:       in.Notes,
				ID:          id,
			}); err != nil {
				return fmt.Errorf("update budget %d: %w", id, err)
			}
			return insertBudgetVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update budget %d: %w", id, err)
	}
	return nil
}

// GetBudget returns a budget's current live row (read; sqlc).
func (s *Store) GetBudget(ctx context.Context, id int64) (sqlc.Budget, error) {
	row, err := s.q.GetBudget(ctx, id)
	if err != nil {
		return sqlc.Budget{}, fmt.Errorf("store: get budget %d: %w", id, err)
	}
	return row, nil
}

// --- budget lines ----------------------------------------------------------

// BudgetLineInput is the desired state of a budget line: the (subsidiary, account,
// fund, program) key + amount-per-occurrence + currency + schedule ref. FundID nil
// = unrestricted. account must be revenue/expense (validated).
type BudgetLineInput struct {
	SubsidiaryID int64
	AccountID    int64
	FundID       *int64
	ProgramID    int64
	Amount       int64
	Currency     string
	ScheduleID   int64
}

// CreateBudgetLine adds a line to a budget under one change and returns the new id.
// Validates the R/E account, the fund/program/sub/schedule/currency refs, and a
// positive amount inside fn (all roll the change back on rejection).
func (s *Store) CreateBudgetLine(ctx context.Context, budgetID int64, in BudgetLineInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "budget_line.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := validateBudgetLine(ctx, q, budgetID, in); err != nil {
				return err
			}
			id, err := q.InsertBudgetLine(ctx, sqlc.InsertBudgetLineParams{
				BudgetID:     budgetID,
				SubsidiaryID: in.SubsidiaryID,
				AccountID:    in.AccountID,
				FundID:       nullInt64Ptr(in.FundID),
				ProgramID:    in.ProgramID,
				Amount:       in.Amount,
				Currency:     in.Currency,
				ScheduleID:   in.ScheduleID,
			})
			if err != nil {
				return fmt.Errorf("insert budget line: %w", err)
			}
			newID = id
			return insertBudgetLineVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create budget line: %w", err)
	}
	return newID, nil
}

// UpdateBudgetLine replaces a budget line's fields under one change (same
// validation as create). The budget id is fixed (a line belongs to its budget).
func (s *Store) UpdateBudgetLine(ctx context.Context, id int64, in BudgetLineInput) error {
	_, err := s.write(ctx, "budget_line.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			cur, err := q.GetBudgetLine(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetLineNotFound
				}
				return fmt.Errorf("load budget line %d: %w", id, err)
			}
			if err := validateBudgetLine(ctx, q, cur.BudgetID, in); err != nil {
				return err
			}
			if err := q.UpdateBudgetLine(ctx, sqlc.UpdateBudgetLineParams{
				BudgetID:     cur.BudgetID,
				SubsidiaryID: in.SubsidiaryID,
				AccountID:    in.AccountID,
				FundID:       nullInt64Ptr(in.FundID),
				ProgramID:    in.ProgramID,
				Amount:       in.Amount,
				Currency:     in.Currency,
				ScheduleID:   in.ScheduleID,
				ID:           id,
			}); err != nil {
				return fmt.Errorf("update budget line %d: %w", id, err)
			}
			return insertBudgetLineVersion(ctx, q, changeID, "update", id)
		})
	if err != nil {
		return fmt.Errorf("update budget line %d: %w", id, err)
	}
	return nil
}

// DeleteBudgetLine HARD-deletes a budget line under one change, appending an
// op='delete' version FIRST (snapshot-before-delete). Rule 14: soft-delete is
// reserved for transactions; every other entity that deletes audits it with a
// delete version, leaving no live row but a permanent snapshot.
func (s *Store) DeleteBudgetLine(ctx context.Context, id int64) error {
	_, err := s.write(ctx, "budget_line.delete", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetBudgetLine(ctx, id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBudgetLineNotFound
				}
				return fmt.Errorf("load budget line %d: %w", id, err)
			}
			// Version BEFORE the live delete (the row must still exist to snapshot).
			if err := insertBudgetLineVersion(ctx, q, changeID, "delete", id); err != nil {
				return err
			}
			if err := q.DeleteBudgetLine(ctx, id); err != nil {
				return fmt.Errorf("delete budget line %d: %w", id, err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("delete budget line %d: %w", id, err)
	}
	return nil
}

// GetBudgetLine returns a budget line's current live row (read; sqlc).
func (s *Store) GetBudgetLine(ctx context.Context, id int64) (sqlc.BudgetLine, error) {
	row, err := s.q.GetBudgetLine(ctx, id)
	if err != nil {
		return sqlc.BudgetLine{}, fmt.Errorf("store: get budget line %d: %w", id, err)
	}
	return row, nil
}

// BudgetLines returns a budget's lines, id-ordered (read; sqlc).
func (s *Store) BudgetLines(ctx context.Context, budgetID int64) ([]sqlc.BudgetLine, error) {
	rows, err := s.q.ListBudgetLines(ctx, budgetID)
	if err != nil {
		return nil, fmt.Errorf("store: budget %d lines: %w", budgetID, err)
	}
	return rows, nil
}

// --- helpers (unexported) --------------------------------------------------

// validateScheduleFields checks a schedule's fields are consistent for its kind by
// feeding the PURE engine (ExpandSchedule) a trivial 1-day horizon: the engine's
// per-kind field validation runs regardless of whether any occurrence lands, so a
// missing/contradictory selector surfaces as ErrScheduleInvalid. This keeps the
// single source of the date-math rules in internal/budget (no duplicated per-kind
// checks in the store).
func validateScheduleFields(in ScheduleInput) error {
	probeDates := in.CustomDates
	// A custom schedule's date list is validated where dates are SUPPLIED (create's
	// import, and update only when CustomDates != nil), NOT on every field pass: a
	// name-only UpdateSchedule of a custom schedule legitimately carries no dates
	// and must not be rejected for emptiness (the live list is left untouched). So
	// for the field-validation probe only, feed a placeholder date when the kind is
	// custom and none were supplied -- any real dates present are still parsed +
	// checked, so no date-content validation is lost.
	if in.Kind == budget.KindCustom && len(probeDates) == 0 {
		probeDates = []string{"2000-01-01"}
	}
	sched := budget.Schedule{
		Kind:          in.Kind,
		DayOfMonth:    intOrZero(in.DayOfMonth),
		DayOfMonth2:   intOrZero(in.DayOfMonth2),
		Ordinal:       intOrZero(in.Ordinal),
		Weekday:       intOrZero(in.Weekday),
		AnchorDate:    stringOrEmpty(in.AnchorDate),
		WeekendAdjust: in.WeekendAdjust,
		CustomDates:   probeDates,
	}
	// A fixed, valid 1-day horizon exercises the engine's field validation without
	// depending on any occurrence landing.
	if _, err := budget.ExpandSchedule(sched, "2000-01-01", "2000-01-01"); err != nil {
		return fmt.Errorf("%w: %v", ErrScheduleInvalid, err)
	}
	return nil
}

// validateBudgetLine enforces the p19.1 line rules on the tx-bound queries: the
// budget exists; the account is revenue/expense (a budget is of R/E flows); the
// subsidiary/program/schedule/currency exist; the fund exists if set; the amount
// is a positive magnitude. Runs inside fn so a rejection rolls the change back.
func validateBudgetLine(ctx context.Context, q *sqlc.Queries, budgetID int64, in BudgetLineInput) error {
	if in.Amount <= 0 {
		return ErrBudgetAmount
	}
	if _, err := q.GetBudget(ctx, budgetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetNotFound
		}
		return fmt.Errorf("load budget %d: %w", budgetID, err)
	}
	if _, err := q.GetSubsidiary(ctx, in.SubsidiaryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetRefMissing
		}
		return fmt.Errorf("load budget line subsidiary %d: %w", in.SubsidiaryID, err)
	}
	acct, err := q.GetAccount(ctx, in.AccountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetRefMissing
		}
		return fmt.Errorf("load budget line account %d: %w", in.AccountID, err)
	}
	if acct.Type != "revenue" && acct.Type != "expense" {
		return ErrBudgetLineAccountNotRE
	}
	if _, err := q.GetProgram(ctx, in.ProgramID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetRefMissing
		}
		return fmt.Errorf("load budget line program %d: %w", in.ProgramID, err)
	}
	if in.FundID != nil {
		if _, err := q.GetFund(ctx, *in.FundID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrBudgetRefMissing
			}
			return fmt.Errorf("load budget line fund %d: %w", *in.FundID, err)
		}
	}
	if _, err := q.GetCurrency(ctx, in.Currency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetRefMissing
		}
		return fmt.Errorf("load budget line currency %q: %w", in.Currency, err)
	}
	if _, err := q.GetBudgetSchedule(ctx, in.ScheduleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetRefMissing
		}
		return fmt.Errorf("load budget line schedule %d: %w", in.ScheduleID, err)
	}
	return nil
}

// importScheduleDates adds each (deduped) custom date to a schedule, versioning
// each op='create'. Live-write-FIRST then snapshot-from-live.
func importScheduleDates(ctx context.Context, q *sqlc.Queries, changeID, scheduleID int64, dates []string) error {
	seen := make(map[string]bool, len(dates))
	for _, d := range dates {
		if seen[d] {
			continue
		}
		seen[d] = true
		if err := addScheduleDate(ctx, q, changeID, scheduleID, d); err != nil {
			return err
		}
	}
	return nil
}

// diffScheduleDates reconciles a schedule's live date set to the desired set:
// imports new dates (op='create'), removes dropped ones (op='delete', version-
// before-delete). Deduplicates the desired list.
func diffScheduleDates(ctx context.Context, q *sqlc.Queries, changeID, scheduleID int64, dates []string) error {
	want := make(map[string]bool, len(dates))
	for _, d := range dates {
		want[d] = true
	}
	haveList, err := q.BudgetScheduleDates(ctx, scheduleID)
	if err != nil {
		return fmt.Errorf("load schedule %d dates: %w", scheduleID, err)
	}
	have := make(map[string]bool, len(haveList))
	for _, d := range haveList {
		have[d] = true
	}
	for d := range have {
		if want[d] {
			continue
		}
		if err := removeScheduleDate(ctx, q, changeID, scheduleID, d); err != nil {
			return err
		}
	}
	for d := range want {
		if have[d] {
			continue
		}
		if err := addScheduleDate(ctx, q, changeID, scheduleID, d); err != nil {
			return err
		}
	}
	return nil
}

// addScheduleDate imports one date live then versions it op='create'.
func addScheduleDate(ctx context.Context, q *sqlc.Queries, changeID, scheduleID int64, occursOn string) error {
	if err := q.InsertBudgetScheduleDate(ctx, sqlc.InsertBudgetScheduleDateParams{ScheduleID: scheduleID, OccursOn: occursOn}); err != nil {
		return fmt.Errorf("import schedule date (%d,%s): %w", scheduleID, occursOn, err)
	}
	if err := q.InsertBudgetScheduleDateVersion(ctx, sqlc.InsertBudgetScheduleDateVersionParams{
		Op: "create", ID: changeID, ScheduleID: scheduleID, OccursOn: occursOn,
	}); err != nil {
		return fmt.Errorf("version schedule date add (%d,%s): %w", scheduleID, occursOn, err)
	}
	return nil
}

// removeScheduleDate versions op='delete' BEFORE deleting the live date (the row
// must still exist to snapshot -- the removal-op ordering, mirroring removeFundSub).
func removeScheduleDate(ctx context.Context, q *sqlc.Queries, changeID, scheduleID int64, occursOn string) error {
	if err := q.InsertBudgetScheduleDateVersion(ctx, sqlc.InsertBudgetScheduleDateVersionParams{
		Op: "delete", ID: changeID, ScheduleID: scheduleID, OccursOn: occursOn,
	}); err != nil {
		return fmt.Errorf("version schedule date remove (%d,%s): %w", scheduleID, occursOn, err)
	}
	if err := q.DeleteBudgetScheduleDate(ctx, sqlc.DeleteBudgetScheduleDateParams{ScheduleID: scheduleID, OccursOn: occursOn}); err != nil {
		return fmt.Errorf("remove schedule date (%d,%s): %w", scheduleID, occursOn, err)
	}
	return nil
}

// insertScheduleVersion appends the budget_schedules snapshot-from-live version row
// (MUST run after the live write). Hides the ID/ID_2 positional-param names.
func insertScheduleVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertBudgetScheduleVersion(ctx, sqlc.InsertBudgetScheduleVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append schedule version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// insertBudgetVersion appends the budgets snapshot-from-live version row.
func insertBudgetVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertBudgetVersion(ctx, sqlc.InsertBudgetVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append budget version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// insertBudgetLineVersion appends the budget_lines snapshot-from-live version row.
// For op='delete' it MUST run BEFORE the live delete (see DeleteBudgetLine).
func insertBudgetLineVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertBudgetLineVersion(ctx, sqlc.InsertBudgetLineVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append budget line version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// weekendOrDefault maps "" to the schema/engine default weekend policy so an
// unspecified policy is stored as the explicit default (matching the column
// DEFAULT and the engine's own fallback).
func weekendOrDefault(policy string) string {
	if policy == "" {
		return budget.DefaultWeekend
	}
	return policy
}

// nullIntPtr maps a *int to sql.NullInt64 (nil -> NULL) for the schedule's nullable
// int fields. The int inverse of nullInt64Ptr (*int64) for the *int inputs.
func nullIntPtr(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

// intOrZero maps a *int to its value or 0 (the engine's "unset" sentinel).
func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// stringOrEmpty maps a *string to its value or "".
func stringOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

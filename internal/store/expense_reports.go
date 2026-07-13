package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cuento/internal/db/sqlc"
)

// Expense-report operations (p20.1) -- a submission->review workflow DECOUPLED from
// book-editing (Phase 20). A low-privilege SUBMITTER (a user with the standalone
// can_submit_expenses capability, INDEPENDENT of txn_perm) drafts a report of
// proposed revenue/expense lines that need NOT balance, then submits it; an editing
// REVIEWER (p20.3) later CONVERTS it into a real balanced ledger transaction (linking
// it via posted_transaction_id) or REJECTS it with a reason routing it back.
//
// These COPY the versioned-entity discipline the budget ops (p19.1) established:
// every public mutation runs through the write funnel as ONE change; inside fn the
// live write happens FIRST, then the snapshot-from-live version append; validation
// (including the STATE MACHINE) lives inside fn so a rejected op rolls the change row
// back and leaves NO audit trace. A report line can be op='delete' -- it HARD-deletes
// with a delete version (rule 14: soft-delete is transactions-only).
//
// State machine (store-enforced; the schema CHECK is only a backstop):
//
//	draft   --submit--> submitted --reject--> rejected --resubmit--> submitted
//	                    submitted --convert--> converted (TERMINAL, immutable)
//
// Lines are editable only while draft or rejected. posted_transaction_id is set ONLY
// on convert (its sole writer).

// Typed sentinel errors handlers and tests branch on (AGENTS Style). Wrapped with %w
// at the call site so errors.Is sees them through the funnel.
var (
	// ErrExpenseReportNotFound: the requested report does not exist.
	ErrExpenseReportNotFound = errors.New("store: expense report not found")
	// ErrExpenseReportLineNotFound: the requested report line does not exist.
	ErrExpenseReportLineNotFound = errors.New("store: expense report line not found")
	// ErrExpenseReportState: an illegal state-machine transition (e.g. convert a
	// draft, resubmit a submitted report).
	ErrExpenseReportState = errors.New("store: illegal expense report state transition")
	// ErrExpenseReportImmutable: a mutation of a CONVERTED report (terminal, immutable).
	ErrExpenseReportImmutable = errors.New("store: converted expense report is immutable")
	// ErrExpenseReportEmpty: submit was attempted on a report with no lines (>= 1 line
	// required).
	ErrExpenseReportEmpty = errors.New("store: expense report has no lines")
	// ErrExpenseReportReasonRequired: reject was called with an empty reason.
	ErrExpenseReportReasonRequired = errors.New("store: expense report reject reason required")
	// ErrExpenseReportTxnMissing: convert referenced a transaction that does not exist.
	ErrExpenseReportTxnMissing = errors.New("store: posted transaction not found")
	// ErrExpenseReportRefMissing: a referenced submitter/subsidiary/account/fund/
	// program does not exist.
	ErrExpenseReportRefMissing = errors.New("store: expense report reference not found")
)

// CreateExpenseReport creates a draft report for submitterID in subsidiaryID under
// ONE change and returns the new id (status=draft). Validates the submitter +
// subsidiary exist inside fn so a rejection rolls back cleanly.
func (s *Store) CreateExpenseReport(ctx context.Context, submitterID, subsidiaryID int64) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "expense_report.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if _, err := q.GetUser(ctx, submitterID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrExpenseReportRefMissing
				}
				return fmt.Errorf("load submitter %d: %w", submitterID, err)
			}
			if _, err := q.GetSubsidiary(ctx, subsidiaryID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrExpenseReportRefMissing
				}
				return fmt.Errorf("load subsidiary %d: %w", subsidiaryID, err)
			}
			id, err := q.InsertExpenseReport(ctx, sqlc.InsertExpenseReportParams{
				SubmitterID:  submitterID,
				SubsidiaryID: subsidiaryID,
				CreatedAt:    s.now().Format(time.RFC3339Nano),
			})
			if err != nil {
				return fmt.Errorf("insert expense report: %w", err)
			}
			newID = id
			return insertExpenseReportVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("create expense report: %w", err)
	}
	return newID, nil
}

// ExpenseReportLineInput is the desired state of one proposed split: an account, a
// SIGNED minor-unit amount (the report need not balance), and optional fund/program
// (nil = the reviewer resolves at convert) + a free-text memo.
type ExpenseReportLineInput struct {
	AccountID int64
	Amount    int64
	FundID    *int64
	ProgramID *int64
	Memo      string
}

// AddExpenseReportLine adds a line to a report (allowed only while draft|rejected)
// under one change and returns the new id. Validates the account (+ fund/program if
// set) exist inside fn.
func (s *Store) AddExpenseReportLine(ctx context.Context, reportID int64, in ExpenseReportLineInput) (int64, error) {
	var newID int64
	_, err := s.write(ctx, "expense_report_line.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			if err := requireEditable(ctx, q, reportID); err != nil {
				return err
			}
			if err := validateExpenseReportLine(ctx, q, in); err != nil {
				return err
			}
			id, err := q.InsertExpenseReportLine(ctx, sqlc.InsertExpenseReportLineParams{
				ReportID:  reportID,
				AccountID: in.AccountID,
				Amount:    in.Amount,
				FundID:    nullInt64Ptr(in.FundID),
				ProgramID: nullInt64Ptr(in.ProgramID),
				Memo:      in.Memo,
			})
			if err != nil {
				return fmt.Errorf("insert expense report line: %w", err)
			}
			newID = id
			return insertExpenseReportLineVersion(ctx, q, changeID, "create", id)
		})
	if err != nil {
		return 0, fmt.Errorf("add expense report line: %w", err)
	}
	return newID, nil
}

// UpdateExpenseReportLine replaces a line's fields (allowed only while the parent
// report is draft|rejected) under one change.
func (s *Store) UpdateExpenseReportLine(ctx context.Context, lineID int64, in ExpenseReportLineInput) error {
	_, err := s.write(ctx, "expense_report_line.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			line, err := q.GetExpenseReportLine(ctx, lineID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrExpenseReportLineNotFound
				}
				return fmt.Errorf("load expense report line %d: %w", lineID, err)
			}
			if err := requireEditable(ctx, q, line.ReportID); err != nil {
				return err
			}
			if err := validateExpenseReportLine(ctx, q, in); err != nil {
				return err
			}
			if err := q.UpdateExpenseReportLine(ctx, sqlc.UpdateExpenseReportLineParams{
				AccountID: in.AccountID,
				Amount:    in.Amount,
				FundID:    nullInt64Ptr(in.FundID),
				ProgramID: nullInt64Ptr(in.ProgramID),
				Memo:      in.Memo,
				ID:        lineID,
			}); err != nil {
				return fmt.Errorf("update expense report line %d: %w", lineID, err)
			}
			return insertExpenseReportLineVersion(ctx, q, changeID, "update", lineID)
		})
	if err != nil {
		return fmt.Errorf("update expense report line %d: %w", lineID, err)
	}
	return nil
}

// RemoveExpenseReportLine HARD-deletes a line (allowed only while draft|rejected)
// under one change, appending an op='delete' version FIRST (snapshot-before-delete,
// rule 14).
func (s *Store) RemoveExpenseReportLine(ctx context.Context, lineID int64) error {
	_, err := s.write(ctx, "expense_report_line.delete", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			line, err := q.GetExpenseReportLine(ctx, lineID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrExpenseReportLineNotFound
				}
				return fmt.Errorf("load expense report line %d: %w", lineID, err)
			}
			if err := requireEditable(ctx, q, line.ReportID); err != nil {
				return err
			}
			// Version BEFORE the live delete (the row must still exist to snapshot).
			if err := insertExpenseReportLineVersion(ctx, q, changeID, "delete", lineID); err != nil {
				return err
			}
			if err := q.DeleteExpenseReportLine(ctx, lineID); err != nil {
				return fmt.Errorf("delete expense report line %d: %w", lineID, err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("remove expense report line %d: %w", lineID, err)
	}
	return nil
}

// SubmitExpenseReport moves a report draft|rejected -> submitted under one change.
// Requires >= 1 line (ErrExpenseReportEmpty). review_notes is preserved (a resubmit
// after a reject still shows the reviewer's reason). Versioned op='update'.
func (s *Store) SubmitExpenseReport(ctx context.Context, reportID int64) error {
	return s.transitionExpenseReport(ctx, "expense_report.submit", reportID,
		func(rep sqlc.ExpenseReport, q *sqlc.Queries) (string, error) {
			if rep.Status != "draft" && rep.Status != "rejected" {
				return "", ErrExpenseReportState
			}
			n, err := q.CountExpenseReportLines(ctx, reportID)
			if err != nil {
				return "", fmt.Errorf("count lines: %w", err)
			}
			if n == 0 {
				return "", ErrExpenseReportEmpty
			}
			return "submitted", nil
		})
}

// ResubmitExpenseReport moves a report rejected -> submitted (after the submitter
// edits lines) under one change. review_notes is preserved. Versioned op='update'.
// Distinct from Submit so the state precondition (rejected only) reads clearly and
// p20.2 can wire a resubmit action separate from a first submit.
func (s *Store) ResubmitExpenseReport(ctx context.Context, reportID int64) error {
	return s.transitionExpenseReport(ctx, "expense_report.resubmit", reportID,
		func(rep sqlc.ExpenseReport, q *sqlc.Queries) (string, error) {
			if rep.Status != "rejected" {
				return "", ErrExpenseReportState
			}
			n, err := q.CountExpenseReportLines(ctx, reportID)
			if err != nil {
				return "", fmt.Errorf("count lines: %w", err)
			}
			if n == 0 {
				return "", ErrExpenseReportEmpty
			}
			return "submitted", nil
		})
}

// RejectExpenseReport moves a report submitted -> rejected under one change, storing
// reason in review_notes (required, ErrExpenseReportReasonRequired). Versioned
// op='update'. The reviewer (p20.3) calls this; the reason routes back to the
// submitter.
func (s *Store) RejectExpenseReport(ctx context.Context, reportID int64, reason string) error {
	if reason == "" {
		return ErrExpenseReportReasonRequired
	}
	_, err := s.write(ctx, "expense_report.reject", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			rep, err := loadExpenseReport(ctx, q, reportID)
			if err != nil {
				return err
			}
			if rep.Status == "converted" {
				return ErrExpenseReportImmutable
			}
			if rep.Status != "submitted" {
				return ErrExpenseReportState
			}
			if err := q.SetExpenseReportStatus(ctx, sqlc.SetExpenseReportStatusParams{
				Status: "rejected", ReviewNotes: reason, ID: reportID,
			}); err != nil {
				return fmt.Errorf("set rejected: %w", err)
			}
			return insertExpenseReportVersion(ctx, q, changeID, "update", reportID)
		})
	if err != nil {
		return fmt.Errorf("reject expense report %d: %w", reportID, err)
	}
	return nil
}

// ConvertExpenseReport moves a report submitted -> converted under one change and
// LINKS the posted transaction (posted_transaction_id, set ONLY here). The txn is the
// real balanced ledger entry the reviewer creates in p20.3; this store method just
// validates it EXISTS (not that it balances / maps the report -- that is the
// reviewer's job) and flips the status. After convert the report is TERMINAL/immutable.
// Versioned op='update'.
func (s *Store) ConvertExpenseReport(ctx context.Context, reportID, postedTxnID int64) error {
	_, err := s.write(ctx, "expense_report.convert", "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			rep, err := loadExpenseReport(ctx, q, reportID)
			if err != nil {
				return err
			}
			if rep.Status == "converted" {
				return ErrExpenseReportState
			}
			if rep.Status != "submitted" {
				return ErrExpenseReportState
			}
			if _, err := q.GetTransaction(ctx, postedTxnID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrExpenseReportTxnMissing
				}
				return fmt.Errorf("load posted transaction %d: %w", postedTxnID, err)
			}
			if err := q.SetExpenseReportConverted(ctx, sqlc.SetExpenseReportConvertedParams{
				PostedTransactionID: sql.NullInt64{Int64: postedTxnID, Valid: true},
				ID:                  reportID,
			}); err != nil {
				return fmt.Errorf("set converted: %w", err)
			}
			return insertExpenseReportVersion(ctx, q, changeID, "update", reportID)
		})
	if err != nil {
		return fmt.Errorf("convert expense report %d: %w", reportID, err)
	}
	return nil
}

// GetExpenseReport returns a report's current live row (read; sqlc).
func (s *Store) GetExpenseReport(ctx context.Context, id int64) (sqlc.ExpenseReport, error) {
	row, err := s.q.GetExpenseReport(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.ExpenseReport{}, ErrExpenseReportNotFound
		}
		return sqlc.ExpenseReport{}, fmt.Errorf("store: get expense report %d: %w", id, err)
	}
	return row, nil
}

// ExpenseReportLines returns a report's lines, id-ordered (read; sqlc).
func (s *Store) ExpenseReportLines(ctx context.Context, reportID int64) ([]sqlc.ExpenseReportLine, error) {
	rows, err := s.q.ListExpenseReportLines(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("store: expense report %d lines: %w", reportID, err)
	}
	return rows, nil
}

// ExpenseReportsBySubmitter returns a submitter's own reports, newest first (read).
func (s *Store) ExpenseReportsBySubmitter(ctx context.Context, submitterID int64) ([]sqlc.ExpenseReport, error) {
	rows, err := s.q.ListExpenseReportsBySubmitter(ctx, submitterID)
	if err != nil {
		return nil, fmt.Errorf("store: expense reports by submitter %d: %w", submitterID, err)
	}
	return rows, nil
}

// ExpenseReportsByStatus returns reports in a status, id-ordered (read; the p20.3
// reviewer queue reads 'submitted').
func (s *Store) ExpenseReportsByStatus(ctx context.Context, status string) ([]sqlc.ExpenseReport, error) {
	rows, err := s.q.ListExpenseReportsByStatus(ctx, status)
	if err != nil {
		return nil, fmt.Errorf("store: expense reports by status %q: %w", status, err)
	}
	return rows, nil
}

// --- helpers (unexported) --------------------------------------------------

// transitionExpenseReport runs a status-only transition (submit/resubmit) through
// the funnel: it loads the report, applies decide (which returns the new status or a
// typed error), writes status (preserving review_notes), and versions op='update'.
func (s *Store) transitionExpenseReport(
	ctx context.Context, kind string, reportID int64,
	decide func(rep sqlc.ExpenseReport, q *sqlc.Queries) (string, error),
) error {
	_, err := s.write(ctx, kind, "",
		func(ctx context.Context, q *sqlc.Queries, changeID int64) error {
			rep, err := loadExpenseReport(ctx, q, reportID)
			if err != nil {
				return err
			}
			newStatus, err := decide(rep, q)
			if err != nil {
				return err
			}
			// review_notes is preserved across the transition (a resubmit still shows
			// the reviewer's earlier reason, p20.2).
			if err := q.SetExpenseReportStatus(ctx, sqlc.SetExpenseReportStatusParams{
				Status: newStatus, ReviewNotes: rep.ReviewNotes, ID: reportID,
			}); err != nil {
				return fmt.Errorf("set status %q: %w", newStatus, err)
			}
			return insertExpenseReportVersion(ctx, q, changeID, "update", reportID)
		})
	if err != nil {
		return fmt.Errorf("%s (report %d): %w", kind, reportID, err)
	}
	return nil
}

// loadExpenseReport reads a report on the tx-bound queries, mapping missing to a
// typed error.
func loadExpenseReport(ctx context.Context, q *sqlc.Queries, reportID int64) (sqlc.ExpenseReport, error) {
	rep, err := q.GetExpenseReport(ctx, reportID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.ExpenseReport{}, ErrExpenseReportNotFound
		}
		return sqlc.ExpenseReport{}, fmt.Errorf("load expense report %d: %w", reportID, err)
	}
	return rep, nil
}

// requireEditable loads a report and rejects a line mutation unless it is draft or
// rejected: a converted report is immutable (ErrExpenseReportImmutable); a submitted
// report is under review and its lines are frozen (ErrExpenseReportState).
func requireEditable(ctx context.Context, q *sqlc.Queries, reportID int64) error {
	rep, err := loadExpenseReport(ctx, q, reportID)
	if err != nil {
		return err
	}
	switch rep.Status {
	case "draft", "rejected":
		return nil
	case "converted":
		return ErrExpenseReportImmutable
	default: // submitted
		return ErrExpenseReportState
	}
}

// validateExpenseReportLine checks the line's account exists and its optional
// fund/program exist if set. Unlike a budget line, there is NO R/E restriction and NO
// amount-sign rule -- a submission need not balance and the reviewer resolves
// accounts/sign at convert (p20.3).
func validateExpenseReportLine(ctx context.Context, q *sqlc.Queries, in ExpenseReportLineInput) error {
	if _, err := q.GetAccount(ctx, in.AccountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrExpenseReportRefMissing
		}
		return fmt.Errorf("load line account %d: %w", in.AccountID, err)
	}
	if in.FundID != nil {
		if _, err := q.GetFund(ctx, *in.FundID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExpenseReportRefMissing
			}
			return fmt.Errorf("load line fund %d: %w", *in.FundID, err)
		}
	}
	if in.ProgramID != nil {
		if _, err := q.GetProgram(ctx, *in.ProgramID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExpenseReportRefMissing
			}
			return fmt.Errorf("load line program %d: %w", *in.ProgramID, err)
		}
	}
	return nil
}

// insertExpenseReportVersion appends the expense_reports snapshot-from-live version
// row (MUST run after the live write). Hides the ID/ID_2 positional-param names.
func insertExpenseReportVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertExpenseReportVersion(ctx, sqlc.InsertExpenseReportVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append expense report version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// insertExpenseReportLineVersion appends the expense_report_lines snapshot-from-live
// version row. For op='delete' it MUST run BEFORE the live delete.
func insertExpenseReportLineVersion(ctx context.Context, q *sqlc.Queries, changeID int64, op string, entityID int64) error {
	if err := q.InsertExpenseReportLineVersion(ctx, sqlc.InsertExpenseReportLineVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append expense report line version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

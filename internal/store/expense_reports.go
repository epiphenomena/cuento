package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cuento/internal/db/sqlc"
	"cuento/internal/ids"
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
// with a delete version (rule 14: soft-delete is transactions-only). Lines are edited
// either one at a time (Add/Update/RemoveExpenseReportLine) OR in a BULK REPLACE-SET
// (ReplaceExpenseReportLines, p25.4: the auto-row grid saves the whole line set under
// ONE change, diffing by line id like UpdateTransaction's split diff).
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
	// ErrExpenseReportHasLines: the subsidiary cannot be changed once the report has
	// lines (they are scoped to the sub; changing it would orphan their accounts).
	ErrExpenseReportHasLines = errors.New("store: expense report subsidiary is locked (has lines)")
)

// CreateExpenseReport creates a draft report for submitterID in subsidiaryID under
// ONE change and returns the new id (status=draft). Validates the submitter +
// subsidiary exist inside fn so a rejection rolls back cleanly. The subsidiary is the
// submitter's default at creation but is EDITABLE in-page until the first line is added
// (UpdateExpenseReportSubsidiary, p25.3) -- it is no longer fixed at creation.
func (s *Store) CreateExpenseReport(ctx context.Context, submitterID ids.UserID, subsidiaryID ids.SubsidiaryID) (ids.ExpenseReportID, error) {
	var newID ids.ExpenseReportID
	_, err := s.write(ctx, "expense_report.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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

// UpdateExpenseReportSubsidiary changes a report's subsidiary (p25.3) under one
// change. Allowed ONLY while the report is draft|rejected AND has NO lines: the sub
// scopes each line's account/fund options, so changing it after lines exist would
// orphan them (ErrExpenseReportHasLines). Validates the new subsidiary exists.
// Versioned op='update'.
func (s *Store) UpdateExpenseReportSubsidiary(ctx context.Context, reportID ids.ExpenseReportID, subsidiaryID ids.SubsidiaryID) error {
	_, err := s.write(ctx, "expense_report.set_subsidiary", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			rep, err := loadExpenseReport(ctx, q, reportID)
			if err != nil {
				return err
			}
			if rep.Status == "converted" {
				return ErrExpenseReportImmutable
			}
			if rep.Status != "draft" && rep.Status != "rejected" {
				return ErrExpenseReportState
			}
			n, err := q.CountExpenseReportLines(ctx, reportID)
			if err != nil {
				return fmt.Errorf("count lines: %w", err)
			}
			if n > 0 {
				return ErrExpenseReportHasLines
			}
			if _, err := q.GetSubsidiary(ctx, subsidiaryID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrExpenseReportRefMissing
				}
				return fmt.Errorf("load subsidiary %d: %w", subsidiaryID, err)
			}
			if err := q.SetExpenseReportSubsidiary(ctx, sqlc.SetExpenseReportSubsidiaryParams{
				SubsidiaryID: subsidiaryID, ID: reportID,
			}); err != nil {
				return fmt.Errorf("set subsidiary: %w", err)
			}
			return insertExpenseReportVersion(ctx, q, changeID, "update", reportID)
		})
	if err != nil {
		return fmt.Errorf("set expense report %d subsidiary: %w", reportID, err)
	}
	return nil
}

// DiscardExpenseReport HARD-deletes a DRAFT report and all its lines (p25.3) under one
// change. Draft-only (ErrExpenseReportState otherwise): a draft has no
// posted_transaction_id and nothing references it, so no FK is fought. Each line gets
// an op='delete' version BEFORE its live delete, then the report gets its own
// op='delete' version BEFORE the report row is deleted (rule 14: snapshot-before-delete).
func (s *Store) DiscardExpenseReport(ctx context.Context, reportID ids.ExpenseReportID) error {
	_, err := s.write(ctx, "expense_report.discard", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			rep, err := loadExpenseReport(ctx, q, reportID)
			if err != nil {
				return err
			}
			if rep.Status != "draft" {
				return ErrExpenseReportState
			}
			// Delete each line (version op='delete' captured before the live delete).
			lines, err := q.ListExpenseReportLines(ctx, reportID)
			if err != nil {
				return fmt.Errorf("list lines: %w", err)
			}
			for _, l := range lines {
				if err := insertExpenseReportLineVersion(ctx, q, changeID, "delete", l.ID); err != nil {
					return err
				}
				if err := q.DeleteExpenseReportLine(ctx, l.ID); err != nil {
					return fmt.Errorf("delete line %d: %w", l.ID, err)
				}
			}
			// Then the report itself (version before the live delete).
			if err := insertExpenseReportVersion(ctx, q, changeID, "delete", reportID); err != nil {
				return err
			}
			if err := q.DeleteExpenseReport(ctx, reportID); err != nil {
				return fmt.Errorf("delete report: %w", err)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("discard expense report %d: %w", reportID, err)
	}
	return nil
}

// ExpenseReportLineInput is the desired state of one proposed split: an account, a
// SIGNED minor-unit amount (the report need not balance), and optional fund/program
// (nil = the reviewer resolves at convert) + a free-text memo.
type ExpenseReportLineInput struct {
	AccountID   int64
	Amount      int64
	FundID      *ids.FundID
	ProgramID   *ids.ProgramID
	Memo        string
	Description string // per-line free-text (p26.15; payee->description migration, INERT this step)
}

// AddExpenseReportLine adds a line to a report (allowed only while draft|rejected)
// under one change and returns the new id. Validates the account (+ fund/program if
// set) exist inside fn.
func (s *Store) AddExpenseReportLine(ctx context.Context, reportID ids.ExpenseReportID, in ExpenseReportLineInput) (ids.ExpenseReportLineID, error) {
	var newID ids.ExpenseReportLineID
	_, err := s.write(ctx, "expense_report_line.create", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			if err := requireEditable(ctx, q, reportID); err != nil {
				return err
			}
			if err := validateExpenseReportLine(ctx, q, in); err != nil {
				return err
			}
			id, err := q.InsertExpenseReportLine(ctx, sqlc.InsertExpenseReportLineParams{
				ReportID:    reportID,
				AccountID:   in.AccountID,
				Amount:      in.Amount,
				FundID:      ids.Null(in.FundID),
				ProgramID:   ids.Null(in.ProgramID),
				Memo:        in.Memo,
				Description: in.Description,
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
func (s *Store) UpdateExpenseReportLine(ctx context.Context, lineID ids.ExpenseReportLineID, in ExpenseReportLineInput) error {
	_, err := s.write(ctx, "expense_report_line.update", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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
				AccountID:   in.AccountID,
				Amount:      in.Amount,
				FundID:      ids.Null(in.FundID),
				ProgramID:   ids.Null(in.ProgramID),
				Memo:        in.Memo,
				Description: in.Description,
				ID:          lineID,
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
func (s *Store) RemoveExpenseReportLine(ctx context.Context, lineID ids.ExpenseReportLineID) error {
	_, err := s.write(ctx, "expense_report_line.delete", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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

// ExpenseReportLineDesired is one line in a bulk replace-set save: ID 0 = a new line
// to insert; ID > 0 = an existing line of this report to update. Lines of the report
// NOT present in the desired set are deleted.
type ExpenseReportLineDesired struct {
	ID ids.ExpenseReportLineID
	ExpenseReportLineInput
}

// ReplaceExpenseReportLines saves a report's whole line set in ONE change (p25.4: the
// auto-row grid's bulk submit), diffing by line id -- the MIRROR of UpdateTransaction's
// split replace-set. Allowed only while draft|rejected (requireEditable). A desired
// line with ID>0 must belong to THIS report (else ErrExpenseReportLineNotFound) and is
// UPDATEd (version 'update'); ID==0 is INSERTed (version 'create'); an existing line
// absent from the desired set is DELETEd (version 'delete' BEFORE the live delete,
// rule 14). Each desired line is validated (validateExpenseReportLine) inside fn so a
// rejection rolls the whole change back and leaves no audit trace.
func (s *Store) ReplaceExpenseReportLines(ctx context.Context, reportID ids.ExpenseReportID, desired []ExpenseReportLineDesired) error {
	_, err := s.write(ctx, "expense_report.replace_lines", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			if err := requireEditable(ctx, q, reportID); err != nil {
				return err
			}
			existing, err := q.ListExpenseReportLines(ctx, reportID)
			if err != nil {
				return fmt.Errorf("list lines: %w", err)
			}
			existingIDs := make(map[ids.ExpenseReportLineID]bool, len(existing))
			for _, l := range existing {
				existingIDs[l.ID] = true
			}
			kept := make(map[ids.ExpenseReportLineID]bool, len(desired))
			for _, d := range desired {
				if err := validateExpenseReportLine(ctx, q, d.ExpenseReportLineInput); err != nil {
					return err
				}
				if d.ID > 0 {
					if !existingIDs[d.ID] {
						return ErrExpenseReportLineNotFound
					}
					kept[d.ID] = true
					if err := q.UpdateExpenseReportLine(ctx, sqlc.UpdateExpenseReportLineParams{
						AccountID:   d.AccountID,
						Amount:      d.Amount,
						FundID:      ids.Null(d.FundID),
						ProgramID:   ids.Null(d.ProgramID),
						Memo:        d.Memo,
						Description: d.Description,
						ID:          d.ID,
					}); err != nil {
						return fmt.Errorf("update expense report line %d: %w", d.ID, err)
					}
					if err := insertExpenseReportLineVersion(ctx, q, changeID, "update", d.ID); err != nil {
						return err
					}
					continue
				}
				id, err := q.InsertExpenseReportLine(ctx, sqlc.InsertExpenseReportLineParams{
					ReportID:    reportID,
					AccountID:   d.AccountID,
					Amount:      d.Amount,
					FundID:      ids.Null(d.FundID),
					ProgramID:   ids.Null(d.ProgramID),
					Memo:        d.Memo,
					Description: d.Description,
				})
				if err != nil {
					return fmt.Errorf("insert expense report line: %w", err)
				}
				if err := insertExpenseReportLineVersion(ctx, q, changeID, "create", id); err != nil {
					return err
				}
			}
			// Delete existing lines not present in the desired set (version BEFORE the
			// live delete: the row must still exist to snapshot, rule 14).
			for _, l := range existing {
				if kept[l.ID] {
					continue
				}
				if err := insertExpenseReportLineVersion(ctx, q, changeID, "delete", l.ID); err != nil {
					return err
				}
				if err := q.DeleteExpenseReportLine(ctx, l.ID); err != nil {
					return fmt.Errorf("delete expense report line %d: %w", l.ID, err)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("replace expense report %d lines: %w", reportID, err)
	}
	return nil
}

// SubmitExpenseReport moves a report draft|rejected -> submitted under one change.
// Requires >= 1 line (ErrExpenseReportEmpty). review_notes is preserved (a resubmit
// after a reject still shows the reviewer's reason). Versioned op='update'.
func (s *Store) SubmitExpenseReport(ctx context.Context, reportID ids.ExpenseReportID) error {
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
func (s *Store) ResubmitExpenseReport(ctx context.Context, reportID ids.ExpenseReportID) error {
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
func (s *Store) RejectExpenseReport(ctx context.Context, reportID ids.ExpenseReportID, reason string) error {
	if reason == "" {
		return ErrExpenseReportReasonRequired
	}
	_, err := s.write(ctx, "expense_report.reject", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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
func (s *Store) ConvertExpenseReport(ctx context.Context, reportID ids.ExpenseReportID, postedTxnID ids.TransactionID) error {
	_, err := s.write(ctx, "expense_report.convert", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
			rep, err := loadExpenseReport(ctx, q, reportID)
			if err != nil {
				return err
			}
			// Only a SUBMITTED report can convert; any other status (draft, rejected,
			// or already-converted) is rejected by the single guard below.
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
				PostedTransactionID: ids.Null(&postedTxnID),
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

// PostAndConvertExpenseReport posts a balanced ledger transaction from the reviewer's
// (possibly adjusted) editor splits AND converts the report to that txn -- IN ONE
// change (atomic, p20.3). This is the MIRROR of PostImportRow: the report is re-read on
// the tx-bound q and must still be 'submitted' (ErrExpenseReportState) -- this re-read,
// not atomicity alone, is what stops a double-review double-post and blocks a converted
// (terminal) report from being re-posted. postTransactionTx is the SOLE validator: an
// unbalanced/invalid post rolls the whole write back, so the report stays submitted for
// free (no window). On success the report is CONVERTED (terminal/immutable) and its
// posted_transaction_id points at the just-created real, versioned txn. Returns the
// created transaction id.
//
// Distinct from ConvertExpenseReport (which links an ALREADY-existing txn, p20.1): here
// the txn is created in the same funnel call, so a converted report can never point at a
// missing txn and vice versa.
func (s *Store) PostAndConvertExpenseReport(ctx context.Context, reportID ids.ExpenseReportID, in PostTransactionInput) (ids.TransactionID, error) {
	var txnID ids.TransactionID
	_, err := s.write(ctx, "expense_report.post_convert", "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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
			id, err := s.postTransactionTx(ctx, q, changeID, in)
			if err != nil {
				return err
			}
			txnID = id
			if err := q.SetExpenseReportConverted(ctx, sqlc.SetExpenseReportConvertedParams{
				PostedTransactionID: ids.Null(&id),
				ID:                  reportID,
			}); err != nil {
				return fmt.Errorf("set converted: %w", err)
			}
			return insertExpenseReportVersion(ctx, q, changeID, "update", reportID)
		})
	if err != nil {
		return 0, fmt.Errorf("post and convert expense report %d: %w", reportID, err)
	}
	return txnID, nil
}

// GetExpenseReport returns a report's current live row (read; sqlc).
func (s *Store) GetExpenseReport(ctx context.Context, id ids.ExpenseReportID) (sqlc.ExpenseReport, error) {
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
func (s *Store) ExpenseReportLines(ctx context.Context, reportID ids.ExpenseReportID) ([]sqlc.ExpenseReportLine, error) {
	rows, err := s.q.ListExpenseReportLines(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("store: expense report %d lines: %w", reportID, err)
	}
	return rows, nil
}

// ExpenseReportsBySubmitter returns a submitter's own reports, newest first (read).
func (s *Store) ExpenseReportsBySubmitter(ctx context.Context, submitterID ids.UserID) ([]sqlc.ExpenseReport, error) {
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
	ctx context.Context, kind string, reportID ids.ExpenseReportID,
	decide func(rep sqlc.ExpenseReport, q *sqlc.Queries) (string, error),
) error {
	_, err := s.write(ctx, kind, "",
		func(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID) error {
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
func loadExpenseReport(ctx context.Context, q *sqlc.Queries, reportID ids.ExpenseReportID) (sqlc.ExpenseReport, error) {
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
func requireEditable(ctx context.Context, q *sqlc.Queries, reportID ids.ExpenseReportID) error {
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
func insertExpenseReportVersion(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, op string, entityID ids.ExpenseReportID) error {
	if err := q.InsertExpenseReportVersion(ctx, sqlc.InsertExpenseReportVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append expense report version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

// insertExpenseReportLineVersion appends the expense_report_lines snapshot-from-live
// version row. For op='delete' it MUST run BEFORE the live delete.
func insertExpenseReportLineVersion(ctx context.Context, q *sqlc.Queries, changeID ids.ChangeID, op string, entityID ids.ExpenseReportLineID) error {
	if err := q.InsertExpenseReportLineVersion(ctx, sqlc.InsertExpenseReportLineVersionParams{Op: op, ID: changeID, ID_2: entityID}); err != nil {
		return fmt.Errorf("append expense report line version (entity %d, op %s): %w", entityID, op, err)
	}
	return nil
}

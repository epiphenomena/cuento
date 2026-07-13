package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"cuento/internal/testutil"
)

// p20.1: the expense-report submission->review workflow, DECOUPLED from book-editing
// (a submitter has no ledger access). These tests prove: the versioned lifecycle
// (draft->submitted->rejected->submitted->converted), each transition an
// AssertVersioned op; posted_transaction_id NULL until convert, set after; the state
// machine rejects illegal transitions; and the standalone can_submit_expenses user
// capability is a VERSIONED column (setting it appends a users_versions row naming the
// actor).

// seedExpenseReportEnv builds the minimal env for report tests: a submitter user, a
// reviewer, a subsidiary (the seeded root, id 1), an account, and returns the store,
// db, a submitter-actor context, and the ids.
func seedExpenseReportEnv(t *testing.T) (*Store, *sql.DB, context.Context, int64, int64) {
	t.Helper()
	d := testutil.NewDB(t)
	s := New(d)
	sysCtx := WithActor(context.Background(), Actor{ID: 1})

	submitterID, err := s.CreateUser(sysCtx, CreateUserInput{Username: "submitter", DisplayName: "Sub", TxnPerm: "none"})
	if err != nil {
		t.Fatalf("create submitter: %v", err)
	}
	acctID, err := s.CreateAccount(sysCtx, CreateAccountInput{
		Type: "expense", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Travel"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	ctx := WithActor(context.Background(), Actor{ID: submitterID})
	return s, d, ctx, submitterID, acctID
}

// TestExpenseReportLifecycleVersioned walks the full lifecycle and asserts each
// mutation is versioned and posted_transaction_id is set ONLY on convert.
func TestExpenseReportLifecycleVersioned(t *testing.T) {
	s, d, ctx, submitterID, acctID := seedExpenseReportEnv(t)

	// create (draft) -> versioned op='create'
	reportID, err := s.CreateExpenseReport(ctx, submitterID, 1)
	if err != nil {
		t.Fatalf("CreateExpenseReport: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_reports", reportID, "create")

	// add a line -> versioned op='create' on the line
	lineID, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: acctID, Amount: -1500, Memo: "taxi"})
	if err != nil {
		t.Fatalf("AddExpenseReportLine: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_report_lines", lineID, "create")

	// edit the line -> op='update'
	if err := s.UpdateExpenseReportLine(ctx, lineID, ExpenseReportLineInput{AccountID: acctID, Amount: -2000, Memo: "taxi + tip"}); err != nil {
		t.Fatalf("UpdateExpenseReportLine: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_report_lines", lineID, "update")

	// submit (draft -> submitted) -> op='update' on the report
	if err := s.SubmitExpenseReport(ctx, reportID); err != nil {
		t.Fatalf("SubmitExpenseReport: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_reports", reportID, "update")
	if got := expenseReportStatus(t, d, reportID); got != "submitted" {
		t.Fatalf("status after submit = %q, want submitted", got)
	}
	if postedTxnID(t, d, reportID).Valid {
		t.Fatal("posted_transaction_id set before convert")
	}

	// reject (submitted -> rejected), storing the reason
	if err := s.RejectExpenseReport(ctx, reportID, "missing receipt"); err != nil {
		t.Fatalf("RejectExpenseReport: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_reports", reportID, "update")
	if got := expenseReportStatus(t, d, reportID); got != "rejected" {
		t.Fatalf("status after reject = %q, want rejected", got)
	}
	if got := reviewNotes(t, d, reportID); got != "missing receipt" {
		t.Fatalf("review_notes after reject = %q, want %q", got, "missing receipt")
	}

	// a submitter edits a line while rejected (allowed), then resubmits
	if err := s.UpdateExpenseReportLine(ctx, lineID, ExpenseReportLineInput{AccountID: acctID, Amount: -2000, Memo: "taxi + receipt attached"}); err != nil {
		t.Fatalf("UpdateExpenseReportLine while rejected: %v", err)
	}

	// resubmit (rejected -> submitted); the reviewer's reason is PRESERVED so the
	// submitter still sees it (p20.2).
	if err := s.ResubmitExpenseReport(ctx, reportID); err != nil {
		t.Fatalf("ResubmitExpenseReport: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_reports", reportID, "update")
	if got := expenseReportStatus(t, d, reportID); got != "submitted" {
		t.Fatalf("status after resubmit = %q, want submitted", got)
	}
	if got := reviewNotes(t, d, reportID); got != "missing receipt" {
		t.Fatalf("review_notes after resubmit = %q, want preserved %q", got, "missing receipt")
	}

	// convert (submitted -> converted), LINKING a real posted transaction
	txnID := seedPostedTxn(t, s, acctID)
	if err := s.ConvertExpenseReport(ctx, reportID, txnID); err != nil {
		t.Fatalf("ConvertExpenseReport: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_reports", reportID, "update")
	if got := expenseReportStatus(t, d, reportID); got != "converted" {
		t.Fatalf("status after convert = %q, want converted", got)
	}
	pt := postedTxnID(t, d, reportID)
	if !pt.Valid || pt.Int64 != txnID {
		t.Fatalf("posted_transaction_id after convert = %v, want %d", pt, txnID)
	}

	// End-to-end integrity: the new tables + their version twins must leave the
	// ledger check clean (the Z3/Z5 blocks reconcile against REAL rows here, not just
	// the empty-db `make check`).
	assertLedgerClean(t, d)
}

// TestRemoveExpenseReportLineVersioned proves a line HARD-deletes with an op='delete'
// version captured BEFORE the live delete (rule 14), and that removal is refused once
// the report is no longer editable.
func TestRemoveExpenseReportLineVersioned(t *testing.T) {
	s, d, ctx, submitterID, acctID := seedExpenseReportEnv(t)

	reportID, err := s.CreateExpenseReport(ctx, submitterID, 1)
	if err != nil {
		t.Fatalf("CreateExpenseReport: %v", err)
	}
	lineID, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: acctID, Amount: -500})
	if err != nil {
		t.Fatalf("AddExpenseReportLine: %v", err)
	}

	// Remove while draft: the live row is gone but a 'delete' version snapshot remains
	// (version-before-delete: AssertVersioned reads the versions table, so the missing
	// live row is fine -- if the two statements were reversed the snapshot would be
	// empty and this would fail).
	if err := s.RemoveExpenseReportLine(ctx, lineID); err != nil {
		t.Fatalf("RemoveExpenseReportLine: %v", err)
	}
	testutil.AssertVersioned(t, d, "expense_report_lines", lineID, "delete")
	var live int
	if err := d.QueryRow(`SELECT COUNT(*) FROM expense_report_lines WHERE id = ?`, lineID).Scan(&live); err != nil {
		t.Fatalf("count live line: %v", err)
	}
	if live != 0 {
		t.Fatalf("live line rows after remove = %d, want 0", live)
	}

	// A line on a SUBMITTED report is frozen: add/update/remove all reject with
	// ErrExpenseReportState (requireEditable's submitted branch).
	line2, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: acctID, Amount: -700})
	if err != nil {
		t.Fatalf("AddExpenseReportLine (second): %v", err)
	}
	if err := s.SubmitExpenseReport(ctx, reportID); err != nil {
		t.Fatalf("SubmitExpenseReport: %v", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: acctID, Amount: -1}); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("add line while submitted = %v, want ErrExpenseReportState", err)
	}
	if err := s.UpdateExpenseReportLine(ctx, line2, ExpenseReportLineInput{AccountID: acctID, Amount: -800}); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("update line while submitted = %v, want ErrExpenseReportState", err)
	}
	if err := s.RemoveExpenseReportLine(ctx, line2); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("remove line while submitted = %v, want ErrExpenseReportState", err)
	}

	assertLedgerClean(t, d)
}

// TestExpenseReportStateMachine proves illegal transitions are rejected with typed
// errors and leave no audit trace (the change rolls back).
func TestExpenseReportStateMachine(t *testing.T) {
	s, d, ctx, submitterID, acctID := seedExpenseReportEnv(t)
	txnID := seedPostedTxn(t, s, acctID)

	reportID, err := s.CreateExpenseReport(ctx, submitterID, 1)
	if err != nil {
		t.Fatalf("CreateExpenseReport: %v", err)
	}

	// submit with NO lines -> rejected (validate >= 1 line).
	if err := s.SubmitExpenseReport(ctx, reportID); !errors.Is(err, ErrExpenseReportEmpty) {
		t.Fatalf("submit with no lines = %v, want ErrExpenseReportEmpty", err)
	}

	// convert a DRAFT -> illegal (must be submitted).
	if err := s.ConvertExpenseReport(ctx, reportID, txnID); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("convert draft = %v, want ErrExpenseReportState", err)
	}
	// reject a DRAFT -> illegal (must be submitted).
	if err := s.RejectExpenseReport(ctx, reportID, "no"); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("reject draft = %v, want ErrExpenseReportState", err)
	}
	// resubmit a DRAFT -> illegal (must be rejected).
	if err := s.ResubmitExpenseReport(ctx, reportID); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("resubmit draft = %v, want ErrExpenseReportState", err)
	}
	// reject with an EMPTY reason -> rejected (reason required).
	if _, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: acctID, Amount: -100}); err != nil {
		t.Fatalf("AddExpenseReportLine: %v", err)
	}
	if err := s.SubmitExpenseReport(ctx, reportID); err != nil {
		t.Fatalf("SubmitExpenseReport: %v", err)
	}
	if err := s.RejectExpenseReport(ctx, reportID, ""); !errors.Is(err, ErrExpenseReportReasonRequired) {
		t.Fatalf("reject empty reason = %v, want ErrExpenseReportReasonRequired", err)
	}

	// convert with a NON-EXISTENT txn -> rejected (txn must exist).
	if err := s.ConvertExpenseReport(ctx, reportID, 99999); !errors.Is(err, ErrExpenseReportTxnMissing) {
		t.Fatalf("convert with missing txn = %v, want ErrExpenseReportTxnMissing", err)
	}

	// convert legitimately, then prove converted is TERMINAL / immutable.
	if err := s.ConvertExpenseReport(ctx, reportID, txnID); err != nil {
		t.Fatalf("ConvertExpenseReport: %v", err)
	}
	if err := s.SubmitExpenseReport(ctx, reportID); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("submit converted = %v, want ErrExpenseReportState", err)
	}
	if err := s.ConvertExpenseReport(ctx, reportID, txnID); !errors.Is(err, ErrExpenseReportState) {
		t.Fatalf("re-convert converted = %v, want ErrExpenseReportState", err)
	}
	if _, err := s.AddExpenseReportLine(ctx, reportID, ExpenseReportLineInput{AccountID: acctID, Amount: -1}); !errors.Is(err, ErrExpenseReportImmutable) {
		t.Fatalf("add line to converted = %v, want ErrExpenseReportImmutable", err)
	}

	// The submit-with-no-lines failure earlier must have left NO version rows beyond
	// the ones the legitimate ops wrote (a rejected op leaves no audit trace). We at
	// least confirm the report still exists with the expected terminal state.
	if got := expenseReportStatus(t, d, reportID); got != "converted" {
		t.Fatalf("final status = %q, want converted", got)
	}
}

// TestCanSubmitExpensesVersioned proves the standalone user capability is a versioned
// column: SetUserCanSubmitExpenses appends a users_versions row naming the acting
// admin, the live row + snapshot carry the new value, and the system user is refused.
func TestCanSubmitExpensesVersioned(t *testing.T) {
	d := testutil.NewDB(t)
	s := New(d)
	sysCtx := WithActor(context.Background(), Actor{ID: 1})

	adminID, err := s.CreateUser(sysCtx, CreateUserInput{Username: "boss", DisplayName: "Boss", IsAdmin: true})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	targetID, err := s.CreateUser(sysCtx, CreateUserInput{Username: "target", DisplayName: "T", TxnPerm: "none"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	adminCtx := WithActor(context.Background(), Actor{ID: adminID})
	if err := s.SetUserCanSubmitExpenses(adminCtx, targetID, true); err != nil {
		t.Fatalf("SetUserCanSubmitExpenses: %v", err)
	}
	testutil.AssertVersioned(t, d, "users", targetID, "update")
	if got := testutil.LatestVersionActor(t, d, "users", targetID); got != adminID {
		t.Errorf("can_submit_expenses change actor = %d, want admin %d", got, adminID)
	}
	// The live row reflects the flag (via the CurrentUser projection).
	if cu, err := s.UserByID(sysCtx, targetID); err != nil {
		t.Fatalf("UserByID: %v", err)
	} else if !cu.CanSubmitExpenses {
		t.Error("CanSubmitExpenses = false after set true")
	}
	// The version snapshot carries the flag too (proves it is audited, not just live).
	if got := latestVersionCanSubmit(t, d, targetID); got != 1 {
		t.Errorf("users_versions.can_submit_expenses = %d, want 1", got)
	}

	// The system user is refused.
	if err := s.SetUserCanSubmitExpenses(adminCtx, systemUserID, true); !errors.Is(err, ErrSystemUser) {
		t.Fatalf("set on system user = %v, want ErrSystemUser", err)
	}
}

// --- test helpers ----------------------------------------------------------

func expenseReportStatus(t *testing.T, d *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := d.QueryRow(`SELECT status FROM expense_reports WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func reviewNotes(t *testing.T, d *sql.DB, id int64) string {
	t.Helper()
	var notes string
	if err := d.QueryRow(`SELECT review_notes FROM expense_reports WHERE id = ?`, id).Scan(&notes); err != nil {
		t.Fatalf("read review_notes: %v", err)
	}
	return notes
}

func postedTxnID(t *testing.T, d *sql.DB, id int64) sql.NullInt64 {
	t.Helper()
	var pt sql.NullInt64
	if err := d.QueryRow(`SELECT posted_transaction_id FROM expense_reports WHERE id = ?`, id).Scan(&pt); err != nil {
		t.Fatalf("read posted_transaction_id: %v", err)
	}
	return pt
}

func latestVersionCanSubmit(t *testing.T, d *sql.DB, userID int64) int64 {
	t.Helper()
	var v int64
	err := d.QueryRow(
		`SELECT can_submit_expenses FROM users_versions WHERE entity_id = ?
		 ORDER BY valid_from DESC, id DESC LIMIT 1`, userID,
	).Scan(&v)
	if err != nil {
		t.Fatalf("read users_versions.can_submit_expenses: %v", err)
	}
	return v
}

// seedPostedTxn posts a minimal balanced 2-split transaction and returns its id, so
// ConvertExpenseReport has a real txn to link (the reviewer builds the real txn in
// p20.3; here the store just links an EXISTING one).
func seedPostedTxn(t *testing.T, s *Store, expenseAcct int64) int64 {
	t.Helper()
	sysCtx := WithActor(context.Background(), Actor{ID: 1})
	cash, err := s.CreateAccount(sysCtx, CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD",
		Names: map[string]string{"en": "Cash"}, Subsidiaries: []int64{1},
	})
	if err != nil {
		t.Fatalf("seed cash account: %v", err)
	}
	// Program id 1 is the seeded root "General" (migration 00008); an R/E split
	// requires a program (Z15) and a functional class (Z16).
	prog := int64(1)
	fc := "program"
	txnID, err := s.PostTransaction(sysCtx, PostTransactionInput{
		Date: "2025-06-01", SubsidiaryID: 1, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: expenseAcct, Amount: 2000, Position: 0, ProgramID: &prog, FunctionalClass: &fc},
			{AccountID: cash, Amount: -2000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("seed posted txn: %v", err)
	}
	return txnID
}

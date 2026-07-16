package store

import (
	"context"
	"errors"
	"testing"

	"cuento/internal/bankimport"
	"cuento/internal/testutil"
)

// p17.3 review-queue store tests: posting a staged row creates a BALANCED, versioned
// ledger transaction and LINKS the row (posted_transaction_id + status=posted);
// discard requires a reason and writes a change; a re-upload flags the already-posted
// row as a duplicate (idempotent -- no double post).

// stageOnePending stages a single pending row on env.checking and returns its id.
func stageOnePending(t *testing.T, env importEnv, date string, amount int64, description, memo string) int64 {
	t.Helper()
	batch, err := env.s.CreateImportBatch(mutCtx(), "queue.csv", env.checking, env.subUS, env.profile, "2025-02-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CreateImportBatch: %v", err)
	}
	staged, err := env.s.StageImportRows(mutCtx(), batch, env.checking, []bankimport.ParsedRow{
		{Date: date, AmountMinor: amount, Description: description, Memo: memo, Raw: []string{date, description, memo}},
	})
	if err != nil {
		t.Fatalf("StageImportRows: %v", err)
	}
	return staged[0].ID
}

// TestPostImportRowCreatesBalancedTxnAndLinks: posting a staged row creates a
// balanced transaction (batch account on one side, a payee-template-style counter
// split carrying fund + functional class on the other), sets posted_transaction_id +
// status=posted, and the created txn is a real versioned ledger entry that appears in
// the account register.
func TestPostImportRowCreatesBalancedTxnAndLinks(t *testing.T) {
	env := newImportEnv(t)
	s := env.s

	// A restricted fund scoped to subUS, and an expense account (so the counter split
	// carries a fund + functional class -- the p12.3 template fields).
	fund, err := s.CreateFund(mutCtx(), CreateFundInput{
		Name: "Grant", Restriction: "purpose", Subsidiaries: []int64{env.subUS},
	})
	if err != nil {
		t.Fatalf("CreateFund: %v", err)
	}
	expense := mkAcctFull(t, s, "expense", "Supplies", []int64{env.subUS}, "program", rootProgramID)

	rowID := stageOnePending(t, env, "2025-01-15", 10000, "Acme", "Invoice")

	// The bank side (checking) carries the SAME fund as the counter split so per-fund
	// zero-sum holds (D20: a restricted counter forces the cash side into the fund).
	txnID, err := s.PostImportRow(mutCtx(), rowID, PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, FundID: &fund, Memo: "Invoice", Position: 0},
			{AccountID: expense, Amount: -10000, FundID: &fund, FunctionalClass: strptr("program"), Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostImportRow: %v", err)
	}
	if txnID == 0 {
		t.Fatal("PostImportRow returned txn id 0")
	}

	// The txn is a real versioned ledger entry.
	testutil.AssertVersioned(t, s.db, "transactions", txnID, "create")

	// The row is LINKED: status=posted, posted_transaction_id = txnID.
	row, err := s.GetImportRow(context.Background(), rowID)
	if err != nil {
		t.Fatalf("GetImportRow: %v", err)
	}
	if row.Status != "posted" {
		t.Errorf("row status = %q, want posted", row.Status)
	}
	if row.PostedTxnID == nil || *row.PostedTxnID != txnID {
		t.Errorf("row PostedTxnID = %v, want %d", row.PostedTxnID, txnID)
	}

	// The counter split carries fund + functional class (the p12.3 template fields).
	splits, err := s.TransactionSplits(context.Background(), txnID)
	if err != nil {
		t.Fatalf("TransactionSplits: %v", err)
	}
	foundClass := false
	for _, sp := range splits {
		if sp.AccountID == expense {
			if !sp.FundID.Valid || sp.FundID.Int64 != fund {
				t.Errorf("counter split fund = %v, want %d", sp.FundID, fund)
			}
			if !sp.FunctionalClass.Valid || sp.FunctionalClass.String != "program" {
				t.Errorf("counter split class = %v, want program", sp.FunctionalClass)
			}
			foundClass = true
		}
	}
	if !foundClass {
		t.Error("counter split (expense) not found in posted txn")
	}

	// The created txn appears in the checking-account register (a real ledger entry).
	regRows, _, _, err := s.RegisterPage(context.Background(), env.checking, RegisterCursor{}, RegisterFilters{}, 50)
	if err != nil {
		t.Fatalf("RegisterPage: %v", err)
	}
	seen := false
	for _, e := range regRows {
		if e.TxnID == txnID {
			seen = true
		}
	}
	if !seen {
		t.Error("posted transaction not in the checking-account register")
	}

	// Re-posting the (now non-pending) row is rejected (no double post).
	if _, err := s.PostImportRow(mutCtx(), rowID, PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, Memo: "Invoice", Position: 0},
			{AccountID: env.equity, Amount: -10000, Position: 1},
		},
	}); !errors.Is(err, ErrImportRowNotPending) {
		t.Fatalf("re-post err = %v, want ErrImportRowNotPending", err)
	}
}

// TestPostImportRowUnbalancedRejected: an unbalanced post is rejected (the store is
// the sole validator) and leaves the row pending (nothing linked).
func TestPostImportRowUnbalancedRejected(t *testing.T) {
	env := newImportEnv(t)
	s := env.s
	rowID := stageOnePending(t, env, "2025-01-15", 10000, "Acme", "Invoice")

	_, err := s.PostImportRow(mutCtx(), rowID, PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, Position: 0},
			{AccountID: env.equity, Amount: -9000, Position: 1}, // does not net to zero
		},
	})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("PostImportRow err = %v, want ErrUnbalanced", err)
	}
	row, err := s.GetImportRow(context.Background(), rowID)
	if err != nil {
		t.Fatalf("GetImportRow: %v", err)
	}
	if row.Status != "pending" || row.PostedTxnID != nil {
		t.Errorf("after rejected post: status=%q posted=%v, want pending/nil (rolled back)", row.Status, row.PostedTxnID)
	}
}

// TestDiscardRequiresReason: discard without a reason is rejected and writes NOTHING;
// with a reason the row is discarded and a change row records the reason.
func TestDiscardRequiresReason(t *testing.T) {
	env := newImportEnv(t)
	s := env.s
	rowID := stageOnePending(t, env, "2025-01-15", 10000, "Acme", "Invoice")

	before := countChanges(t, s.db)

	// Empty reason -> rejected, nothing written.
	if err := s.DiscardImportRow(mutCtx(), rowID, "   "); !errors.Is(err, ErrDiscardReasonRequired) {
		t.Fatalf("discard(empty) err = %v, want ErrDiscardReasonRequired", err)
	}
	if got := countChanges(t, s.db); got != before {
		t.Fatalf("empty-reason discard wrote %d change(s); want 0", got-before)
	}
	row, _ := s.GetImportRow(context.Background(), rowID)
	if row.Status != "pending" {
		t.Fatalf("after rejected discard row status = %q, want pending", row.Status)
	}

	// With a reason -> discarded + one change whose note is the reason.
	if err := s.DiscardImportRow(mutCtx(), rowID, "not our account"); err != nil {
		t.Fatalf("DiscardImportRow: %v", err)
	}
	row, _ = s.GetImportRow(context.Background(), rowID)
	if row.Status != "discarded" {
		t.Errorf("row status = %q, want discarded", row.Status)
	}
	var note string
	if err := s.db.QueryRow(
		`SELECT note FROM changes WHERE kind = 'import.row.discard' ORDER BY id DESC LIMIT 1`,
	).Scan(&note); err != nil {
		t.Fatalf("read discard change: %v", err)
	}
	if note != "not our account" {
		t.Errorf("discard change note = %q, want %q", note, "not our account")
	}

	// A re-discard of a non-pending row is rejected.
	if err := s.DiscardImportRow(mutCtx(), rowID, "again"); !errors.Is(err, ErrImportRowNotPending) {
		t.Fatalf("re-discard err = %v, want ErrImportRowNotPending", err)
	}
}

// TestReimportFlagsDuplicates: after posting a staged row, re-uploading the same line
// (a new batch) flags it a duplicate -- idempotent, no double post. The dedupe lookup
// spans batches AND includes posted rows.
func TestReimportFlagsDuplicates(t *testing.T) {
	env := newImportEnv(t)
	s := env.s

	// Stage + post a row.
	rowID := stageOnePending(t, env, "2025-01-15", 10000, "Acme", "Invoice")
	posted, err := s.GetImportRow(context.Background(), rowID)
	if err != nil {
		t.Fatalf("GetImportRow: %v", err)
	}
	_, err = s.PostImportRow(mutCtx(), rowID, PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, Memo: "Invoice", Position: 0},
			{AccountID: env.equity, Amount: -10000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostImportRow: %v", err)
	}

	// Re-upload the identical line in a NEW batch: it stages but is FLAGGED duplicate
	// (matches the already-posted import row) -- no double post.
	batch2, err := s.CreateImportBatch(mutCtx(), "reupload.csv", env.checking, env.subUS, env.profile, "2025-03-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CreateImportBatch 2: %v", err)
	}
	staged, err := s.StageImportRows(mutCtx(), batch2, env.checking, []bankimport.ParsedRow{
		{Date: "2025-01-15", AmountMinor: 10000, Description: "Acme", Memo: "Invoice", Raw: []string{"2025-01-15", "Acme", "Invoice"}},
	})
	if err != nil {
		t.Fatalf("StageImportRows 2: %v", err)
	}
	if len(staged) != 1 || !staged[0].Duplicate {
		t.Fatalf("re-uploaded already-posted row not flagged duplicate: %+v", staged)
	}
	// The re-upload's recomputed hash equals the posted row's stored hash (idempotency).
	rows, _ := s.ImportRowsForBatch(context.Background(), batch2)
	if rows[0].DedupeHash != posted.dedupeHashOf(t, s) {
		// posted.dedupeHashOf is a tiny helper reading the stored hash of the posted row.
		t.Errorf("re-upload hash %q != posted row hash", rows[0].DedupeHash)
	}

	// Exactly one non-deleted transaction posted for this account (no double post).
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT s.transaction_id) FROM splits s
		 JOIN transactions t ON t.id = s.transaction_id
		 WHERE s.account_id = ? AND t.deleted = 0`, env.checking,
	).Scan(&n); err != nil {
		t.Fatalf("count posted txns: %v", err)
	}
	if n != 1 {
		t.Errorf("posted %d transactions on checking, want 1 (idempotent re-upload)", n)
	}
}

// dedupeHashOf reads the stored dedupe_hash of a posted import row (helper for the
// idempotency assertion).
func (r ImportRow) dedupeHashOf(t *testing.T, s *Store) string {
	t.Helper()
	var h string
	if err := s.db.QueryRow(`SELECT dedupe_hash FROM import_rows WHERE id = ?`, r.ID).Scan(&h); err != nil {
		t.Fatalf("read dedupe hash: %v", err)
	}
	return h
}

func strptr(s string) *string { return &s }

// Bank-CSV-import store tests (p17.2): batch-subsidiary validation and the dedupe
// flag against both an already-posted ledger split and a pending row in another
// batch. They build a tiny bespoke chart inline (AGENTS conventions).

// importEnv is the shared chart for the import store tests: a US subsidiary, a
// second subsidiary the account does NOT map to, an asset Checking account mapped
// to US, and an equity account for the other side of a posted transaction.
type importEnv struct {
	s        *Store
	subUS    int64
	subOther int64
	checking int64
	equity   int64
	profile  int64
}

func newImportEnv(t *testing.T) importEnv {
	t.Helper()
	s := New(testutil.NewDB(t))
	subUS := newSub(t, s, rootID, "US")
	subOther := newSub(t, s, rootID, "MX")

	checking, err := s.CreateAccount(mutCtx(), CreateAccountInput{
		Type: "asset", DefaultCurrency: "USD", Names: enName("Checking"),
		Subsidiaries: []int64{subUS},
	})
	if err != nil {
		t.Fatalf("CreateAccount checking: %v", err)
	}
	equity := mkAcct(t, s, "equity", "Opening", []int64{subUS}, nil, nil)

	profile, err := s.CreateMappingProfile(mutCtx(), "bank", bankimport.Config{
		Delimiter: bankimport.DelimiterComma, HasHeader: true, Amount: bankimport.AmountSingle,
		DateFmt: bankimport.DateISO, DateCol: 0, AmountCol: 1, DescCol: 2, MemoCol: 3,
	})
	if err != nil {
		t.Fatalf("CreateMappingProfile: %v", err)
	}
	return importEnv{s: s, subUS: subUS, subOther: subOther, checking: checking, equity: equity, profile: profile}
}

// TestBatchSubValidated: a batch whose account does NOT map to the chosen
// subsidiary is rejected with ErrBatchSubsidiaryMismatch; a mapped account is ok.
func TestBatchSubValidated(t *testing.T) {
	env := newImportEnv(t)

	// Checking maps to subUS -> ok.
	if _, err := env.s.CreateImportBatch(mutCtx(), "ok.csv", env.checking, env.subUS, env.profile, "2025-01-01T00:00:00Z"); err != nil {
		t.Fatalf("CreateImportBatch (mapped) rejected: %v", err)
	}

	// Checking does NOT map to subOther -> ErrBatchSubsidiaryMismatch.
	_, err := env.s.CreateImportBatch(mutCtx(), "bad.csv", env.checking, env.subOther, env.profile, "2025-01-01T00:00:00Z")
	if !errors.Is(err, ErrBatchSubsidiaryMismatch) {
		t.Fatalf("CreateImportBatch (unmapped) err = %v, want ErrBatchSubsidiaryMismatch", err)
	}

	// The rejected batch left NO row behind (validation inside the funnel rolled back).
	var n int
	if err := env.s.db.QueryRow(`SELECT COUNT(*) FROM import_batches WHERE filename = 'bad.csv'`).Scan(&n); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected batch left %d rows; want 0 (no audit trace)", n)
	}
}

// TestDedupeFlagsExistingSplitsAndPendingRows: a staged row duplicating (a) an
// already-posted ledger transaction on the account and (b) a pending row in another
// batch are both flagged duplicate; a genuinely new row is not.
func TestDedupeFlagsExistingSplitsAndPendingRows(t *testing.T) {
	env := newImportEnv(t)
	s := env.s

	// (a) Post a REAL ledger transaction: a $100.00 deposit into Checking (a
	// positive net-debit on the asset account) whose bank-side split carries the
	// description "Acme" and memo "Invoice". That split's (date, amount, description,
	// memo) is a dedupe source a matching bank row must hit (p26.20: the split
	// description replaces the retired payee name as the dedupe text).
	_, err := s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, Memo: "Invoice", Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, Memo: "Invoice", Description: "Acme", Position: 0},
			{AccountID: env.equity, Amount: -10000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// (b) Stage a PENDING row in a FIRST batch: a $55.00 line described "Bob".
	batch1, err := s.CreateImportBatch(mutCtx(), "jan.csv", env.checking, env.subUS, env.profile, "2025-02-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CreateImportBatch 1: %v", err)
	}
	staged1, err := s.StageImportRows(mutCtx(), batch1, env.checking, []bankimport.ParsedRow{
		{Date: "2025-01-20", AmountMinor: 5500, Description: "Bob", Memo: "Donation", Raw: []string{"2025-01-20", "55.00", "Bob", "Donation"}},
	})
	if err != nil {
		t.Fatalf("StageImportRows 1: %v", err)
	}
	if len(staged1) != 1 || staged1[0].Duplicate {
		t.Fatalf("first-batch row should be new (not duplicate): %+v", staged1)
	}

	// Now a SECOND batch (a re-upload) contains THREE rows:
	//   - the ledger deposit (matches the posted split) -> duplicate
	//   - the Bob line from batch1 (matches a pending row) -> duplicate
	//   - a brand-new line -> not a duplicate
	batch2, err := s.CreateImportBatch(mutCtx(), "reupload.csv", env.checking, env.subUS, env.profile, "2025-03-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CreateImportBatch 2: %v", err)
	}
	staged2, err := s.StageImportRows(mutCtx(), batch2, env.checking, []bankimport.ParsedRow{
		// Same natural key as the posted ledger split (case/whitespace differ to
		// prove normalization): date, +10000, "acme", "invoice".
		{Date: "2025-01-15", AmountMinor: 10000, Description: "ACME", Memo: "  invoice ", Raw: []string{"2025-01-15", "100.00", "ACME", "invoice"}},
		// Same natural key as the pending batch1 row.
		{Date: "2025-01-20", AmountMinor: 5500, Description: "Bob", Memo: "Donation", Raw: []string{"2025-01-20", "55.00", "Bob", "Donation"}},
		// Brand-new.
		{Date: "2025-02-10", AmountMinor: 2500, Description: "Carol", Memo: "New", Raw: []string{"2025-02-10", "25.00", "Carol", "New"}},
	})
	if err != nil {
		t.Fatalf("StageImportRows 2: %v", err)
	}
	if len(staged2) != 3 {
		t.Fatalf("staged %d rows, want 3", len(staged2))
	}
	if !staged2[0].Duplicate {
		t.Error("row matching a posted ledger split should be flagged duplicate")
	}
	if !staged2[1].Duplicate {
		t.Error("row matching a pending row in another batch should be flagged duplicate")
	}
	if staged2[2].Duplicate {
		t.Error("a genuinely new row should NOT be flagged duplicate")
	}

	// A duplicate STILL stages (advisory flag): all three rows persisted as pending.
	rows, err := s.ImportRowsForBatch(context.Background(), batch2)
	if err != nil {
		t.Fatalf("ImportRowsForBatch: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("persisted %d rows in batch2, want 3 (duplicates still stage)", len(rows))
	}
	for _, r := range rows {
		if r.Status != "pending" {
			t.Errorf("row %d status = %q, want pending", r.ID, r.Status)
		}
	}
}

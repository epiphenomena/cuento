package store

import (
	"context"
	"errors"
	"testing"

	"cuento/internal/bankimport"
	"cuento/internal/testutil"
)

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
		DateFmt: bankimport.DateISO, DateCol: 0, AmountCol: 1, PayeeCol: 2, MemoCol: 3,
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
	// positive net-debit on the asset account) with payee "Acme" and memo "Invoice".
	// Its bank-side split becomes a dedupe source a matching bank row must hit.
	payee, err := s.EnsurePayee(mutCtx(), "Acme")
	if err != nil {
		t.Fatalf("EnsurePayee: %v", err)
	}
	_, err = s.PostTransaction(mutCtx(), PostTransactionInput{
		Date: "2025-01-15", SubsidiaryID: env.subUS, PayeeID: &payee, Memo: "Invoice", Currency: "USD",
		Splits: []SplitInput{
			{AccountID: env.checking, Amount: 10000, Memo: "Invoice", Position: 0},
			{AccountID: env.equity, Amount: -10000, Position: 1},
		},
	})
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}

	// (b) Stage a PENDING row in a FIRST batch: a $55.00 line for payee "Bob".
	batch1, err := s.CreateImportBatch(mutCtx(), "jan.csv", env.checking, env.subUS, env.profile, "2025-02-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CreateImportBatch 1: %v", err)
	}
	staged1, err := s.StageImportRows(mutCtx(), batch1, env.checking, []bankimport.ParsedRow{
		{Date: "2025-01-20", AmountMinor: 5500, Payee: "Bob", Memo: "Donation", Raw: []string{"2025-01-20", "55.00", "Bob", "Donation"}},
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
		{Date: "2025-01-15", AmountMinor: 10000, Payee: "ACME", Memo: "  invoice ", Raw: []string{"2025-01-15", "100.00", "ACME", "invoice"}},
		// Same natural key as the pending batch1 row.
		{Date: "2025-01-20", AmountMinor: 5500, Payee: "Bob", Memo: "Donation", Raw: []string{"2025-01-20", "55.00", "Bob", "Donation"}},
		// Brand-new.
		{Date: "2025-02-10", AmountMinor: 2500, Payee: "Carol", Memo: "New", Raw: []string{"2025-02-10", "25.00", "Carol", "New"}},
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

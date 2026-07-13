package fixture_test

import (
	"context"
	"testing"

	"cuento/internal/ledger"
	"cuento/internal/testutil/fixture"
)

// TestExtendReconciliationSeamOptIn proves the p16 reconciliation seam is OPT-IN:
// New leaves no reconciliation (the Expected zero value), so the default fixture is
// unchanged.
func TestExtendReconciliationSeamOptIn(t *testing.T) {
	f := fixture.New(t)
	if f.Expected.Reconciliation.ID != 0 {
		t.Fatalf("reconciliation populated before ExtendReconciliation; seam should be opt-in")
	}
	// No reconciliation rows exist yet.
	var n int
	if err := f.DB.QueryRow(`SELECT COUNT(*) FROM reconciliations`).Scan(&n); err != nil {
		t.Fatalf("count reconciliations: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciliations before seam = %d, want 0", n)
	}
}

// TestReconSpansFunds is the D13/D20 payoff: a SINGLE reconciliation on Checking US
// (USD) clears both RESTRICTED (fund_id NOT NULL) and UNRESTRICTED (fund_id NULL)
// splits against ONE statement -- the statement is about the bank account, not the
// fund. It also proves the seam leaves exactly the two intended items uncleared and
// that ledger.Check stays clean (Z8/Z9 pass on the finalized recon).
func TestReconSpansFunds(t *testing.T) {
	f := fixture.New(t)
	f.ExtendReconciliation(t)

	recon := f.Expected.Reconciliation
	if recon.ID == 0 {
		t.Fatalf("seam did not populate the reconciliation")
	}

	// The recon is finalized on Checking US / USD, opening 0.
	var status, currency string
	var acct, bal int64
	if err := f.DB.QueryRow(
		`SELECT account_id, currency, status, statement_balance FROM reconciliations WHERE id = ?`, recon.ID,
	).Scan(&acct, &currency, &status, &bal); err != nil {
		t.Fatalf("load recon: %v", err)
	}
	if status != "finalized" || acct != f.IDs.CheckingUS || currency != "USD" {
		t.Errorf("recon = {acct %d, %s, %s}, want {%d, USD, finalized}", acct, currency, status, f.IDs.CheckingUS)
	}
	if bal != recon.StatementBalance {
		t.Errorf("statement_balance = %d, want %d", bal, recon.StatementBalance)
	}

	// SPANS FUNDS: the cleared set contains BOTH a fund-tagged split and a NULL-fund
	// split against this one recon.
	var restricted, unrestricted int
	if err := f.DB.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE reconciliation_id = ? AND fund_id IS NOT NULL`, recon.ID,
	).Scan(&restricted); err != nil {
		t.Fatalf("count restricted cleared: %v", err)
	}
	if err := f.DB.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE reconciliation_id = ? AND fund_id IS NULL`, recon.ID,
	).Scan(&unrestricted); err != nil {
		t.Fatalf("count unrestricted cleared: %v", err)
	}
	if restricted == 0 {
		t.Errorf("cleared restricted (fund_id NOT NULL) splits = 0, want > 0 (D13/D20 spans funds)")
	}
	if unrestricted == 0 {
		t.Errorf("cleared unrestricted (fund_id NULL) splits = 0, want > 0 (D13/D20 spans funds)")
	}

	// The two intended items are UNCLEARED: no Checking US split of either txn is
	// cleared.
	for _, txnID := range recon.UnclearedTxns {
		var cleared int
		if err := f.DB.QueryRow(
			`SELECT COUNT(*) FROM splits WHERE transaction_id = ? AND account_id = ? AND reconciliation_id IS NOT NULL`,
			txnID, f.IDs.CheckingUS,
		).Scan(&cleared); err != nil {
			t.Fatalf("count cleared for uncleared txn %d: %v", txnID, err)
		}
		if cleared != 0 {
			t.Errorf("txn %d Checking US split cleared = %d, want 0 (should be uncleared)", txnID, cleared)
		}
	}

	// Cleared count matches Expected, and the ledger stays clean (Z8/Z9).
	var clearedCount int
	if err := f.DB.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE reconciliation_id = ?`, recon.ID,
	).Scan(&clearedCount); err != nil {
		t.Fatalf("count cleared: %v", err)
	}
	if clearedCount != recon.ClearedCount {
		t.Errorf("cleared count = %d, want %d", clearedCount, recon.ClearedCount)
	}

	vs, err := ledger.Check(context.Background(), f.DB)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	warnRules := map[string]int{}
	for _, v := range vs {
		switch v.Severity {
		case ledger.Error:
			t.Errorf("unexpected Error violation after ExtendReconciliation: %s: %s", v.Rule, v.Detail)
		case ledger.Warning:
			warnRules[v.Rule]++
		}
	}
	// The fixture's only warning is Z19 (the deliberately unmapped Event Income leaf);
	// the reconciliation must not introduce any new warning.
	for rule := range warnRules {
		if rule != "Z19" {
			t.Errorf("unexpected warning rule %s after ExtendReconciliation", rule)
		}
	}
}

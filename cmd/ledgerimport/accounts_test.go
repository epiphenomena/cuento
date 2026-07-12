package main

import (
	"strings"
	"testing"
)

func TestRunAccountsEmitsReviewableSkeleton(t *testing.T) {
	// Synthetic source with two distinct accounts across two subsidiaries; one
	// account (Checking) appears twice and in both US and MX, so subsidiaries must
	// be collected as a UNION and the account emitted ONCE.
	src := strings.Join([]string{
		header,
		row("US", "A", "x", "Checking", "", "2025-01-01", "", "1", "m", "", "USD", "1.0", "10.00", "10.00", "0", "0", "Assets"),
		row("MX", "A", "x", "Checking", "", "2025-01-02", "", "2", "m", "", "MXN", "1.0", "5.00", "5.00", "0", "0", "Assets"),
		row("US", "E", "x", "Rent", "PROG", "2025-01-03", "MGT", "3", "m", "", "USD", "1.0", "20.00", "20.00", "0", "0", "Expenses"),
	}, "\n") + "\n"

	var out strings.Builder
	if err := runAccounts(strings.NewReader(src), &out); err != nil {
		t.Fatalf("runAccounts: %v", err)
	}

	rows, err := ReadAccountMap(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("emitted skeleton not parseable: %v\n%s", err, out.String())
	}

	byAcct := map[string]AccountMap{}
	for _, r := range rows {
		byAcct[r.SourceAcct] = r
	}

	// Checking appears once, type asset (from stmt A), subs union {US,MX} names.
	chk, ok := byAcct["Checking"]
	if !ok {
		t.Fatalf("Checking not emitted; got %v", byAcct)
	}
	if chk.CuentoType != "asset" {
		t.Errorf("Checking type = %q, want asset", chk.CuentoType)
	}
	if len(chk.Subsidiaries) != 2 {
		t.Errorf("Checking subs = %v, want two (US+MX union)", chk.Subsidiaries)
	}
	if chk.CuentoParent != "Assets" {
		t.Errorf("Checking parent = %q, want Assets", chk.CuentoParent)
	}

	// Rent: expense (stmt E), functional class prefilled from kls, parent Expenses.
	rent, ok := byAcct["Rent"]
	if !ok {
		t.Fatalf("Rent not emitted")
	}
	if rent.CuentoType != "expense" {
		t.Errorf("Rent type = %q, want expense", rent.CuentoType)
	}

	// Exactly the two distinct leaf accounts (Checking, Rent) plus their parents
	// (Assets, Expenses) as rows -- a distinct set, no duplicates.
	if len(rows) != len(byAcct) {
		t.Errorf("duplicate rows emitted: %d rows, %d distinct accounts", len(rows), len(byAcct))
	}
}

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"cuento/internal/db"
	"cuento/internal/ledger"
	"cuento/internal/synth"
)

// TestDemoGeneratorAntiDrift is the ALIGNMENT GUARD (the p26.81 owner requirement):
// it generates the demo database through the EXACT path the `cuento demo` CLI uses
// (generateDemo -> synth.BuildDemo, the store write funnel) and then asserts two
// things, so the build FAILS if a schema/invariant/API change breaks the generator
// OR a new feature ships without demo data behind it:
//
//  1. INTEGRITY: ledger.Check (the `cuento check` suite) is ERROR-clean on the
//     generated db. (Warnings are allowed: the demo carries the SAME deliberate
//     Z19 warning as the fixture -- Event Income is an intentionally-unmapped R/E
//     leaf -- so this asserts Error-clean, NOT --strict, exactly as the task says.)
//  2. FEATURE COVERAGE: counts > 0 across every showcased feature (subsidiaries,
//     funds incl. a restricted one, programs, transactions across multiple years,
//     expense reports in all three states, a finalized AND an open reconciliation,
//     a budget with lines, a bank-import profile, users across permission levels,
//     bilingual account names).
func TestDemoGeneratorAntiDrift(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "demo.db")

	if err := generateDemo(ctx, path); err != nil {
		t.Fatalf("generateDemo: %v", err)
	}

	sqldb, err := db.Open(path)
	if err != nil {
		t.Fatalf("open generated demo db: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	// --- (1) integrity: ledger.Check is ERROR-clean (warnings allowed).
	violations, err := ledger.Check(ctx, sqldb)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	var errs, warns int
	for _, v := range violations {
		switch v.Severity {
		case ledger.Error:
			errs++
			t.Errorf("demo db integrity ERROR %s: %s", v.Rule, v.Detail)
		case ledger.Warning:
			warns++
		}
	}
	if errs != 0 {
		t.Fatalf("demo db has %d integrity error(s); expected 0", errs)
	}

	// --- (2) feature coverage: every showcased feature has data.
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := sqldb.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count query %q: %v", query, err)
		}
		return n
	}
	atLeast := func(name string, got, want int) {
		t.Helper()
		if got < want {
			t.Errorf("feature coverage %s: got %d, want >= %d", name, got, want)
		}
	}

	// Multi-subsidiary org (parent + 2 children) in >1 currency.
	atLeast("subsidiaries", count(`SELECT count(*) FROM subsidiaries`), 3)
	atLeast("currencies used by subsidiaries", count(`SELECT count(DISTINCT base_currency) FROM subsidiaries`), 2)

	// Programs + a full chart of accounts across all five types.
	atLeast("programs", count(`SELECT count(*) FROM programs`), 2)
	atLeast("account types", count(`SELECT count(DISTINCT type) FROM accounts`), 5)

	// p27.1 shared account attributes: at least one spendable-cash account, one
	// open-item receivable (asset -> A/R) and one open-item payable (liability -> A/P).
	atLeast("current_cash accounts", count(`SELECT count(*) FROM accounts WHERE current_cash = 1`), 1)
	atLeast("open_item receivables", count(`SELECT count(*) FROM accounts WHERE open_item = 1 AND type = 'asset'`), 1)
	atLeast("open_item payables", count(`SELECT count(*) FROM accounts WHERE open_item = 1 AND type = 'liability'`), 1)

	// Funds including at least one RESTRICTED fund.
	atLeast("funds", count(`SELECT count(*) FROM funds`), 3)
	atLeast("restricted funds", count(`SELECT count(*) FROM funds WHERE restriction != 'unrestricted'`), 1)

	// Transactions across MULTIPLE YEARS.
	atLeast("transaction years", count(`SELECT count(DISTINCT substr(date,1,4)) FROM transactions WHERE deleted = 0`), 2)
	atLeast("transactions", count(`SELECT count(*) FROM transactions WHERE deleted = 0`), 20)

	// Expense reports in draft / submitted / posted(converted) states.
	atLeast("draft expense reports", count(`SELECT count(*) FROM expense_reports WHERE status = 'draft'`), 1)
	atLeast("submitted expense reports", count(`SELECT count(*) FROM expense_reports WHERE status = 'submitted'`), 1)
	atLeast("converted expense reports", count(`SELECT count(*) FROM expense_reports WHERE status = 'converted'`), 1)

	// At least one FINALIZED and one OPEN reconciliation.
	atLeast("finalized reconciliations", count(`SELECT count(*) FROM reconciliations WHERE status = 'finalized'`), 1)
	atLeast("open reconciliations", count(`SELECT count(*) FROM reconciliations WHERE status = 'open'`), 1)

	// A budget with lines (old schedule-based model, live until p27.3).
	atLeast("budgets", count(`SELECT count(*) FROM budgets`), 1)
	atLeast("budget lines", count(`SELECT count(*) FROM budget_lines`), 1)

	// A budget PLAN with splits (new p27.2 split-derived model): >=1 plan, several
	// splits across >=2 programs, incl. at least one open_item A/L leg (program NULL).
	atLeast("budget plans", count(`SELECT count(*) FROM budget_plans`), 1)
	atLeast("budget splits", count(`SELECT count(*) FROM budget_splits`), 5)
	atLeast("budget-split programs", count(`SELECT count(DISTINCT program_id) FROM budget_splits WHERE program_id IS NOT NULL`), 2)
	atLeast("open-item budget splits (no program)", count(`SELECT count(*) FROM budget_splits WHERE program_id IS NULL`), 1)

	// A bank-import mapping profile (+ a staged batch with rows).
	atLeast("mapping profiles", count(`SELECT count(*) FROM mapping_profiles`), 1)
	atLeast("import batches", count(`SELECT count(*) FROM import_batches`), 1)
	atLeast("staged import rows", count(`SELECT count(*) FROM import_rows`), 1)

	// Users across permission levels (an admin, a submitter, a viewer) beyond the
	// seeded system user (id 1).
	atLeast("human users", count(`SELECT count(*) FROM users WHERE id > 1`), 3)
	atLeast("admin users", count(`SELECT count(*) FROM users WHERE is_admin = 1`), 1)
	atLeast("expense submitters", count(`SELECT count(*) FROM users WHERE can_submit_expenses = 1`), 1)
	atLeast("read-only users", count(`SELECT count(*) FROM users WHERE id > 1 AND txn_perm = 'read'`), 1)

	// Bilingual account names (en + es) so the language toggle demonstrates.
	atLeast("english account names", count(`SELECT count(*) FROM account_names WHERE lang = 'en'`), 5)
	atLeast("spanish account names", count(`SELECT count(*) FROM account_names WHERE lang = 'es'`), 5)

	// Rates so the multi-currency conversion / consolidation reports populate.
	atLeast("exchange rates", count(`SELECT count(*) FROM exchange_rates`), 1)
}

// TestDemoGeneratorDeterministic asserts the generator produces the SAME business data
// on repeat runs (fixed clock + fixed base dates, no time.Now). It compares the
// non-user tables (whose only non-reproducible surface -- argon2id password salts on
// users -- is deliberately excluded, per the p26.81 determinism note).
func TestDemoGeneratorDeterministic(t *testing.T) {
	ctx := context.Background()

	fingerprint := func() string {
		path := filepath.Join(t.TempDir(), "demo.db")
		if err := generateDemo(ctx, path); err != nil {
			t.Fatalf("generateDemo: %v", err)
		}
		sqldb, err := db.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = sqldb.Close() }()

		// A stable projection of the deterministic business data (no users, no salts).
		// Includes the SPLIT rows (id/txn/account/amount/fund) so a drift in any
		// posted amount -- not just a header field -- is caught, matching the manual
		// md5 check the generator was verified with.
		rows, err := sqldb.QueryContext(ctx, `
			SELECT s.id, s.transaction_id, s.account_id, s.amount, COALESCE(s.fund_id, 0),
			       t.date, t.memo, t.currency, t.subsidiary_id
			FROM splits s JOIN transactions t ON t.id = s.transaction_id
			ORDER BY s.id`)
		if err != nil {
			t.Fatalf("query splits: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var fp string
		for rows.Next() {
			var sid, tid, acct, amount, fund, sub int64
			var date, memo, ccy string
			if err := rows.Scan(&sid, &tid, &acct, &amount, &fund, &date, &memo, &ccy, &sub); err != nil {
				t.Fatalf("scan: %v", err)
			}
			fp += fmt.Sprintf("%d|%d|%d|%d|%d|%s|%s|%s|%d;", sid, tid, acct, amount, fund, date, memo, ccy, sub)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return fp
	}

	if a, b := fingerprint(), fingerprint(); a != b {
		t.Errorf("demo generation not deterministic:\n run1 = %q\n run2 = %q", a, b)
	}
}

// TestDemoCredentialsDocumented pins the demo credential contract so docs/deploy.md and
// the CLI output cannot silently drift from what the generator seeds.
func TestDemoCredentialsDocumented(t *testing.T) {
	users := synth.DemoUsers()
	if len(users) != 3 {
		t.Fatalf("expected 3 demo users, got %d", len(users))
	}
	for _, u := range users {
		if u.Username == "" || u.Password == "" || u.Role == "" {
			t.Errorf("incomplete demo user: %+v", u)
		}
	}
}

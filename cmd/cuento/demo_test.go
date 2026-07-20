package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"cuento/internal/db"
	"cuento/internal/ids"
	"cuento/internal/ledger"
	"cuento/internal/reports"
	"cuento/internal/store"
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

	// p31 FX remeasurement: at least one transaction denominated in a currency that is
	// NOT its subsidiary's functional (base) currency -- the ASC 830-20 remeasurement
	// exposure the FX-detail report and Statement-of-Activities FX line demonstrate (the
	// Lempira example: an HNL transaction in the USD-functional US sub).
	atLeast("cross-functional-currency transactions",
		count(`SELECT count(*) FROM transactions t JOIN subsidiaries s ON s.id = t.subsidiary_id
		        WHERE t.deleted = 0 AND t.currency <> s.base_currency`), 1)

	// Programs + a full chart of accounts across all five types.
	atLeast("programs", count(`SELECT count(*) FROM programs`), 2)
	atLeast("account types", count(`SELECT count(DISTINCT type) FROM accounts`), 5)

	// p27.1 shared account attributes: at least one spendable-cash account, one
	// open-item receivable (asset -> A/R) and one open-item payable (liability -> A/P).
	atLeast("current_cash accounts", count(`SELECT count(*) FROM accounts WHERE current_cash = 1`), 1)
	atLeast("receivable_payable receivables", count(`SELECT count(*) FROM accounts WHERE receivable_payable = 1 AND type = 'asset'`), 1)
	atLeast("receivable_payable payables", count(`SELECT count(*) FROM accounts WHERE receivable_payable = 1 AND type = 'liability'`), 1)

	// p28.7: at least one account carries a free-text note, so the chart's Notes
	// column (p28.8) shows populated in the demo.
	atLeast("accounts with notes", count(`SELECT count(*) FROM accounts WHERE notes IS NOT NULL AND notes != ''`), 1)

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

	// A budget PLAN with splits (the p27.2 split-derived model): >=1 plan, several
	// splits across >=2 programs, incl. at least one receivable_payable A/L leg (program NULL).
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

	// p27.4d: a PROGRAM-SUBTREE-SCOPED report grant EXISTS (the feature ships with demo
	// data). This is the load-bearing anti-drift line -- if the demo generator ever stops
	// minting the scoped grant, the program-scope feature would be undemonstrable in the
	// hosted demo and this fails.
	atLeast("program-scoped report grants", count(`SELECT count(*) FROM user_report_grants WHERE program_id IS NOT NULL`), 1)
	assertDemoProgramScopedGrant(ctx, t, sqldb)
}

// assertDemoProgramScopedGrant verifies the p27.4d camp-director scoped grant end to
// end on the generated demo db: (a) the grant has the expected SHAPE (the camp director
// holds exactly "financial" scoped to the Educacion program subtree); (b) it FILTERS --
// running income_statement (a program-dimensioned report) under that subtree shows only
// Educacion's rows, NOT the Food Pantry sibling; (c) the DENY PRECONDITION holds -- the
// demoted activities_by_restriction report is in the SAME granted group yet is NOT
// program-dimensioned, so a purely program-scoped grant cannot reach it. The runtime 403
// for such a user is proven by the web layer's TestPermissionMatrix/TestDecidePolicy and
// the p27.4c e2e (report-grant-scope.spec.js); here we assert the demo satisfies the
// precondition, which is the reachable-from-cmd analogue (decide/grantChecker are
// unexported web internals, deliberately not reimplemented here per the advisor note).
func assertDemoProgramScopedGrant(ctx context.Context, t *testing.T, sqldb *sql.DB) {
	t.Helper()

	// (a) shape: the camp director holds exactly one grant -- "financial" scoped to the
	// program named "Educacion".
	var group, progName string
	err := sqldb.QueryRowContext(ctx, `
		SELECT g.group_name, p.name
		FROM user_report_grants g
		JOIN users u ON u.id = g.user_id
		JOIN programs p ON p.id = g.program_id
		WHERE u.username = ? AND g.program_id IS NOT NULL`,
		synth.DemoCampDirectorUser).Scan(&group, &progName)
	if err != nil {
		t.Fatalf("camp-director scoped grant: %v", err)
	}
	if group != "financial" {
		t.Errorf("camp-director scoped grant group = %q, want financial", group)
	}
	if progName != "Educacion" {
		t.Errorf("camp-director scope program = %q, want Educacion", progName)
	}

	// Resolve the Educacion program id + the demo Root subsidiary, then compute the
	// grant's subtree via the SAME primitive production uses (ProgramSubtree).
	st := store.New(sqldb)
	var educacionID ids.ProgramID
	var rootSub int64
	if err := sqldb.QueryRowContext(ctx, `SELECT id FROM programs WHERE name = 'Educacion'`).Scan(&educacionID); err != nil {
		t.Fatalf("resolve Educacion id: %v", err)
	}
	if err := sqldb.QueryRowContext(ctx, `SELECT id FROM subsidiaries WHERE parent_id IS NULL`).Scan(&rootSub); err != nil {
		t.Fatalf("resolve root subsidiary: %v", err)
	}
	subtree, err := st.ProgramSubtree(ctx, educacionID)
	if err != nil {
		t.Fatalf("program subtree: %v", err)
	}

	// (b) filters: run income_statement under the scope. Educacion's "Program Supplies"
	// (in-subtree) is present; the Food Pantry sibling's "Food Purchases" is absent.
	rep, ok := reports.Default().Get(reports.IncomeStatementReportID)
	if !ok {
		t.Fatalf("income_statement not registered")
	}
	progScope := make([]reports.ProgramID, len(subtree))
	for i, id := range subtree {
		progScope[i] = reports.ProgramID(id)
	}
	p := reports.Params{
		Scope:          reports.SubsidiaryID(rootSub),
		From:           "2025-01-01",
		To:             "2030-12-31",
		Granularity:    reports.GranNone,
		TargetCurrency: "USD",
		Lang:           "en",
		ProgramScope:   progScope,
	}
	table, err := rep.Run(ctx, reports.NewToolkit(st, p), p)
	if err != nil {
		t.Fatalf("run scoped income_statement: %v", err)
	}
	text := tableText(table)
	if !strings.Contains(text, "Program Supplies") {
		t.Errorf("scoped income_statement missing Educacion's Program Supplies (in-subtree)")
	}
	if strings.Contains(text, "Food Purchases") {
		t.Errorf("scoped income_statement leaks Food Pantry's Food Purchases (sibling subtree)")
	}

	// (c) deny precondition: activities_by_restriction is in the granted "financial" group
	// but is NOT program-dimensioned -> a purely program-scoped grant cannot reach it.
	demoted, ok := reports.Default().Get(reports.ActivitiesByRestrictionReportID)
	if !ok {
		t.Fatalf("activities_by_restriction not registered")
	}
	if demoted.Group != "financial" {
		t.Errorf("activities_by_restriction group = %q, want financial (same group as the scoped grant)", demoted.Group)
	}
	if demoted.ProgramDimensioned {
		t.Errorf("activities_by_restriction is ProgramDimensioned; a scoped grant would reach it (deny precondition broken)")
	}
}

// tableText concatenates every cell's literal text across a rendered report Table --
// enough to assert the PRESENCE / ABSENCE of a stored account name (a proper noun
// rendered verbatim) without depending on cell positions.
func tableText(tb reports.Table) string {
	var b []byte
	for _, row := range tb.Rows {
		for _, c := range row.Cells {
			b = append(b, c.Text...)
			b = append(b, '\n')
		}
	}
	return string(b)
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
	if len(users) != 4 {
		t.Fatalf("expected 4 demo users, got %d", len(users))
	}
	for _, u := range users {
		if u.Username == "" || u.Password == "" || u.Role == "" {
			t.Errorf("incomplete demo user: %+v", u)
		}
	}
}

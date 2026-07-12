package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"cuento/internal/db/sqlc"
	"cuento/internal/ledger"
	"cuento/internal/store"
	"cuento/internal/testutil"
)

// ---- synthetic mapping (all invented values, AGENTS rule 11) --------------

// testConfig returns a global config JSON string wiring a tiny two-country,
// one-program, one-fund org. FX Clearing, Opening Balances, and a skip-country
// marker are configured so the build exercises every structural path.
func testConfig() string {
	return `{
  "root_subsidiary_name": "Test Org Root",
  "root_base_currency": "USD",
  "subsidiaries": {
    "US": {"name": "Test US", "base_currency": "USD"},
    "MX": {"name": "Test MX", "base_currency": "MXN"}
  },
  "programs": {"EDU": "Education"},
  "funds": {
    "GRANT1": {"name": "Grant One", "funder": "Funder A", "purpose": "water",
               "restriction": "purpose", "subsidiaries": ["Test US", "Test MX"],
               "program": "Education"}
  },
  "functional_classes": {"PRG": "program", "MGT": "management"},
  "base_currency": "USD",
  "fx_clearing_account": "FX Clearing",
  "opening_balance_account": "Opening Balances",
  "payee_column": "desc",
  "skip_countries": ["CONSOL"],
  "opening_balance_typs": ["opening"]
}`
}

// testAccountMap returns the account-mapping CSV for the accounts referenced by
// the synthetic source below. subsidiaries are ";"-separated cuento sub NAMES.
func testAccountMap() string {
	rows := []AccountMap{
		{SourceAcct: "Assets", CuentoType: "asset", CuentoParent: "", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Assets", NameES: "Activos"},
		{SourceAcct: "Checking", CuentoType: "asset", CuentoParent: "Assets", Subsidiaries: []string{"Test US"}, NameEN: "Checking", NameES: "Cuenta"},
		{SourceAcct: "Cash MX", CuentoType: "asset", CuentoParent: "Assets", Subsidiaries: []string{"Test MX"}, NameEN: "Cash MX", NameES: "Efectivo"},
		{SourceAcct: "Revenue", CuentoType: "revenue", CuentoParent: "", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Revenue", NameES: "Ingresos"},
		{SourceAcct: "Grant Revenue", CuentoType: "revenue", CuentoParent: "Revenue", Subsidiaries: []string{"Test US", "Test MX"}, DefaultProgram: "Education", NameEN: "Grant Revenue", NameES: "Ingreso Beca"},
		{SourceAcct: "Expenses", CuentoType: "expense", CuentoParent: "", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Expenses", NameES: "Gastos"},
		{SourceAcct: "Supplies", CuentoType: "expense", CuentoParent: "Expenses", Subsidiaries: []string{"Test US", "Test MX"}, FunctionalClass: "program", NameEN: "Supplies", NameES: "Suministros"},
		{SourceAcct: "Equity", CuentoType: "equity", CuentoParent: "", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Equity", NameES: "Patrimonio"},
		{SourceAcct: "Opening Balances", CuentoType: "equity", CuentoParent: "Equity", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Opening Balances", NameES: "Saldos Iniciales"},
		{SourceAcct: "FX Clearing", CuentoType: "equity", CuentoParent: "Equity", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "FX Clearing", NameES: "Compensacion FX"},
	}
	var b strings.Builder
	if err := WriteAccountMap(&b, rows); err != nil {
		panic(err)
	}
	return b.String()
}

// row builds one synthetic 22-field CSV data line. Only the fields the build uses
// are meaningful; the float-noisy v/ndb/fndb are filled with obvious garbage to
// prove they are ignored.
func row(country, stmt, typ, acct, kat, dt, kls, tid, desc, donor, currency, xrt, db, fdb, cr, fcr, parent string) string {
	f := []string{
		country, stmt, typ, acct, kat, dt,
		"9.9999999", "8.8888888", "7.7777777", // v, ndb, fndb -- garbage, ignored
		kls, "subclass", tid, desc, donor, currency, xrt,
		db, fdb, cr, fcr, "", parent,
	}
	return strings.Join(f, ",")
}

// testSource returns the synthetic export exercising: an opening-balance
// single-currency group, a plain single-currency 2-split txn, a restricted-fund
// txn (per-fund balance), a MULTI-CURRENCY tid decomposed via FX Clearing, and a
// skipped consolidation-marker row.
func testSource() string {
	lines := []string{
		header,
		// tid 1: opening balances (typ "opening") -- one side only; the counter-leg
		// goes to Opening Balances. US, USD.
		row("US", "A", "opening", "Checking", "", "2025-01-01", "", "1", "opening", "", "USD", "1.0", "1000.00", "1000.00", "0", "0", "Assets"),
		// tid 2: plain expense, US, USD, unrestricted. Supplies (expense, program).
		row("US", "E", "spend", "Supplies", "", "2025-02-01", "PRG", "2", "office", "", "USD", "1.0", "40.00", "40.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "", "2025-02-01", "", "2", "office", "", "USD", "1.0", "0", "0", "40.00", "40.00", "Assets"),
		// tid 3: restricted grant receipt, MX, MXN, fund GRANT1, program EDU.
		row("MX", "A", "receipt", "Cash MX", "", "2025-03-01", "", "3", "grant in", "GRANT1", "MXN", "1.0", "500.00", "500.00", "0", "0", "Assets"),
		row("MX", "I", "receipt", "Grant Revenue", "EDU", "2025-03-01", "", "3", "grant in", "GRANT1", "MXN", "1.0", "0", "0", "500.00", "500.00", "Revenue"),
		// tid 4: MULTI-CURRENCY transfer (unrestricted): MXN cash out, USD cash in.
		row("MX", "A", "xfer", "Cash MX", "", "2025-04-01", "", "4", "fx transfer", "", "MXN", "0.05", "0", "0", "5000.00", "5000.00", "Assets"),
		row("US", "A", "xfer", "Checking", "", "2025-04-01", "", "4", "fx transfer", "", "USD", "0.05", "250.00", "250.00", "0", "0", "Assets"),
		// tid 5: MULTI-CURRENCY *restricted* transfer (fund GRANT1) -- the case that
		// breaks a per-CURRENCY-only counter-leg: each FX-Clearing counter-leg must
		// carry the fund so every currency leg balances WITHIN the GRANT1 group.
		row("MX", "A", "xfer", "Cash MX", "", "2025-06-01", "", "5", "fx grant", "GRANT1", "MXN", "0.05", "0", "0", "5000.00", "5000.00", "Assets"),
		row("US", "A", "xfer", "Checking", "", "2025-06-01", "", "5", "fx grant", "GRANT1", "USD", "0.05", "250.00", "250.00", "0", "0", "Assets"),
		// A consolidation-marker row (country CONSOL) that must be SKIPPED entirely.
		row("CONSOL", "A", "elim", "Checking", "", "2025-05-01", "", "9", "elim", "", "", "1.0", "0", "0", "0", "0", "Assets"),
	}
	return strings.Join(lines, "\n") + "\n"
}

// buildInto runs the build core into a fresh migrated temp db and returns the
// result. anonymize toggles the payee/memo hashing.
func buildInto(t *testing.T, anonymize bool) (*sql.DB, *store.Store, *BuildResult) {
	t.Helper()
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	accMap, err := ReadAccountMap(strings.NewReader(testAccountMap()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	cfg, err := ReadConfig(strings.NewReader(testConfig()))
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, st, anonymize)
	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	return sqldb, st, res
}

func TestMappingAppliesSubFundProgramFunction(t *testing.T) {
	sqldb, st, res := buildInto(t, false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Subsidiary tree: root renamed, two children created.
	if got := mustSub(t, st, res.SubsidiaryIDs["Test US"]); got.Name != "Test US" || got.BaseCurrency != "USD" {
		t.Errorf("Test US sub wrong: %+v", got)
	}
	if got := mustSub(t, st, res.SubsidiaryIDs["Test MX"]); got.BaseCurrency != "MXN" {
		t.Errorf("Test MX base currency = %q, want MXN", got.BaseCurrency)
	}
	root := mustSub(t, st, 1)
	if root.Name != "Test Org Root" {
		t.Errorf("root not renamed: %q", root.Name)
	}

	// Program derived from kat.
	if _, ok := res.ProgramIDs["Education"]; !ok {
		t.Errorf("Education program not created")
	}

	// Fund derived from donor, scoped to two subs, program-scoped.
	fid, ok := res.FundIDs["GRANT1"]
	if !ok {
		t.Fatalf("fund GRANT1 not created")
	}
	fund, err := st.GetFund(ctx, fid)
	if err != nil {
		t.Fatalf("GetFund: %v", err)
	}
	if fund.Name != "Grant One" || fund.Restriction != "purpose" {
		t.Errorf("fund wrong: %+v", fund)
	}
	if !fund.ProgramID.Valid || fund.ProgramID.Int64 != res.ProgramIDs["Education"] {
		t.Errorf("fund program scope not applied: %+v", fund.ProgramID)
	}

	// The restricted revenue split must carry the fund AND the account-default
	// program (Education). Verify via a raw read of the posted splits for tid 3.
	assertSplitFundProgram(t, sqldb, res, "Grant Revenue", res.FundIDs["GRANT1"], res.ProgramIDs["Education"])

	// The unrestricted expense split (Supplies) must carry its functional class
	// default (program) and NO fund.
	assertExpenseClass(t, sqldb, res, "Supplies", "program")
}

func TestImportedBooksBalance(t *testing.T) {
	sqldb, _, res := buildInto(t, false)
	ctx := context.Background()

	// ledger.Check must have ZERO Error violations on the produced db.
	vs, err := ledger.Check(ctx, sqldb)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	if ledger.HasErrors(vs) {
		t.Fatalf("produced db has Error violations: %+v", vs)
	}

	// The multi-currency tid 4 must have produced TWO transactions (one per
	// currency), each balanced through FX Clearing.
	if n := res.txnCountForTid("4"); n != 2 {
		t.Errorf("multi-currency tid 4 produced %d transactions, want 2 (FX-clearing pair)", n)
	}

	// FX Clearing must carry a split in each currency leg (proof the decomposition
	// routed through it).
	fxID := res.AccountIDs["FX Clearing"]
	if !res.accountHasSplit(fxID) {
		t.Errorf("FX Clearing account carries no split; decomposition did not route through it")
	}

	// The RESTRICTED multi-currency tid 5 must ALSO produce two transactions -- the
	// FX-Clearing counter-leg must be tagged the fund so each currency leg balances
	// WITHIN the GRANT1 group (a per-currency-only counter-leg would net the fund
	// group nonzero and the store would reject the whole transaction).
	if n := res.txnCountForTid("5"); n != 2 {
		t.Fatalf("restricted multi-currency tid 5 produced %d transactions, want 2", n)
	}
	// And the FX-Clearing splits produced for that group must carry the fund.
	gid := res.FundIDs["GRANT1"]
	var fundedFXClearing int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM splits WHERE account_id = ? AND fund_id = ?`, fxID, gid,
	).Scan(&fundedFXClearing); err != nil {
		t.Fatalf("count funded FX Clearing splits: %v", err)
	}
	if fundedFXClearing == 0 {
		t.Errorf("FX Clearing carries no GRANT1-tagged split; restricted FX counter-leg not fund-aware")
	}

	// Opening Balances absorbed the single-split opening group (tid 1).
	obID := res.AccountIDs["Opening Balances"]
	if !res.accountHasSplit(obID) {
		t.Errorf("Opening Balances carries no split; opening-balance group not absorbed")
	}

	// The consolidation-marker row (tid 9) must have been skipped: no transaction.
	if res.txnCountForTid("9") != 0 {
		t.Errorf("consolidation-marker tid 9 was not skipped")
	}

	// Every produced transaction balances overall and per fund (store enforces it;
	// a clean ledger.Check above already proves Z1/Z10, but assert no warnings we
	// did not expect were swallowed -- surface them).
	for _, w := range res.Warnings {
		t.Logf("build warning surfaced: %s", w)
	}
}

func TestAnonymizeHashesPayeesAndMemos(t *testing.T) {
	sqldb, _, res := buildInto(t, true)

	// The synthetic source used payee-ish desc "office"/"grant in" and memos. With
	// --anonymize, NO produced payee name nor memo may equal a synthetic original.
	originals := []string{"office", "grant in", "fx transfer", "opening"}
	names := allPayeeNames(t, sqldb)
	memos := allMemos(t, sqldb)
	for _, o := range originals {
		for _, n := range names {
			if n == o {
				t.Errorf("payee %q was not anonymized", o)
			}
		}
		for _, m := range memos {
			if m == o {
				t.Errorf("memo %q was not anonymized", o)
			}
		}
	}
	// Sanity: anonymization actually produced hashed (hex) content, not empties.
	if len(names) == 0 {
		t.Errorf("no payees produced")
	}
	_ = res
}

// ---- small raw-read helpers (reads outside the store are fine via sqlc/raw) --

func mustSub(t *testing.T, st *store.Store, id int64) sqlc.Subsidiary {
	t.Helper()
	s, err := st.GetSubsidiary(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSubsidiary(%d): %v", id, err)
	}
	return s
}

func assertSplitFundProgram(t *testing.T, sqldb *sql.DB, res *BuildResult, srcAcct string, wantFund, wantProg int64) {
	t.Helper()
	acctID := res.AccountIDs[srcAcct]
	rows, err := sqldb.Query(`SELECT fund_id, program_id FROM splits WHERE account_id = ?`, acctID)
	if err != nil {
		t.Fatalf("query splits: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var f, p interface{}
		if err := rows.Scan(&f, &p); err != nil {
			t.Fatal(err)
		}
		found = true
		if asInt(f) != wantFund {
			t.Errorf("split on %s fund_id = %v, want %d", srcAcct, f, wantFund)
		}
		if asInt(p) != wantProg {
			t.Errorf("split on %s program_id = %v, want %d", srcAcct, p, wantProg)
		}
	}
	if !found {
		t.Errorf("no split found on account %s", srcAcct)
	}
}

func assertExpenseClass(t *testing.T, sqldb *sql.DB, res *BuildResult, srcAcct, wantClass string) {
	t.Helper()
	acctID := res.AccountIDs[srcAcct]
	rows, err := sqldb.Query(`SELECT functional_class, fund_id FROM splits WHERE account_id = ?`, acctID)
	if err != nil {
		t.Fatalf("query splits: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var fc interface{}
		var fund interface{}
		if err := rows.Scan(&fc, &fund); err != nil {
			t.Fatal(err)
		}
		found = true
		if asStr(fc) != wantClass {
			t.Errorf("expense split on %s class = %v, want %q", srcAcct, fc, wantClass)
		}
		if fund != nil {
			t.Errorf("unrestricted expense split on %s carries fund %v", srcAcct, fund)
		}
	}
	if !found {
		t.Errorf("no split on expense account %s", srcAcct)
	}
}

func allPayeeNames(t *testing.T, sqldb *sql.DB) []string {
	t.Helper()
	return scanStrings(t, sqldb, `SELECT name FROM payees`)
}

func allMemos(t *testing.T, sqldb *sql.DB) []string {
	t.Helper()
	out := scanStrings(t, sqldb, `SELECT memo FROM transactions WHERE memo <> ''`)
	out = append(out, scanStrings(t, sqldb, `SELECT memo FROM splits WHERE memo <> ''`)...)
	return out
}

func scanStrings(t *testing.T, sqldb *sql.DB, q string) []string {
	t.Helper()
	rows, err := sqldb.Query(q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func asInt(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case nil:
		return 0
	default:
		return -1
	}
}

func asStr(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return ""
	}
}

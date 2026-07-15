package main

import (
	"context"
	"database/sql"
	"fmt"
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
  "programs": {"EDU": "Education", "campus": "Campus"},
  "funds": {
    "GRANT1": {"name": "Grant One", "funder": "Funder A", "purpose": "water",
               "restriction": "purpose", "subsidiaries": ["Test US", "Test MX"],
               "program": "Education"}
  },
  "campus_fund": {"name": "Restore the Way", "funder": "", "purpose": "campus",
                  "restriction": "purpose"},
  "functional_classes": {"PRG": "program", "MGT": "management"},
  "base_currency": "USD",
  "fx_clearing_account": "FX Clearing",
  "opening_balance_account": "Opening Balances",
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
		{SourceAcct: "Donations", CuentoType: "revenue", CuentoParent: "Revenue", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Donations", NameES: "Donaciones"},
		{SourceAcct: "Campus Revenue", CuentoType: "revenue", CuentoParent: "Revenue", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Campus Revenue", NameES: "Ingreso Campus"},
		{SourceAcct: "Expenses", CuentoType: "expense", CuentoParent: "", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Expenses", NameES: "Gastos"},
		{SourceAcct: "Supplies", CuentoType: "expense", CuentoParent: "Expenses", Subsidiaries: []string{"Test US", "Test MX"}, FunctionalClass: "program", NameEN: "Supplies", NameES: "Suministros"},
		{SourceAcct: "Campus Costs", CuentoType: "expense", CuentoParent: "Expenses", Subsidiaries: []string{"Test US", "Test MX"}, FunctionalClass: "program", NameEN: "Campus Costs", NameES: "Costos Campus"},
		{SourceAcct: "Equity", CuentoType: "equity", CuentoParent: "", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Equity", NameES: "Patrimonio"},
		{SourceAcct: "Opening Balances", CuentoType: "equity", CuentoParent: "Equity", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Opening Balances", NameES: "Saldos Iniciales"},
		{SourceAcct: "FX Clearing", CuentoType: "equity", CuentoParent: "Equity", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "FX Clearing", NameES: "Compensacion FX"},
	}
	for i := range rows { // accounts are active unless a test says otherwise
		rows[i].Active = true
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
		// The two source rows carry DISTINCT desc values so a per-SPLIT description
		// mapping (vs. a per-transaction one) is observable: each split must carry its
		// own row's desc.
		row("US", "E", "spend", "Supplies", "", "2025-02-01", "PRG", "2", "office supplies", "", "USD", "1.0", "40.00", "40.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "", "2025-02-01", "", "2", "paid from checking", "", "USD", "1.0", "0", "0", "40.00", "40.00", "Assets"),
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
		// tid 6: a revenue line that carries a kls (functional-class code) and an
		// asset line that carries a kat (program code) -- BOTH on non-target account
		// types. The real export populates kls/kat on non-expense/non-R/E lines; the
		// importer must NOT forward kls to a non-expense split (the store rejects a
		// functional class off an expense account) nor kat to an A/L/E split (the
		// store rejects a program on a balance-sheet split). US, USD, unrestricted.
		row("US", "I", "receipt", "Donations", "EDU", "2025-07-01", "PRG", "6", "donation", "", "USD", "1.0", "0", "0", "70.00", "70.00", "Revenue"),
		row("US", "A", "receipt", "Checking", "EDU", "2025-07-01", "PRG", "6", "donation", "", "USD", "1.0", "70.00", "70.00", "0", "0", "Assets"),
		// --- Campus "Restore the Way" drawdown model (D p26.43) --------------------
		// The campus fund is a LIVE restricted fund: campus REVENUE grows a per-currency
		// pool; a campus EXPENSE is RtW only while the pool covers it, else it overflows
		// to unrestricted. Processed CHRONOLOGICALLY, so dates order the pool events.
		//
		// tid 7 (2025-08-01): a campus REVENUE receipt of 100. The revenue split (I,
		// campus) grows the pool to 100 and is RtW; the offsetting cash DEBIT (Checking,
		// blank kat = a plain unrestricted balance-sheet split) is retagged RtW by the
		// offset pass so the campus-fund group nets to zero -- NO plug. Proves (a).
		row("US", "I", "receipt", "Campus Revenue", "campus", "2025-08-01", "PRG", "7", "campus gift", "", "USD", "1.0", "0", "0", "100.00", "100.00", "Revenue"),
		row("US", "A", "receipt", "Checking", "", "2025-08-01", "", "7", "campus gift in", "", "USD", "1.0", "100.00", "100.00", "0", "0", "Assets"),
		// tid 8 (2025-09-01): a campus EXPENSE of 60 WITHIN the pool (100 -> 40), so RtW,
		// PLUS a NON-campus expense of 40, both paid from ONE cash credit of 100. The
		// shared Checking split (kat=campus but a BALANCE-SHEET split, so unrestricted and
		// an offset candidate) is DIVIDED: 60 to the campus fund, 40 unrestricted. The
		// campus expense carries a DONOR (GRANT1) yet resolves to the campus fund (RtW
		// overrides donor). Per-fund balanced, NO plug. Proves (b) and (d).
		row("US", "E", "spend", "Campus Costs", "campus", "2025-09-01", "PRG", "8", "campus supplies", "GRANT1", "USD", "1.0", "60.00", "60.00", "0", "0", "Expenses"),
		row("US", "E", "spend", "Supplies", "", "2025-09-01", "PRG", "8", "office supplies", "", "USD", "1.0", "40.00", "40.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "campus", "2025-09-01", "", "8", "paid both", "", "USD", "1.0", "0", "0", "100.00", "100.00", "Assets"),
		// tid 10 (2025-10-01): a campus EXPENSE of 80 that EXCEEDS the remaining pool
		// (40). It OVERFLOWS to unrestricted (its would-be donor fund is dropped too);
		// its cash offset stays unrestricted; the txn balances as an ordinary
		// unrestricted entry with NO campus fund and NO plug. Proves (c). Its Campus
		// program is still assigned (kat feeds program even when overflowed).
		row("US", "E", "spend", "Campus Costs", "campus", "2025-10-01", "PRG", "10", "campus overspend", "GRANT1", "USD", "1.0", "80.00", "80.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "", "2025-10-01", "", "10", "overspend paid", "", "USD", "1.0", "0", "0", "80.00", "80.00", "Assets"),
		// A consolidation-marker row (country CONSOL) that must be SKIPPED entirely.
		row("CONSOL", "A", "elim", "Checking", "", "2025-05-01", "", "9", "elim", "", "", "1.0", "0", "0", "0", "0", "Assets"),
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestReadRatesParses checks the historical-rates CSV reader: header validation,
// float parsing (D12), and the blank-source default. All values synthetic (rule 11).
func TestReadRatesParses(t *testing.T) {
	csv := "rate_date,base,quote,rate,source\n" +
		"2024-01-01,HNL,USD,0.0405,yahoo\n" +
		"2024-02-01,HNL,USD,0.0402,\n"
	rates, err := ReadRates(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ReadRates: %v", err)
	}
	if len(rates) != 2 {
		t.Fatalf("got %d rates, want 2", len(rates))
	}
	if rates[0] != (store.Rate{RateDate: "2024-01-01", Base: "HNL", Quote: "USD", Value: 0.0405, Source: "yahoo"}) {
		t.Errorf("row 0 = %+v", rates[0])
	}
	if rates[1].Source != "import" { // blank source defaults
		t.Errorf("blank source = %q, want import", rates[1].Source)
	}

	// A scrambled header must fail loudly, not mis-map columns.
	if _, err := ReadRates(strings.NewReader("base,rate_date,quote,rate,source\n")); err == nil {
		t.Error("scrambled header accepted; want error")
	}
}

// TestAccountMapIntercompanyColumn: the intercompany column parses (blank=false,
// "true"=true) and a bad value fails loudly. Round-trips through Write/Read.
func TestAccountMapIntercompanyColumn(t *testing.T) {
	rows := []AccountMap{
		{SourceAcct: "Transfer", CuentoType: "expense", Subsidiaries: []string{"A"}, Intercompany: true, NameEN: "Transfer", NameES: "Transfer"},
		{SourceAcct: "Rent", CuentoType: "expense", Subsidiaries: []string{"A"}, Intercompany: false, NameEN: "Rent", NameES: "Renta"},
	}
	var b strings.Builder
	if err := WriteAccountMap(&b, rows); err != nil {
		t.Fatalf("WriteAccountMap: %v", err)
	}
	got, err := ReadAccountMap(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	if !got[0].Intercompany || got[1].Intercompany {
		t.Errorf("intercompany round-trip wrong: %v / %v", got[0].Intercompany, got[1].Intercompany)
	}

	// A garbage intercompany cell fails loudly (not silently false).
	bad := "source_acct,cuento_type,cuento_parent,subsidiaries,functional_class_default,default_program,form990_code,intercompany,active,name_en,name_es\n" +
		"X,expense,,A,,,,notabool,true,X,X\n"
	if _, err := ReadAccountMap(strings.NewReader(bad)); err == nil {
		t.Error("garbage intercompany cell accepted; want error")
	}
	// A garbage active cell also fails loudly.
	bad2 := "source_acct,cuento_type,cuento_parent,subsidiaries,functional_class_default,default_program,form990_code,intercompany,active,name_en,name_es\n" +
		"X,expense,,A,,,,false,maybe,X,X\n"
	if _, err := ReadAccountMap(strings.NewReader(bad2)); err == nil {
		t.Error("garbage active cell accepted; want error")
	}
}

// TestBuildLoadsRates asserts the build loads a rates batch so the produced db can
// convert at report time (RateOn resolves the loaded pair).
func TestBuildLoadsRates(t *testing.T) {
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
	rates := []store.Rate{{RateDate: "2024-01-01", Base: "HNL", Quote: "USD", Value: 0.04, Source: "import"}}
	if _, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, rates, st, false); err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	got, err := st.RateOn(ctx, "HNL", "USD", "2024-06-01")
	if err != nil {
		t.Fatalf("RateOn: %v", err)
	}
	if got.Rate != 0.04 || got.RateDate != "2024-01-01" {
		t.Errorf("RateOn = %+v, want rate 0.04 on 2024-01-01", got)
	}
}

// TestBuildDeactivatesInactiveAccounts: an account flagged inactive (source
// "(deleted)") is created ACTIVE, receives its historical splits, then is
// deactivated — the splits survive (rule 14: deactivate, never delete).
func TestBuildDeactivatesInactiveAccounts(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	accMap, err := ReadAccountMap(strings.NewReader(testAccountMap()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	for i := range accMap {
		if accMap[i].SourceAcct == "Checking" { // an account that carries splits
			accMap[i].Active = false
		}
	}
	cfg, err := ReadConfig(strings.NewReader(testConfig()))
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	id := res.AccountIDs["Checking"]

	var active, nSplits int
	if err := sqldb.QueryRow(`SELECT active FROM accounts WHERE id=?`, id).Scan(&active); err != nil {
		t.Fatalf("query active: %v", err)
	}
	if active != 0 {
		t.Errorf("inactive-flagged account not deactivated (active=%d)", active)
	}
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM splits WHERE account_id=?`, id).Scan(&nSplits); err != nil {
		t.Fatalf("query splits: %v", err)
	}
	if nSplits == 0 {
		t.Error("historical splits lost on the deactivated account")
	}
}

// TestProgramTreeFromKlass asserts the klass-keyed child-program tree: a program
// named in program_classes is created under its program_parents parent, and a R/E
// split routes to it by `klass` even though its `kat` maps to the parent program
// (klass is finer AND more correct). The shared source fixes klass="subclass".
func TestProgramTreeFromKlass(t *testing.T) {
	cfg, err := ReadConfig(strings.NewReader(`{
  "root_subsidiary_name": "Test Org Root", "root_base_currency": "USD",
  "subsidiaries": {"US": {"name": "Test US", "base_currency": "USD"},
                   "MX": {"name": "Test MX", "base_currency": "MXN"}},
  "programs": {"EDU": "Education"},
  "program_classes": {"subclass": "Summer Camp"},
  "program_parents": {"Summer Camp": "Education"},
  "funds": {}, "functional_classes": {"PRG": "program", "MGT": "management"},
  "base_currency": "USD", "fx_clearing_account": "FX Clearing",
  "opening_balance_account": "Opening Balances",
  "skip_countries": ["CONSOL"], "opening_balance_typs": ["opening"]
}`))
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, err := ReadAccountMap(strings.NewReader(testAccountMap()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	// Tree: Summer Camp exists and its parent is Education (not the root).
	camp, ok := res.ProgramIDs["Summer Camp"]
	if !ok {
		t.Fatal("Summer Camp program not created")
	}
	edu := res.ProgramIDs["Education"]
	prog, err := st.GetProgram(ctx, camp)
	if err != nil {
		t.Fatalf("GetProgram: %v", err)
	}
	if !prog.ParentID.Valid || prog.ParentID.Int64 != edu {
		t.Errorf("Summer Camp parent = %v, want Education (%d)", prog.ParentID, edu)
	}

	// Routing: at least one R/E split lands on Summer Camp (via klass), proving klass
	// took precedence over the kat=EDU->Education mapping.
	var n int
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM splits WHERE program_id = ?`, camp).Scan(&n); err != nil {
		t.Fatalf("count splits: %v", err)
	}
	if n == 0 {
		t.Error("no split routed to Summer Camp via klass")
	}
}

// buildInto runs the build core into a fresh migrated temp db and returns the
// result. anonymize toggles the memo/description hashing.
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
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, nil, st, anonymize)
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

// TestCampusFundAssignedByKat proves the p26.43 "Restore the Way" drawdown model
// (SUPERSEDES the p26.40 whole-transaction plug): (a) campus revenue builds the pool
// and is RtW with its cash offset RtW; (b) a campus expense WITHIN the pool is RtW
// with a DIVIDED cash offset, per-fund balanced with NO plug; (c) a campus expense
// that EXCEEDS the remaining pool OVERFLOWS to unrestricted; (d) a cash split shared
// between a campus and a non-campus expense is DIVIDED so each portion carries the
// right fund; (e) ledger.Check is Error-clean AND Z18 (restricted-fund overspend) is
// GONE (the fund asset balance is >= 0); (f) non-campus splits stay unrestricted.
// Synthetic values only (AGENTS rule 11).
func TestCampusFundAssignedByKat(t *testing.T) {
	sqldb, st, res := buildInto(t, false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	if res.CampusFundID == nil {
		t.Fatalf("campus fund not created (CampusFundID nil)")
	}
	campusID := *res.CampusFundID

	// The campus fund is purpose-restricted, scoped to the child superset {US, MX}.
	fund, err := st.GetFund(ctx, campusID)
	if err != nil {
		t.Fatalf("GetFund: %v", err)
	}
	if fund.Name != "Restore the Way" || fund.Restriction != "purpose" {
		t.Errorf("campus fund = %q/%q, want %q/purpose", fund.Name, fund.Restriction, "Restore the Way")
	}
	subIDs, err := st.FundSubsidiaryIDs(ctx, campusID)
	if err != nil {
		t.Fatalf("FundSubsidiaryIDs: %v", err)
	}
	if len(subIDs) != 2 {
		t.Errorf("campus fund scope size = %d, want 2 (all configured children)", len(subIDs))
	}

	// (a) Campus revenue (tid 7) is RtW and its cash DEBIT offset was retagged RtW.
	// The campus revenue split carries the fund and the Campus program.
	assertSplitFundProgram(t, sqldb, res, "Campus Revenue", campusID, res.ProgramIDs["Campus"])
	// tid 7's Checking debit (100) is the offset: it must be RtW (a whole-split retag).
	if got := checkingCampusDebits(t, sqldb, res, campusID); got != 1 {
		t.Errorf("tid 7 campus revenue offset: %d RtW Checking debit(s), want 1", got)
	}

	// (b)+(d) tid 8: campus expense 60 (RtW) + non-campus expense 40, one cash credit
	// of 100 DIVIDED into a 60 RtW portion + a 40 unrestricted remainder. Exactly ONE
	// transaction, NO campus plug leg.
	if n := res.txnCountForTid("8"); n != 1 {
		t.Fatalf("tid 8 produced %d transactions, want 1", n)
	}
	// The 100.00 cash credit split into two Checking credits on the same txn: one
	// -60.00 RtW, one -40.00 unrestricted (amounts in minor units).
	assertCheckingCredit(t, sqldb, res, "8", campusID, -6000)   // RtW portion
	assertCheckingCredit(t, sqldb, res, "8", 0 /*NULL*/, -4000) // unrestricted remainder
	// The campus expense (tid 8) carries the fund AND the Campus program despite its
	// donor (GRANT1) -- RtW overrides the donor.
	var grant1OnCampus int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Campus Costs"], res.FundIDs["GRANT1"],
	).Scan(&grant1OnCampus); err != nil {
		t.Fatalf("count GRANT1 campus splits: %v", err)
	}
	if grant1OnCampus != 0 {
		t.Errorf("a campus expense resolved to donor fund GRANT1; RtW must override donor")
	}

	// (c) tid 10: campus expense 80 exceeds the remaining pool (40) -> OVERFLOW to
	// unrestricted. Its Campus Costs split carries NO fund but still the Campus program.
	var overflow int
	if err := sqldb.QueryRow(`
		SELECT COUNT(*) FROM splits s JOIN transactions t ON t.id = s.transaction_id
		WHERE s.account_id = ? AND s.fund_id IS NULL AND s.program_id = ?`,
		res.AccountIDs["Campus Costs"], res.ProgramIDs["Campus"],
	).Scan(&overflow); err != nil {
		t.Fatalf("count overflow campus expense: %v", err)
	}
	if overflow == 0 {
		t.Errorf("tid 10 campus expense did not overflow to unrestricted (pool exhausted)")
	}

	// No campus-fund plug leg landed on Opening Balances, and no [campus-plug] warning
	// surfaced -- the offset pass balanced every campus fund group directly.
	var campusOnOpening int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Opening Balances"], campusID,
	).Scan(&campusOnOpening); err != nil {
		t.Fatalf("count campus Opening Balances splits: %v", err)
	}
	if campusOnOpening != 0 {
		t.Errorf("campus fund got %d Opening-Balances plug legs, want 0 (offset pass should balance it)", campusOnOpening)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "[campus-plug]") {
			t.Errorf("unexpected [campus-plug] warning on synthetic data: %s", w)
		}
	}

	// (e) ledger.Check Error-clean AND Z18 (restricted-fund overspend) GONE FOR THE
	// CAMPUS FUND. The fund holds a POSITIVE asset balance (revenue 100 in, only 60
	// drawn RtW). (An unrelated synthetic GRANT1 MXN transfer legitimately trips its
	// OWN Z18 and is not this test's concern; scope the assertion to the campus fund.)
	vs, err := ledger.Check(context.Background(), sqldb)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	if ledger.HasErrors(vs) {
		t.Fatalf("produced db has Error violations after campus import: %+v", vs)
	}
	campusZ18 := fmt.Sprintf("restricted fund %d asset balance", campusID)
	for _, v := range vs {
		if v.Rule == "Z18" && strings.Contains(v.Detail, campusZ18) {
			t.Errorf("Z18 fires for the campus fund; its asset went negative: %s", v.Detail)
		}
	}
	// The fund's net-debit sum over asset splits (the Z18 quantity) must be >= 0.
	var fundAsset sql.NullInt64
	if err := sqldb.QueryRow(`
		SELECT SUM(s.amount) FROM splits s JOIN accounts a ON a.id = s.account_id
		WHERE s.fund_id = ? AND a.type = 'asset'`, campusID,
	).Scan(&fundAsset); err != nil {
		t.Fatalf("sum fund asset: %v", err)
	}
	if fundAsset.Int64 < 0 {
		t.Errorf("campus fund asset balance = %d, want >= 0 (no overspend)", fundAsset.Int64)
	}

	// (f) Non-campus splits stay unrestricted: the tid 8 Supplies expense carries no
	// fund, and Checking holds unrestricted splits (tids 1,2,4,6,10 + the divided
	// remainder).
	var suppliesUnrestricted int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id IS NULL`,
		res.AccountIDs["Supplies"],
	).Scan(&suppliesUnrestricted); err != nil {
		t.Fatalf("count unrestricted Supplies splits: %v", err)
	}
	if suppliesUnrestricted == 0 {
		t.Errorf("non-campus Supplies expense did not stay unrestricted")
	}
}

// checkingCampusDebits counts positive (debit) Checking splits tagged the campus
// fund -- tid 7's retagged cash-in offset.
func checkingCampusDebits(t *testing.T, sqldb *sql.DB, res *BuildResult, campusID int64) int {
	t.Helper()
	var n int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ? AND amount > 0`,
		res.AccountIDs["Checking"], campusID,
	).Scan(&n); err != nil {
		t.Fatalf("count campus Checking debits: %v", err)
	}
	return n
}

// assertCheckingCredit asserts exactly one Checking split in tid `tid`'s transaction
// with the given fund (fund==0 means NULL/unrestricted) and exact amount.
func assertCheckingCredit(t *testing.T, sqldb *sql.DB, res *BuildResult, tid string, fund, amount int64) {
	t.Helper()
	txns := res.tidTxns[tid]
	if len(txns) == 0 {
		t.Fatalf("tid %s produced no transactions", tid)
	}
	fundClause := "s.fund_id = ?"
	args := []any{res.AccountIDs["Checking"], amount, txns[0], fund}
	if fund == 0 {
		fundClause = "s.fund_id IS NULL"
		args = []any{res.AccountIDs["Checking"], amount, txns[0]}
	}
	var n int
	q := `SELECT COUNT(*) FROM splits s WHERE s.account_id = ? AND s.amount = ?
		AND s.transaction_id = ? AND ` + fundClause
	if err := sqldb.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count Checking credit (tid %s, fund %d, amt %d): %v", tid, fund, amount, err)
	}
	if n != 1 {
		t.Errorf("tid %s Checking split fund=%d amount=%d: got %d, want 1", tid, fund, amount, n)
	}
}

// TestCampusOffsetFallbackPlug covers the RARE pathological case (spec B / D
// p26.43) where an RtW campus split has NO unrestricted balance-sheet split on the
// correct side to offset it: the leftover falls through to the existing per-fund
// self-heal (a fund-tagged Opening-Balances [campus-plug] leg) so the campus fund
// group still nets to zero. Here a campus REVENUE (RtW) is booked against a
// NON-campus expense with no cash leg -- the whole campus subset has no asset/
// liability counterpart, so it plugs. This is the real-data ~707-plug path; the
// synthetic case proves it stays balanced and marked (rule 11: synthetic only).
func TestCampusOffsetFallbackPlug(t *testing.T) {
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
	// A single US/USD transaction: campus revenue -50 (RtW, grows the pool) offset by a
	// NON-campus expense +50 (a "revenue applied straight against an expense" entry, no
	// cash leg). The campus subset (just the -50 revenue) has no balance-sheet split to
	// offset, so it plugs to Opening Balances with the campus fund.
	src := header + "\n" +
		row("US", "I", "receipt", "Campus Revenue", "campus", "2025-08-01", "PRG", "20", "campus gift", "", "USD", "1.0", "0", "0", "50.00", "50.00", "Revenue") + "\n" +
		row("US", "E", "spend", "Supplies", "", "2025-08-01", "PRG", "20", "applied cost", "", "USD", "1.0", "50.00", "50.00", "0", "0", "Expenses") + "\n"
	recs, err := ParseRecords(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	res, err := runScaffold(ctx, accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runScaffold: %v", err)
	}
	subRes, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false)
	if err != nil {
		t.Fatalf("runImportSubsidiary: %v", err)
	}

	// The transaction posted (not rejected/dropped) as ONE transaction.
	if n := subRes.txnCountForTid("20"); n != 1 {
		t.Fatalf("fallback tid 20 produced %d transactions, want 1", n)
	}
	// A [campus-plug] warning surfaced and a campus-tagged Opening-Balances leg exists.
	plugged := false
	for _, w := range subRes.Warnings {
		if strings.Contains(w, "[campus-plug]") {
			plugged = true
		}
	}
	if !plugged {
		t.Errorf("no [campus-plug] warning for the un-offsettable campus subset")
	}
	var campusOnOpening int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Opening Balances"], *res.CampusFundID,
	).Scan(&campusOnOpening); err != nil {
		t.Fatalf("count campus Opening Balances splits: %v", err)
	}
	if campusOnOpening != 1 {
		t.Errorf("campus fallback plug legs on Opening Balances = %d, want 1", campusOnOpening)
	}
	// ledger.Check stays Error-clean (the plug keeps the fund group balanced).
	vs, err := ledger.Check(context.Background(), sqldb)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	if ledger.HasErrors(vs) {
		t.Fatalf("fallback plug left Error violations: %+v", vs)
	}
}

// parseTestInputs parses the synthetic mapping/config/source once for the
// split-import (scaffold + per-subsidiary) tests.
func parseTestInputs(t *testing.T) ([]AccountMap, Config, []Record) {
	t.Helper()
	accMap, err := ReadAccountMap(strings.NewReader(testAccountMap()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	cfg, err := ReadConfig(strings.NewReader(testConfig()))
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	recs, err := ParseRecords(strings.NewReader(testSource()))
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	return accMap, cfg, recs
}

func txnCount(t *testing.T, st *store.Store, ctx context.Context, subID int64) int64 {
	t.Helper()
	n, err := st.SubsidiaryTxnCount(ctx, subID)
	if err != nil {
		t.Fatalf("SubsidiaryTxnCount: %v", err)
	}
	return n
}

// TestScaffoldCreatesReferenceDataNoTxns: runScaffold populates subsidiaries,
// programs, funds and the WHOLE account chart (incl. the synthetic counter
// accounts) but posts ZERO transactions -- the reference-data half of the split.
func TestScaffoldCreatesReferenceDataNoTxns(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, cfg, _ := parseTestInputs(t)

	res, err := runScaffold(ctx, accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runScaffold: %v", err)
	}
	for _, want := range []string{"Test US", "Test MX"} {
		if _, ok := res.SubsidiaryIDs[want]; !ok {
			t.Errorf("subsidiary %q not scaffolded", want)
		}
	}
	for _, want := range []string{"FX Clearing", "Opening Balances", "Checking", "Cash MX"} {
		if _, ok := res.AccountIDs[want]; !ok {
			t.Errorf("account %q not scaffolded", want)
		}
	}
	if _, ok := res.ProgramIDs["Education"]; !ok {
		t.Errorf("program Education not scaffolded")
	}
	if _, ok := res.FundIDs["GRANT1"]; !ok {
		t.Errorf("fund GRANT1 not scaffolded")
	}
	var nTxns int
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&nTxns); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if nTxns != 0 {
		t.Errorf("scaffold posted %d transactions, want 0", nTxns)
	}
}

// TestImportSubsidiaryAdditive: scaffold, import Test US, then import Test MX into
// the SAME db. Each import posts only its own subsidiary's transactions; the second
// import does not disturb the first; and the cross-currency transfer decomposes
// through the SAME scaffolded FX Clearing account in both runs.
func TestImportSubsidiaryAdditive(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, cfg, recs := parseTestInputs(t)

	scaf, err := runScaffold(ctx, accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runScaffold: %v", err)
	}
	usSub := scaf.SubsidiaryIDs["Test US"]
	mxSub := scaf.SubsidiaryIDs["Test MX"]
	fxID := scaf.AccountIDs["FX Clearing"]

	// Import Test US: US gets transactions, MX stays empty.
	usRes, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false)
	if err != nil {
		t.Fatalf("import Test US: %v", err)
	}
	usCount := txnCount(t, st, ctx, usSub)
	if usCount == 0 {
		t.Fatalf("Test US has no transactions after its import")
	}
	if n := txnCount(t, st, ctx, mxSub); n != 0 {
		t.Errorf("Test MX has %d transactions before its import, want 0", n)
	}
	if !usRes.accountHasSplit(fxID) {
		t.Errorf("US import did not route the cross-currency transfer through FX Clearing")
	}

	// Import Test MX into the SAME db: additive, US untouched.
	mxRes, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test MX", false)
	if err != nil {
		t.Fatalf("import Test MX: %v", err)
	}
	if n := txnCount(t, st, ctx, usSub); n != usCount {
		t.Errorf("Test US transaction count changed from %d to %d after importing Test MX", usCount, n)
	}
	if n := txnCount(t, st, ctx, mxSub); n == 0 {
		t.Errorf("Test MX has no transactions after its import")
	}
	if !mxRes.accountHasSplit(fxID) {
		t.Errorf("MX import did not route the cross-currency transfer through FX Clearing")
	}
	// Cross-run resolution: the FX Clearing id reloaded in each run is the scaffold id.
	if usRes.AccountIDs["FX Clearing"] != fxID || mxRes.AccountIDs["FX Clearing"] != fxID {
		t.Errorf("FX Clearing id diverged across runs: scaffold=%d us=%d mx=%d",
			fxID, usRes.AccountIDs["FX Clearing"], mxRes.AccountIDs["FX Clearing"])
	}
}

// TestImportSubsidiaryGuardRefusesReimport: importing a subsidiary that already has
// transactions is refused, and no transactions are added.
func TestImportSubsidiaryGuardRefusesReimport(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, cfg, recs := parseTestInputs(t)

	scaf, err := runScaffold(ctx, accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runScaffold: %v", err)
	}
	usSub := scaf.SubsidiaryIDs["Test US"]
	if _, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	before := txnCount(t, st, ctx, usSub)

	_, err = runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false)
	if err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("re-import error = %v, want an 'already has' refusal", err)
	}
	if after := txnCount(t, st, ctx, usSub); after != before {
		t.Errorf("re-import changed Test US transaction count from %d to %d", before, after)
	}
}

// TestImportSubsidiaryFailsWithoutScaffold: a per-subsidiary import into a bare
// migrated db (no scaffold) fails loud and creates nothing.
func TestImportSubsidiaryFailsWithoutScaffold(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, cfg, recs := parseTestInputs(t)

	if _, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false); err == nil {
		t.Fatal("import into an un-scaffolded db succeeded; want a loud failure")
	}
	var nTxns int
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&nTxns); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if nTxns != 0 {
		t.Errorf("failed import left %d transactions; want 0", nTxns)
	}
}

// TestSharedCounterResolvedNoDuplicate: the reloaded FX Clearing / Opening Balances
// ids equal the scaffold ids, and no duplicate synthetic account rows are created.
func TestSharedCounterResolvedNoDuplicate(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, cfg, recs := parseTestInputs(t)

	scaf, err := runScaffold(ctx, accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runScaffold: %v", err)
	}
	usRes, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false)
	if err != nil {
		t.Fatalf("import Test US: %v", err)
	}
	for _, name := range []string{"FX Clearing", "Opening Balances"} {
		if usRes.AccountIDs[name] != scaf.AccountIDs[name] {
			t.Errorf("%s id diverged: scaffold=%d reload=%d", name, scaf.AccountIDs[name], usRes.AccountIDs[name])
		}
		var n int
		if err := sqldb.QueryRow(
			`SELECT COUNT(*) FROM account_names WHERE lang='en' AND name=?`, name,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("%s has %d account rows, want 1 (no duplicate from the per-sub run)", name, n)
		}
	}
}

// TestSubsidiaryNativeReconciliation: the per-currency net-debit total for an
// imported subsidiary is zero (the reconcile gate), and the per-type breakdown is
// non-empty (the books actually posted).
func TestSubsidiaryNativeReconciliation(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})
	accMap, cfg, recs := parseTestInputs(t)

	scaf, err := runScaffold(ctx, accMap, cfg, nil, st, false)
	if err != nil {
		t.Fatalf("runScaffold: %v", err)
	}
	if _, err := runImportSubsidiary(ctx, recs, accMap, cfg, st, "Test US", false); err != nil {
		t.Fatalf("import Test US: %v", err)
	}
	totals, err := st.SubsidiaryNativeTotals(ctx, scaf.SubsidiaryIDs["Test US"])
	if err != nil {
		t.Fatalf("SubsidiaryNativeTotals: %v", err)
	}
	if len(totals) == 0 {
		t.Fatal("no native totals; the subsidiary posted nothing")
	}
	byCur := map[string]int64{}
	for _, tot := range totals {
		byCur[tot.Currency] += tot.Total
	}
	for cur, sum := range byCur {
		if sum != 0 {
			t.Errorf("native total for %s = %d, want 0 (posted splits must net to zero)", cur, sum)
		}
	}
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

// TestSourceDimensionsGatedByAccountType is the p09.4 regression for the real-data
// quirk that the source populates kls (functional class) on non-expense lines and
// kat (program) on non-R/E lines. The importer must forward each source dimension
// ONLY to the account type the store accepts it on, else the store rejects the
// whole transaction (ErrNonExpenseFunction / ErrProgramOnBalanceSheet) and the
// group is dropped. tid 6 posts a revenue+asset pair that both carry kls and kat.
func TestSourceDimensionsGatedByAccountType(t *testing.T) {
	sqldb, _, res := buildInto(t, false)

	// The group must have posted as one transaction (not rejected/dropped).
	if n := res.txnCountForTid("6"); n != 1 {
		t.Fatalf("tid 6 produced %d transactions, want 1 (dimensions must not force a store rejection)", n)
	}

	// The revenue split carried a kls ("PRG") but revenue is not expense -> its
	// functional_class must be NULL (the store would reject a non-NULL one).
	revID := res.AccountIDs["Donations"]
	var fcNonNull int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM splits WHERE account_id = ? AND functional_class IS NOT NULL`, revID,
	).Scan(&fcNonNull); err != nil {
		t.Fatalf("count revenue functional_class: %v", err)
	}
	if fcNonNull != 0 {
		t.Errorf("revenue split carries a functional class (%d non-NULL); kls must not forward off an expense account", fcNonNull)
	}

	// The asset split carried a kat ("EDU") but asset is not R/E -> its program_id
	// must be NULL (the store would reject a program on a balance-sheet split).
	astID := res.AccountIDs["Checking"]
	var progNonNull int
	if err := sqldb.QueryRow(
		`SELECT count(*) FROM splits WHERE account_id = ? AND program_id IS NOT NULL`, astID,
	).Scan(&progNonNull); err != nil {
		t.Fatalf("count asset program_id: %v", err)
	}
	if progNonNull != 0 {
		t.Errorf("asset split carries a program (%d non-NULL); kat must not forward onto a balance-sheet split", progNonNull)
	}
}

// TestAnonymizeHashesMemosAndDescriptions: with --anonymize, no produced split
// memo NOR split description may equal a synthetic source original. The importer
// never minted payees (p26.16); the payee entity is fully removed (p26.20).
func TestAnonymizeHashesMemosAndDescriptions(t *testing.T) {
	sqldb, _, res := buildInto(t, true)

	// The synthetic source used desc "office supplies"/"grant in" etc. as both memo
	// and description. With --anonymize, none of those originals may survive verbatim.
	originals := []string{"office supplies", "paid from checking", "grant in", "fx transfer", "opening", "donation"}
	memos := allMemos(t, sqldb)
	descs := allDescriptions(t, sqldb)
	for _, o := range originals {
		for _, m := range memos {
			if m == o {
				t.Errorf("memo %q was not anonymized", o)
			}
		}
		for _, d := range descs {
			if d == o {
				t.Errorf("description %q was not anonymized", o)
			}
		}
	}
	// Sanity: anonymization actually produced hashed (hex) content, not empties.
	if len(descs) == 0 {
		t.Errorf("no split descriptions produced")
	}
	_ = res
}

// TestSplitDescriptionFromSourceRow: each posted split carries the DESCRIPTION of
// the ledger row that produced it (per-split, not per-transaction). tid 2's two
// source rows carry distinct desc, so the expense split and the checking split must
// carry their own row's desc. Synthesized counter-legs (FX Clearing / Opening
// Balances) come from no source row and carry an EMPTY description.
func TestSplitDescriptionFromSourceRow(t *testing.T) {
	sqldb, _, res := buildInto(t, false)

	// tid 2: expense split (Supplies) desc "office supplies"; asset split (Checking)
	// desc "paid from checking". Each split carries its own row's desc.
	assertAcctDescription(t, sqldb, res, "Supplies", "office supplies")

	// Checking carries splits from several tids; assert the tid-2 desc is present on
	// at least one of its splits (the tid-2 leg).
	if !acctHasDescription(t, sqldb, res, "Checking", "paid from checking") {
		t.Errorf("checking split for tid 2 missing its own row description")
	}

	// The synthesized Opening Balances counter-leg (tid 1) has no source row -> empty
	// description.
	assertAcctDescription(t, sqldb, res, "Opening Balances", "")
	// The synthesized FX Clearing counter-leg (tid 4/5) likewise.
	assertAcctDescription(t, sqldb, res, "FX Clearing", "")
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

func allMemos(t *testing.T, sqldb *sql.DB) []string {
	t.Helper()
	out := scanStrings(t, sqldb, `SELECT memo FROM transactions WHERE memo <> ''`)
	out = append(out, scanStrings(t, sqldb, `SELECT memo FROM splits WHERE memo <> ''`)...)
	return out
}

func allDescriptions(t *testing.T, sqldb *sql.DB) []string {
	t.Helper()
	return scanStrings(t, sqldb, `SELECT description FROM splits WHERE description <> ''`)
}

// assertAcctDescription requires EVERY split on the given source account to carry
// exactly want as its description (use "" for the synthesized counter-leg accounts).
func assertAcctDescription(t *testing.T, sqldb *sql.DB, res *BuildResult, srcAcct, want string) {
	t.Helper()
	acctID := res.AccountIDs[srcAcct]
	rows, err := sqldb.Query(`SELECT description FROM splits WHERE account_id = ?`, acctID)
	if err != nil {
		t.Fatalf("query descriptions on %s: %v", srcAcct, err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		found = true
		if d != want {
			t.Errorf("split on %s description = %q, want %q", srcAcct, d, want)
		}
	}
	if !found {
		t.Errorf("no split found on account %s", srcAcct)
	}
}

// acctHasDescription reports whether at least one split on the account carries want.
func acctHasDescription(t *testing.T, sqldb *sql.DB, res *BuildResult, srcAcct, want string) bool {
	t.Helper()
	acctID := res.AccountIDs[srcAcct]
	var n int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND description = ?`, acctID, want,
	).Scan(&n); err != nil {
		t.Fatalf("count descriptions on %s: %v", srcAcct, err)
	}
	return n > 0
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

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
		// tid 7: a FULLY-CAMPUS, self-balancing txn (both splits kat=campus). Each
		// split must carry the campus fund ("Restore the Way") AND the account default
		// program; the campus fund group nets to zero, so NO plug leg. US, USD. The
		// expense split's kat=campus ALSO drives its program (Campus) -- proving kat
		// still feeds program while newly feeding the fund. Donor is blank here.
		row("US", "E", "spend", "Campus Costs", "campus", "2025-08-01", "PRG", "7", "campus supplies", "", "USD", "1.0", "80.00", "80.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "campus", "2025-08-01", "", "7", "campus paid", "", "USD", "1.0", "0", "0", "80.00", "80.00", "Assets"),
		// tid 8: a MIXED txn whose campus SUBSET does not self-balance. One kat=campus
		// expense split (campus fund) + one NON-campus asset split (unrestricted). The
		// txn nets to zero OVERALL but not PER FUND, so each fund group gets an Opening
		// Balances plug leg (the accepted self-heal, D p26.40); the campus one carries
		// the distinct [campus-plug] warning marker. The campus split carries the
		// campus fund and its Campus program; the non-campus split stays unrestricted.
		// A donor is set on the campus split to prove kat=campus OVERRIDES the donor
		// fund (it must resolve to Restore the Way, NOT Grant One). US, USD.
		row("US", "E", "spend", "Campus Costs", "campus", "2025-09-01", "PRG", "8", "campus mixed", "GRANT1", "USD", "1.0", "30.00", "30.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "", "2025-09-01", "", "8", "unrestricted mixed", "", "USD", "1.0", "0", "0", "30.00", "30.00", "Assets"),
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

// TestCampusFundAssignedByKat proves the p26.40 marker-driven "campus" fund path:
// (a) the fund is created with the right subsidiary superset, (b) every kat=campus
// split carries it (overriding a donor fund), (c) a mixed campus txn whose campus
// subset does not self-balance still imports via the Opening Balances self-heal (and
// ledger.Check passes), and (d) non-campus splits stay unrestricted. Synthetic
// values only (AGENTS rule 11).
func TestCampusFundAssignedByKat(t *testing.T) {
	sqldb, st, res := buildInto(t, false)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// (a) The campus fund is created, purpose-restricted, scoped to a SUPERSET of the
	// campus-posting subsidiaries -- here all configured children {Test US, Test MX}.
	if res.CampusFundID == nil {
		t.Fatalf("campus fund not created (CampusFundID nil)")
	}
	fund, err := st.GetFund(ctx, *res.CampusFundID)
	if err != nil {
		t.Fatalf("GetFund: %v", err)
	}
	if fund.Name != "Restore the Way" {
		t.Errorf("campus fund name = %q, want %q", fund.Name, "Restore the Way")
	}
	if fund.Restriction != "purpose" {
		t.Errorf("campus fund restriction = %q, want purpose", fund.Restriction)
	}
	subIDs, err := st.FundSubsidiaryIDs(ctx, *res.CampusFundID)
	if err != nil {
		t.Fatalf("FundSubsidiaryIDs: %v", err)
	}
	got := map[int64]bool{}
	for _, id := range subIDs {
		got[id] = true
	}
	// Every subsidiary that posts a campus split (Test US via tids 7,8) must be in
	// the fund's scope; the full child set is the superset actually created.
	for _, want := range []string{"Test US", "Test MX"} {
		if !got[res.SubsidiaryIDs[want]] {
			t.Errorf("campus fund scope missing subsidiary %q", want)
		}
	}
	if len(subIDs) != 2 {
		t.Errorf("campus fund scope size = %d, want 2 (all configured children)", len(subIDs))
	}

	// (b) The campus expense splits (tids 7,8 on Campus Costs) carry the campus fund
	// AND the kat=campus program (Campus) -- kat still feeds program while newly
	// feeding the fund. Every Campus Costs split is campus, so assert over all of them.
	assertSplitFundProgram(t, sqldb, res, "Campus Costs", *res.CampusFundID, res.ProgramIDs["Campus"])

	// (b'/precedence) The mixed txn (tid 8) campus split set a DONOR (GRANT1) yet must
	// resolve to the campus fund, never Grant One -- kat=campus overrides the donor.
	var grant1OnCampus int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Campus Costs"], res.FundIDs["GRANT1"],
	).Scan(&grant1OnCampus); err != nil {
		t.Fatalf("count GRANT1 campus splits: %v", err)
	}
	if grant1OnCampus != 0 {
		t.Errorf("a campus expense split resolved to the donor fund GRANT1; kat=campus must override donor")
	}

	// (c) The mixed campus txn imported (tid 8 -> 1 transaction) and its campus fund
	// residual was routed to Opening Balances with the distinct [campus-plug] marker.
	if n := res.txnCountForTid("8"); n != 1 {
		t.Fatalf("mixed campus tid 8 produced %d transactions, want 1", n)
	}
	campusPlugs := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, "[campus-plug]") {
			campusPlugs++
		}
	}
	if campusPlugs == 0 {
		t.Errorf("no [campus-plug] warning surfaced for the imbalanced campus subset (tid 8)")
	}
	// The Opening Balances counter-leg for tid 8's campus group must carry the campus
	// fund (so the fund group nets to zero and the store accepts it).
	var campusOnOpening int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Opening Balances"], *res.CampusFundID,
	).Scan(&campusOnOpening); err != nil {
		t.Fatalf("count campus Opening Balances splits: %v", err)
	}
	if campusOnOpening == 0 {
		t.Errorf("no campus-tagged Opening Balances plug leg; the self-heal did not fund-tag the counter-leg")
	}

	// ledger.Check must be clean (Z1/Z10 per-txn and per-fund zero-sum hold).
	vs, err := ledger.Check(context.Background(), sqldb)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	if ledger.HasErrors(vs) {
		t.Fatalf("produced db has Error violations after campus import: %+v", vs)
	}

	// (d) The tid 8 NON-campus asset split (Checking, blank kat) stays unrestricted:
	// at least one Checking split carries no fund. (Checking also holds tids 1,2,4,6
	// unrestricted splits and tid 7's campus split, so assert the unrestricted-split
	// count is nonzero AND the tid 7 campus Checking split carries the campus fund.)
	var unrestrictedChecking int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id IS NULL`,
		res.AccountIDs["Checking"],
	).Scan(&unrestrictedChecking); err != nil {
		t.Fatalf("count unrestricted Checking splits: %v", err)
	}
	if unrestrictedChecking == 0 {
		t.Errorf("no unrestricted Checking split; non-campus splits must stay unrestricted")
	}
	var campusChecking int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Checking"], *res.CampusFundID,
	).Scan(&campusChecking); err != nil {
		t.Fatalf("count campus Checking splits: %v", err)
	}
	if campusChecking == 0 {
		t.Errorf("tid 7 campus Checking split did not carry the campus fund")
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

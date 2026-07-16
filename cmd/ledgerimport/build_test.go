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
  "campus_asset_accounts": ["Campus Land", "Campus Building"],
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
		{SourceAcct: "Campus Land", CuentoType: "asset", CuentoParent: "Assets", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Campus Land", NameES: "Terreno Campus"},
		{SourceAcct: "Campus Building", CuentoType: "asset", CuentoParent: "Assets", Subsidiaries: []string{"Test US", "Test MX"}, NameEN: "Campus Building", NameES: "Edificio Campus"},
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
		// tid 11 (2025-08-15, BETWEEN the campus revenue and the campus expense): a
		// campus CAPITAL purchase Dr Campus Land 1000 / Cr Checking 1000. Campus Land is
		// an ACCOUNT-DRIVEN campus marker (D p26.46), a FIXED ASSET with blank kat -- the
		// isRE-guarded kat/pool path would skip it, but the account marker tags it RtW.
		// It carries a DONOR (GRANT1) to prove the marker overrides the donor. Because it
		// is an asset SWAP it is NOT a pool event: the pool stays 100 after Aug, so the
		// Sep campus expense (tid 8, 60) is still WITHIN the pool and RtW -- proving the
		// 1000 asset did NOT drain the pool. Its Checking offset (nil fund) is retagged
		// RtW by Pass-2 so the campus subset nets to zero with NO plug.
		row("US", "A", "buy", "Campus Land", "", "2025-08-15", "", "11", "campus land", "GRANT1", "USD", "1.0", "1000.00", "1000.00", "0", "0", "Assets"),
		row("US", "A", "buy", "Checking", "", "2025-08-15", "", "11", "paid for land", "", "USD", "1.0", "0", "0", "1000.00", "1000.00", "Assets"),
		// --- FX-normalized drawdown pool (D p26.47): cross-currency funding ----------
		// The pre-p26.47 pool was PER CURRENCY, so USD donations could not fund a
		// non-USD (MXN) campus cost and vice versa. These tids prove the FX-NORMALIZED
		// (single USD) pool lets a NON-USD campus revenue fund a later USD campus expense
		// that NO per-currency USD pool could ever cover.
		//
		// tid 12 (2025-11-01): a campus REVENUE receipt of MXN 4000 in the MX
		// subsidiary. Converted at the seeded MXN->USD rate 0.05 it grows the SINGLE USD
		// pool by USD 200 (4000 * 0.05). Its Cash MX debit offset is retagged RtW.
		// COLUMN CONVENTION (p26.56): db/cr carry the BASE (USD) amount and fdb/fcr the
		// NATIVE (MXN) amount -- matching the real export (db/cr = org functional, fdb/fcr
		// = foreign). So the stored MXN native comes from fdb/fcr (4000), and the pool
		// decision converts that 4000 MXN * 0.05 = 200 USD. (The old importer read db/cr,
		// storing 200 as if native -- the corruption this step fixes.)
		row("MX", "I", "receipt", "Campus Revenue", "campus", "2025-11-01", "PRG", "12", "mx campus gift", "", "MXN", "0.05", "0", "0", "200.00", "4000.00", "Revenue"),
		row("MX", "A", "receipt", "Cash MX", "", "2025-11-01", "", "12", "mx campus gift in", "", "MXN", "0.05", "200.00", "4000.00", "0", "0", "Assets"),
		// tid 13 (2025-12-01): a USD campus EXPENSE of 200. At this date USD-only campus
		// revenue AVAILABLE in the pool is just 40 (tid 7's 100 less tid 8's 60; tid 10
		// overflowed and drew nothing), so the OLD per-currency USD pool (40 < 200) would
		// OVERFLOW this to unrestricted. Under the FX-normalized single pool the MXN
		// revenue's USD 200 (tid 12) lifted the pool to 240, so this expense is RtW -- the
		// cross-currency BRIDGE. Proves fix (a). Its Checking offset is retagged RtW. The
		// USD asset column now dips to -160 (100 - 60 - 200); tid 15 below restores it.
		row("US", "E", "spend", "Campus Costs", "campus", "2025-12-01", "PRG", "13", "us campus paid by mx gift", "", "USD", "1.0", "200.00", "200.00", "0", "0", "Expenses"),
		row("US", "A", "spend", "Checking", "", "2025-12-01", "", "13", "paid us campus", "", "USD", "1.0", "0", "0", "200.00", "200.00", "Assets"),
		// tid 15 (2026-01-01): a LATER USD campus REVENUE of 200 -- the domestic revenue
		// that the tid-13 expense anticipated. It brings the USD asset column back to +40
		// (>= 0), so per-currency Z18 stays clean even though the MXN bridge funded tid 13
		// ahead of this receipt. This is the timing-bridge shape (foreign revenue covers a
		// domestic expense at spend time; domestic revenue arrives later), NOT a permanent
		// cross-currency subsidy. Its Checking debit offset is retagged RtW.
		row("US", "I", "receipt", "Campus Revenue", "campus", "2026-01-01", "PRG", "15", "later us campus gift", "", "USD", "1.0", "0", "0", "200.00", "200.00", "Revenue"),
		row("US", "A", "receipt", "Checking", "", "2026-01-01", "", "15", "later campus gift in", "", "USD", "1.0", "200.00", "200.00", "0", "0", "Assets"),
		// tid 14 (2025-08-20): a SECOND account-driven campus-asset purchase (D p26.46
		// widened set, D p26.47 Fix 2): Dr Campus Building 500 / Cr Checking 500. Campus
		// Building is a marker fixed-asset account (a `1670`-style building line). Like
		// Campus Land it is tagged RtW directly (asset swap, NOT a pool event, so it does
		// NOT drain the drawdown pool), with its Checking offset retagged RtW -- one
		// balanced transaction, no plug.
		row("US", "A", "buy", "Campus Building", "", "2025-08-20", "", "14", "campus building", "", "USD", "1.0", "500.00", "500.00", "0", "0", "Assets"),
		row("US", "A", "buy", "Checking", "", "2025-08-20", "", "14", "paid for building", "", "USD", "1.0", "0", "0", "500.00", "500.00", "Assets"),
		// A consolidation-marker row (country CONSOL) that must be SKIPPED entirely.
		row("CONSOL", "A", "elim", "Checking", "", "2025-05-01", "", "9", "elim", "", "", "1.0", "0", "0", "0", "0", "Assets"),
	}
	return strings.Join(lines, "\n") + "\n"
}

// testRates returns the synthetic FX rates the build needs (all values invented,
// rule 11). The MXN->USD row lets the FX-normalized campus drawdown pool (D p26.47)
// convert testSource's non-USD (MXN) campus revenue into the single USD pool; without
// it, RateOn returns ErrRateMissing when the pool converts that split. Every all-USD
// campus tid short-circuits (currency == base) and never hits RateOn, so this rate
// affects only the cross-currency campus path.
func testRates() []store.Rate {
	return []store.Rate{{RateDate: "2025-01-01", Base: "MXN", Quote: "USD", Value: 0.05, Source: "test"}}
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
	rates := append(testRates(),
		store.Rate{RateDate: "2024-01-01", Base: "HNL", Quote: "USD", Value: 0.04, Source: "import"})
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
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, testRates(), st, false)
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
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, testRates(), st, false)
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
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, testRates(), st, anonymize)
	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	return sqldb, st, res
}

// TestNativeNetDebitSelectsColumnPairByCurrency is the p26.56 fix's unit proof: a
// split's stored native amount comes from db/cr when the split is in the org BASE
// currency and from fdb/fcr otherwise. In the real export db/cr always carry the
// BASE (USD) figure and fdb/fcr the FOREIGN native figure -- and the two DIFFER on
// a large fraction of BOTH USD splits (their foreign counterpart) and foreign
// splits (their USD counterpart) -- so a uniform column choice corrupts one side.
// This test uses db != fdb to prove the branch, which db==fdb fixtures cannot.
func TestNativeNetDebitSelectsColumnPairByCurrency(t *testing.T) {
	const base = "USD"
	// A non-USD (MXN, exp 2) split: db/cr = USD 200, fdb/fcr = MXN 4000 native. The
	// stored native must be the MXN 4000 (from fdb/fcr), NOT the USD 200 (db/cr).
	mxn := Record{Currency: "MXN", Db: "0", Cr: "200.00", Fdb: "4000.00", Fcr: "0"}
	got, err := nativeNetDebit(mxn, 2, base)
	if err != nil {
		t.Fatalf("nativeNetDebit(MXN): %v", err)
	}
	if got != 400000 { // 4000.00 MXN native, a DEBIT (fdb) -> positive net-debit
		t.Errorf("MXN native = %d, want 400000 (from fdb/fcr, not the USD db/cr 20000)", got)
	}
	// A USD (base) split whose fdb/fcr carry an HNL-style ~24x foreign counterpart:
	// the native must come from db/cr (the USD figure), NOT fdb/fcr.
	usd := Record{Currency: "USD", Db: "10.00", Cr: "0", Fdb: "242.61", Fcr: "0"}
	got, err = nativeNetDebit(usd, 2, base)
	if err != nil {
		t.Fatalf("nativeNetDebit(USD): %v", err)
	}
	if got != 1000 { // 10.00 USD native, from db/cr (uniform fdb would give 24261)
		t.Errorf("USD native = %d, want 1000 (from db/cr, not the foreign fdb 24261)", got)
	}
}

// TestNativeNetDebitReverseConventionBaseRow is the p26.67 guard's unit proof. A tiny
// set of USD rows (Caja Moneda Extranjera) VIOLATE the base invariant: their db/cr hold
// the FOREIGN (lempira) figure and their fdb/fcr hold the true USD, backward from every
// other base row. The tell is xrt != 1 with the base pair the xrt-SCALED (larger)
// magnitude: |net(db,cr)| == |net(fdb,fcr)| * xrt. The guard must take native from
// fdb/fcr on such a row, while leaving BOTH a normal base row (db==fdb, xrt=1) AND a
// normal USD row (db/cr the SMALLER USD side, fdb/fcr the larger scaled counterpart)
// on the db/cr path.
func TestNativeNetDebitReverseConventionBaseRow(t *testing.T) {
	const base = "USD"

	// Reverse-convention USD row: db/cr = HNL 2480.00 (the ~24.8x-scaled figure),
	// fdb/fcr = USD 100.00 (the true USD), xrt = 24.80. Native must be the USD 100.00
	// from fdb/fcr -- NOT the lempira 2480.00 from db/cr.
	reverse := Record{Currency: "USD", Xrt: "24.80", Db: "2480.00", Cr: "0", Fdb: "100.00", Fcr: "0"}
	got, err := nativeNetDebit(reverse, 2, base)
	if err != nil {
		t.Fatalf("nativeNetDebit(reverse): %v", err)
	}
	if got != 10000 { // 100.00 USD from fdb/fcr, not 248000 from db/cr
		t.Errorf("reverse-convention native = %d, want 10000 (from fdb/fcr, not db/cr 248000)", got)
	}

	// A NORMAL base row: db==fdb, xrt=1. Guard must NOT fire (stays on db/cr).
	normalBase := Record{Currency: "USD", Xrt: "1.0", Db: "50.00", Cr: "0", Fdb: "50.00", Fcr: "0"}
	got, err = nativeNetDebit(normalBase, 2, base)
	if err != nil {
		t.Fatalf("nativeNetDebit(normalBase): %v", err)
	}
	if got != 5000 {
		t.Errorf("normal base native = %d, want 5000 (unchanged, from db/cr)", got)
	}

	// A NORMAL USD row with a foreign HNL counterpart: db/cr = USD 100.00 (the SMALLER
	// USD side), fdb/fcr = HNL 2480.00 (the larger scaled counterpart), xrt = 24.80.
	// The guard must NOT fire -- db/cr already holds the correct USD, and the base side
	// is the smaller magnitude (the discriminator's |base| > |foreign| clause fails).
	normalUSD := Record{Currency: "USD", Xrt: "24.80", Db: "100.00", Cr: "0", Fdb: "2480.00", Fcr: "0"}
	got, err = nativeNetDebit(normalUSD, 2, base)
	if err != nil {
		t.Fatalf("nativeNetDebit(normalUSD): %v", err)
	}
	if got != 10000 { // 100.00 USD from db/cr, unchanged
		t.Errorf("normal USD native = %d, want 10000 (from db/cr, guard must not fire)", got)
	}

	// Both a reverse row and a normal base row must BALANCE against their credit twin
	// (the store's per-transaction zero-sum). A reverse credit twin uses fdb/fcr too.
	revCredit := Record{Currency: "USD", Xrt: "24.80", Db: "0", Cr: "2480.00", Fdb: "0", Fcr: "100.00"}
	rc, err := nativeNetDebit(revCredit, 2, base)
	if err != nil {
		t.Fatalf("nativeNetDebit(revCredit): %v", err)
	}
	if got, _ := nativeNetDebit(reverse, 2, base); got+rc != 0 {
		t.Errorf("reverse debit %d + reverse credit %d != 0 (unbalanced)", got, rc)
	}
}

// TestForeignSplitStoresNativeAndConvertsForReport is the p26.56 fix's end-to-end
// proof on the synthetic build: the MXN campus-revenue split (tid 12, db/cr = USD
// 200, fdb/fcr = MXN 4000) must STORE the MXN 4000 native (from fdb/fcr, not the
// USD 200), and the drawdown-pool DECISION converts that native back to USD 200
// (4000 * 0.05) -- shown by tid 13's USD 200 campus expense being funded (RtW). A
// single-currency non-USD (MXN) transaction must still balance (the store rejects
// otherwise). Under the OLD importer the MXN split stored USD 200 as if native, and
// a report converting MXN->USD would shrink it ~20x -- the corruption this fixes.
func TestForeignSplitStoresNativeAndConvertsForReport(t *testing.T) {
	sqldb, _, res := buildInto(t, false)
	ctx := context.Background()

	// The tid 12 MXN campus-revenue split stores 4000.00 MXN native (400000 minor,
	// a credit -> negative net-debit), NOT the USD 200 (which db/cr held).
	var mxnNative int64
	if err := sqldb.QueryRow(`
		SELECT s.amount FROM splits s JOIN transactions t ON t.id = s.transaction_id
		WHERE s.account_id = ? AND t.currency = 'MXN' AND s.amount < 0
		  AND t.id IN (SELECT transaction_id FROM splits WHERE description = 'mx campus gift')`,
		res.AccountIDs["Campus Revenue"],
	).Scan(&mxnNative); err != nil {
		t.Fatalf("query tid 12 MXN campus revenue native: %v", err)
	}
	if mxnNative != -400000 {
		t.Errorf("tid 12 MXN campus revenue stored native = %d, want -400000 (4000.00 MXN from fdb/fcr, not USD 200)", mxnNative)
	}

	// The converted-to-USD pool decision (4000 MXN * 0.05 = 200 USD) funded tid 13's
	// USD 200 campus expense as RtW -- proving the report-basis conversion is correct.
	campusID := *res.CampusFundID
	var tid13RtW int
	if err := sqldb.QueryRow(`
		SELECT COUNT(*) FROM splits s JOIN transactions t ON t.id = s.transaction_id
		WHERE s.account_id = ? AND s.fund_id = ? AND s.amount = 20000
		  AND t.id IN (SELECT transaction_id FROM splits WHERE description = 'us campus paid by mx gift')`,
		res.AccountIDs["Campus Costs"], campusID,
	).Scan(&tid13RtW); err != nil {
		t.Fatalf("query tid 13 RtW: %v", err)
	}
	if tid13RtW != 1 {
		t.Errorf("tid 13 USD campus expense RtW = %d, want 1 (MXN native must convert to USD 200 in the pool)", tid13RtW)
	}

	// A single-currency non-USD (MXN) transaction still balances: tid 3 (MXN grant
	// receipt) posted, and the store enforces per-transaction zero-sum on write, so
	// its very existence proves balance. Assert its two MXN splits net to zero.
	var mxnTxnSum sql.NullInt64
	if err := sqldb.QueryRow(`
		SELECT SUM(s.amount) FROM splits s JOIN transactions t ON t.id = s.transaction_id
		WHERE t.currency = 'MXN'
		  AND t.id IN (SELECT transaction_id FROM splits WHERE description = 'grant in')`,
	).Scan(&mxnTxnSum); err != nil {
		t.Fatalf("sum tid 3 MXN splits: %v", err)
	}
	if !mxnTxnSum.Valid || mxnTxnSum.Int64 != 0 {
		t.Errorf("single-currency MXN transaction net-debit sum = %v, want 0 (must balance)", mxnTxnSum)
	}
	_ = ctx
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
	assertCheckingCredit(t, sqldb, res, "7", campusID, 10000)

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

	// (b2) tid 11: account-driven campus-asset purchase (D p26.46). Campus Land is a
	// FIXED-ASSET marker account, so its split joins the campus fund despite blank kat
	// and a GRANT1 donor (marker overrides donor), and its Checking offset is retagged
	// RtW -- one balanced transaction, no plug. It is an asset SWAP, not a pool event:
	// the campus expense (tid 8, above) is still RtW, proving the 1000 asset did NOT
	// drain the pool.
	if n := res.txnCountForTid("11"); n != 1 {
		t.Fatalf("tid 11 produced %d transactions, want 1", n)
	}
	// The Campus Land split carries the campus fund (+1000, a debit), never GRANT1.
	var landCampus, landDonor int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ? AND amount = 100000`,
		res.AccountIDs["Campus Land"], campusID,
	).Scan(&landCampus); err != nil {
		t.Fatalf("count campus Land splits: %v", err)
	}
	if landCampus != 1 {
		t.Errorf("Campus Land split: %d RtW debits, want 1 (account-driven marker)", landCampus)
	}
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ?`,
		res.AccountIDs["Campus Land"], res.FundIDs["GRANT1"],
	).Scan(&landDonor); err != nil {
		t.Fatalf("count GRANT1 Land splits: %v", err)
	}
	if landDonor != 0 {
		t.Errorf("Campus Land resolved to donor GRANT1; the campus-asset marker must override the donor")
	}
	// The 1000 Checking credit offsetting the land purchase is retagged RtW (whole
	// split), so tid 11's campus subset nets to zero with no plug.
	assertCheckingCredit(t, sqldb, res, "11", campusID, -100000)

	// (b3) tid 14: a SECOND account-driven campus-asset marker (D p26.47 Fix 2, the
	// widened campus fixed-asset set). Campus Building (a `1670`-style building line)
	// is tagged RtW just like Campus Land, its Checking offset retagged RtW, one
	// balanced transaction, no plug -- proving the marker set is not special-cased to a
	// single account and an asset swap does not drain the pool.
	if n := res.txnCountForTid("14"); n != 1 {
		t.Fatalf("tid 14 produced %d transactions, want 1", n)
	}
	var buildingCampus int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ? AND amount = 50000`,
		res.AccountIDs["Campus Building"], campusID,
	).Scan(&buildingCampus); err != nil {
		t.Fatalf("count campus Building splits: %v", err)
	}
	if buildingCampus != 1 {
		t.Errorf("Campus Building split: %d RtW debits, want 1 (account-driven marker)", buildingCampus)
	}
	assertCheckingCredit(t, sqldb, res, "14", campusID, -50000)

	// (a2) FX-NORMALIZED POOL (D p26.47): a NON-USD campus revenue funds a later USD
	// campus expense the OLD per-currency pool could never cover. tid 12 (MXN 4000
	// campus revenue, USD 200 at rate 0.05) grows the SINGLE USD pool; tid 13 (USD 200
	// campus expense) exceeds ALL USD campus revenue (only 100, tid 7) yet is RtW
	// because the converted MXN revenue lifted the USD pool to cover it. Under the old
	// per-currency pool this expense would have OVERFLOWED. The tid 13 Campus Costs
	// split carries the campus fund (not NULL), and its Checking offset is retagged RtW.
	var tid13Campus int
	if err := sqldb.QueryRow(`
		SELECT COUNT(*) FROM splits s JOIN transactions t ON t.id = s.transaction_id
		WHERE s.account_id = ? AND s.fund_id = ? AND s.amount = 20000
		  AND t.id IN (SELECT transaction_id FROM splits WHERE description = 'us campus paid by mx gift')`,
		res.AccountIDs["Campus Costs"], campusID,
	).Scan(&tid13Campus); err != nil {
		t.Fatalf("count tid 13 campus expense: %v", err)
	}
	if tid13Campus != 1 {
		t.Errorf("tid 13 USD campus expense is %d RtW splits, want 1 (FX-normalized pool must fund it via MXN revenue)", tid13Campus)
	}
	// The tid 12 MXN campus revenue is RtW and its Cash MX debit offset retagged RtW.
	var mxRevCampus int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM splits WHERE account_id = ? AND fund_id = ? AND amount < 0`,
		res.AccountIDs["Campus Revenue"], campusID,
	).Scan(&mxRevCampus); err != nil {
		t.Fatalf("count MXN campus revenue: %v", err)
	}
	if mxRevCampus == 0 {
		t.Errorf("tid 12 MXN campus revenue not RtW")
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
	res, err := runScaffold(ctx, accMap, cfg, testRates(), st, false)
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

	res, err := runScaffold(ctx, accMap, cfg, testRates(), st, false)
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

	scaf, err := runScaffold(ctx, accMap, cfg, testRates(), st, false)
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

	scaf, err := runScaffold(ctx, accMap, cfg, testRates(), st, false)
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

	scaf, err := runScaffold(ctx, accMap, cfg, testRates(), st, false)
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

	scaf, err := runScaffold(ctx, accMap, cfg, testRates(), st, false)
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

// configWithCorrections returns testConfig with a `corrections` list appended: one
// balanced manual adjustment (D p26.72) posting a due-to/from cutoff pair in USD on
// the US subsidiary -- a Dr asset (Checking) / Cr revenue (Donations) entry of an
// exact integer amount, restricted to the GRANT1 fund on both legs so it is per-fund
// balanced. All values are invented (rule 11).
func configWithCorrections() string {
	base := strings.TrimSuffix(strings.TrimSpace(testConfig()), "}")
	return base + `,
  "corrections": [
    {
      "date": "2025-12-31",
      "subsidiary": "Test US",
      "currency": "USD",
      "memo": "FY cutoff adjustment",
      "description": "in-transit cutoff",
      "splits": [
        {"account": "Checking", "amount": 400000, "fund": "GRANT1"},
        {"account": "Grant Revenue", "amount": -400000, "fund": "GRANT1", "program": "Education"}
      ]
    }
  ]
}`
}

// TestCorrectionPostsBalancedAdjustment proves the config-driven manual adjustment
// primitive (D p26.72): a `corrections` entry posts a balanced correction
// transaction into the built db through the store (versioned, invariant-checked),
// with the exact integer amounts, accounts and fund the config names, and leaves
// ledger.Check Error-clean. Synthetic-only (rule 11).
func TestCorrectionPostsBalancedAdjustment(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	accMap, err := ReadAccountMap(strings.NewReader(testAccountMap()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	cfg, err := ReadConfig(strings.NewReader(configWithCorrections()))
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if len(cfg.Corrections) != 1 {
		t.Fatalf("parsed %d corrections, want 1", len(cfg.Corrections))
	}
	res, err := runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, testRates(), st, false)
	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}

	// The correction posted exactly one transaction, recorded under its synthetic tid.
	txns := res.tidTxns["correction-0"]
	if len(txns) != 1 {
		t.Fatalf("correction produced %d transactions, want 1", len(txns))
	}
	txnID := txns[0]

	// Header: US subsidiary, the config date, memo, USD.
	var subID int64
	var date, memo, currency string
	if err := sqldb.QueryRow(
		`SELECT subsidiary_id, date, memo, currency FROM transactions WHERE id = ?`, txnID,
	).Scan(&subID, &date, &memo, &currency); err != nil {
		t.Fatalf("load correction txn: %v", err)
	}
	if subID != res.SubsidiaryIDs["Test US"] {
		t.Errorf("correction subsidiary = %d, want Test US (%d)", subID, res.SubsidiaryIDs["Test US"])
	}
	if date != "2025-12-31" || memo != "FY cutoff adjustment" || currency != "USD" {
		t.Errorf("correction header = (%q,%q,%q), want (2025-12-31, FY cutoff adjustment, USD)", date, memo, currency)
	}

	// Splits: exactly the two legs, exact integer amounts, on the named accounts,
	// both carrying the GRANT1 fund (so the store's per-fund zero-sum held). The
	// grant-revenue leg carries the Education program.
	rows, err := sqldb.Query(
		`SELECT account_id, amount, fund_id, program_id FROM splits WHERE transaction_id = ? ORDER BY position`, txnID)
	if err != nil {
		t.Fatalf("load correction splits: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type leg struct {
		acct, amount int64
		fund         sql.NullInt64
		prog         sql.NullInt64
	}
	var legs []leg
	for rows.Next() {
		var l leg
		if err := rows.Scan(&l.acct, &l.amount, &l.fund, &l.prog); err != nil {
			t.Fatalf("scan split: %v", err)
		}
		legs = append(legs, l)
	}
	if len(legs) != 2 {
		t.Fatalf("correction has %d splits, want 2", len(legs))
	}
	grant := res.FundIDs["GRANT1"]
	var sum int64
	for _, l := range legs {
		sum += l.amount
		if !l.fund.Valid || l.fund.Int64 != grant {
			t.Errorf("split acct=%d not tagged GRANT1 fund %d (got %v)", l.acct, grant, l.fund)
		}
	}
	if sum != 0 {
		t.Errorf("correction splits net-debit sum = %d, want 0 (balanced)", sum)
	}
	// The Checking (asset) DEBIT leg is +400000; the Grant Revenue (revenue) CREDIT
	// leg is -400000 and carries the Education program.
	byAcct := map[int64]leg{}
	for _, l := range legs {
		byAcct[l.acct] = l
	}
	if l, ok := byAcct[res.AccountIDs["Checking"]]; !ok || l.amount != 400000 {
		t.Errorf("Checking leg = %+v, want amount 400000", l)
	}
	if l, ok := byAcct[res.AccountIDs["Grant Revenue"]]; !ok || l.amount != -400000 {
		t.Errorf("Grant Revenue leg = %+v, want amount -400000", l)
	} else if !l.prog.Valid || l.prog.Int64 != res.ProgramIDs["Education"] {
		t.Errorf("Grant Revenue leg program = %v, want Education %d", l.prog, res.ProgramIDs["Education"])
	}

	// The whole produced db (import + adjustment) is Error-clean.
	vs, err := ledger.Check(context.Background(), sqldb)
	if err != nil {
		t.Fatalf("ledger.Check: %v", err)
	}
	if ledger.HasErrors(vs) {
		t.Fatalf("ledger.Check has errors after correction: %+v", vs)
	}
}

// TestCorrectionRejectsUnbalanced proves a mis-specified (non-zero-sum) correction
// FAILS the build loudly rather than silently plugging or continuing -- an
// adjustment must balance (the store enforces zero-sum on write, rule 7).
func TestCorrectionRejectsUnbalanced(t *testing.T) {
	sqldb := testutil.NewDB(t)
	st := store.New(sqldb)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	accMap, err := ReadAccountMap(strings.NewReader(testAccountMap()))
	if err != nil {
		t.Fatalf("ReadAccountMap: %v", err)
	}
	cfg, err := ReadConfig(strings.NewReader(configWithCorrections()))
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	// Break the balance: make the credit leg smaller than the debit leg.
	cfg.Corrections[0].Splits[1].Amount = -300000
	_, err = runBuild(ctx, strings.NewReader(testSource()), accMap, cfg, testRates(), st, false)
	if err == nil {
		t.Fatal("unbalanced correction was accepted; want a loud build failure")
	}
	if !strings.Contains(err.Error(), "correction 0") {
		t.Errorf("error %q does not name the offending correction", err)
	}
}

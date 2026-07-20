package reports_test

// TDD for p15.2 — the Appendix-E report toolkit. Every expected number below is
// hand-computed from the canonical fixture (PLAN Appendix D, exported in
// internal/testutil/fixture as Expected/Rates) BEFORE the implementation exists:
// the fixture is the oracle, never this test's own output. Conversion numbers are
// derived from the p14.1 rate seam (monthly USD->MXN 17.00->18.10) under the D12
// rule (rate lookup on-or-before the date; round HALF-EVEN at the final aggregate).

import (
	"context"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// find returns the Minor of the CurAmt for currency ccy in amts, and whether it
// was present.
func find(amts []reports.CurAmt, ccy string) (int64, bool) {
	for _, a := range amts {
		if a.Currency == ccy {
			return a.Minor, true
		}
	}
	return 0, false
}

// tkFor builds a toolkit over the fixture scoped to the given subsidiary, target
// currency USD (the fixture's report base).
func tkFor(f *fixture.Fixture, scope ids.SubsidiaryID) *reports.Toolkit {
	return reports.NewToolkit(f.Store, reports.Params{Scope: reports.SubsidiaryID(scope), TargetCurrency: "USD"})
}

// TestBalancesAsOfRootVsLeafScope: consolidation = the scope's descendant closure
// (D18). At ROOT scope every account's balance equals the fixture's ROOT
// AccountBalances oracle. At a LEAF scope (RV México) only MX-mapped accounts
// appear with the SAME native balance (single-sub accounts are scope-invariant),
// and US-only accounts are ABSENT — the discriminating check that scope filters to
// the descendant closure rather than the whole org.
func TestBalancesAsOfRootVsLeafScope(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	none := reports.ConvertOpts{Mode: reports.RateNone}

	root, err := tkFor(f, f.IDs.Root).BalancesAsOf(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf, none)
	if err != nil {
		t.Fatalf("BalancesAsOf root: %v", err)
	}
	// Every ROOT oracle balance must be reproduced exactly.
	for _, ab := range f.Expected.AccountBalances {
		got, ok := find(root[reports.AccountID(ab.Account)], ab.Currency)
		if !ok || got != ab.Amount {
			t.Errorf("root balance acct %d %s = %d/%v, want %d", ab.Account, ab.Currency, got, ok, ab.Amount)
		}
	}

	leaf, err := tkFor(f, f.IDs.MX).BalancesAsOf(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.MX)}, f.Expected.AsOf, none)
	if err != nil {
		t.Fatalf("BalancesAsOf leaf: %v", err)
	}
	// MX-only accounts: identical native balance at leaf scope (scope-invariant).
	mxOnly := map[ids.AccountID]struct {
		ccy    string
		amount int64
	}{
		f.IDs.CheckingMX: {"MXN", 39_500_000},
		f.IDs.CashMXN:    {"MXN", 640_000},
		f.IDs.DueToIntl:  {"USD", -1_000_000},
	}
	for acct, want := range mxOnly {
		got, ok := find(leaf[reports.AccountID(acct)], want.ccy)
		if !ok || got != want.amount {
			t.Errorf("leaf(MX) balance acct %d %s = %d/%v, want %d", acct, want.ccy, got, ok, want.amount)
		}
	}
	// US-only accounts must be ABSENT from the MX-leaf scope (descendant closure of
	// MX does not include US).
	for _, usOnly := range []ids.AccountID{f.IDs.CheckingUS, f.IDs.Building, f.IDs.DueFromMX, f.IDs.CreditCard} {
		if _, ok := leaf[reports.AccountID(usOnly)]; ok {
			t.Errorf("leaf(MX) unexpectedly contains US-only account %d", usOnly)
		}
	}
}

// TestFundBalancesClosingConversion: FundBalancesAsOf with Mode: Closing converts
// each fund's per-currency balance at the AsOf closing rate (D12). The oracle is
// the p14.1 seam's ConvertedFundBalances (MXN funds ÷ 18.10, USD pass-through),
// rounded HALF-EVEN once per output cell. Hand-checked cells:
//
//	BecaAgua MXN 9,700,000 / 18.10 = 535,911.60.. -> 535,912
//	unrestricted MXN 30,440,000 / 18.10 = 1,681,767.95.. -> 1,681,768
//	  (FX Clearing MXN 500,000 is EQUITY-class now, out of the fund-asset sum)
//	all USD funds pass through unchanged.
func TestFundBalancesClosingConversion(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	fb, err := tkFor(f, f.IDs.Root).FundBalancesAsOf(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf,
		reports.ConvertOpts{To: "USD", Mode: reports.RateClosing})
	if err != nil {
		t.Fatalf("FundBalancesAsOf closing: %v", err)
	}

	// A converting result is single-currency (USD): assert per fund the USD cell.
	want := map[reports.FundID]int64{
		f.IDs.BecaAgua:     535_912 + 50_000,       // MXN 9.7M->535,912 plus its USD 50,000 leg
		f.IDs.BuildingFund: 5_000_000,              // USD pass-through
		0:                  1_681_768 + 17_543_500, // unrestricted MXN->USD + USD leg (FX Clearing now equity -> out of fund-asset sum)
	}
	for fund, wantUSD := range want {
		got, ok := find(fb[reports.FundID(fund)], "USD")
		if !ok || got != wantUSD {
			t.Errorf("fund %d converted USD = %d/%v, want %d", fund, got, ok, wantUSD)
		}
	}

	// Cross-check the seam's per-cell converted floats round to the same integers.
	for _, cb := range f.Expected.Rates.ConvertedFundBalances {
		if cb.NativeCcy != "MXN" {
			continue
		}
		wantMinor := reports.RoundHalfEven(cb.ConvertedUSD * 100)
		conv, err := tkFor(f, f.IDs.Root).ConvertMinorAt(ctx, cb.NativeMinor, "MXN", "USD", f.Expected.AsOf)
		if err != nil {
			t.Fatalf("ConvertMinorAt: %v", err)
		}
		if conv != wantMinor {
			t.Errorf("convert fund %d MXN %d -> USD = %d, want %d", cb.Fund, cb.NativeMinor, conv, wantMinor)
		}
	}
}

// TestActivityTxnDate: Activity with Mode: TxnDate converts each month's activity
// at that month's on-or-before rate, accumulating the UNROUNDED sum per output cell
// and rounding HALF-EVEN once (D12 "at final aggregates"). Food Purchases (MXN)
// activity: +120,000 @ 2025-03, +90,000 @ 2025-04, +150,000 @ 2026-03. Rates
// (USD->MXN): 2025-03 = 17.1294117647, 2025-04 = 17.1941176471, 2026-03 =
// 17.9058823529. Converted USD minor (unrounded) =
// 120000/r3 + 90000/r4 + 150000/r36 = 20,616.978.. -> 20,617.
func TestActivityTxnDate(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	act, err := tkFor(f, f.IDs.Root).Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo,
		reports.ConvertOpts{To: "USD", Mode: reports.RateTxnDate})
	if err != nil {
		t.Fatalf("Activity txndate: %v", err)
	}
	got, ok := find(act[reports.AccountID(f.IDs.FoodPurchases)], "USD")
	if !ok || got != 20_617 {
		t.Errorf("FoodPurchases TxnDate USD = %d/%v, want 20617", got, ok)
	}

	// GRAIN LOCK (D12 "round at the final aggregate"): Cash MXN's monthly activity
	// makes the two grains DIVERGE, so this discriminates accumulate-unrounded-then-
	// round-ONCE (correct) from round-per-month-then-sum (wrong). Months (MXN):
	// +1,500,000 @ 2025-01, -120,000 @ 2025-03, -90,000 @ 2025-04, -500,000 @
	// 2025-08, -150,000 @ 2026-03, at rates 17.00/17.1294../17.1941../17.6529../
	// 17.9059.. . Per-month-rounded sums to 38,971; accumulate-then-round-once =
	// 38,970.28.. -> 38,970. The toolkit MUST yield 38,970.
	if m, ok := find(act[reports.AccountID(f.IDs.CashMXN)], "USD"); !ok || m != 38_970 {
		t.Errorf("CashMXN TxnDate USD = %d/%v, want 38970 (accumulate-then-round-once, not 38971)", m, ok)
	}

	// Native (Mode: None) must equal the raw MXN activity oracle (360,000): TxnDate
	// only changes the converted figure, not the underlying tally.
	nat, err := tkFor(f, f.IDs.Root).Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("Activity native: %v", err)
	}
	if m, _ := find(nat[reports.AccountID(f.IDs.FoodPurchases)], "MXN"); m != 360_000 {
		t.Errorf("FoodPurchases native MXN activity = %d, want 360000", m)
	}
}

// TestNetIncomeClosing: NetIncome sums all revenue+expense activity over the window
// and converts to the target at the closing rate (Mode: Closing). Native subtotals
// (from the fixture R/E oracle): USD = -3,567,500; MXN = -9,140,000. MXN->USD at
// 1/18.10 = -504,972.37.. -> -504,972 (half-even). Total USD = -3,567,500 +
// (-504,972) = -4,072,472.
func TestNetIncomeClosing(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	ni, err := tkFor(f, f.IDs.Root).NetIncome(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo,
		reports.ConvertOpts{To: "USD", Mode: reports.RateClosing})
	if err != nil {
		t.Fatalf("NetIncome closing: %v", err)
	}
	if ni.Currency != "USD" || ni.Minor != -4_072_472 {
		t.Errorf("NetIncome closing = %d %s, want -4072472 USD", ni.Minor, ni.Currency)
	}
}

// TestFundBalancesUnrestrictedLine: FundBalancesAsOf includes the unrestricted line
// as fund id 0 (D20). Native (Mode: None): unrestricted MXN 30,440,000 and USD
// 17,543,500 straight from the oracle (FX Clearing is EQUITY-class now, so its
// MXN 500,000 / USD 974,000 are out of the asset-side fund-balance sum).
func TestFundBalancesUnrestrictedLine(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	fb, err := tkFor(f, f.IDs.Root).FundBalancesAsOf(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf,
		reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("FundBalancesAsOf native: %v", err)
	}
	if m, ok := find(fb[0], "MXN"); !ok || m != 30_440_000 {
		t.Errorf("unrestricted MXN = %d/%v, want 30440000", m, ok)
	}
	if u, ok := find(fb[0], "USD"); !ok || u != 17_543_500 {
		t.Errorf("unrestricted USD = %d/%v, want 17543500", u, ok)
	}
}

// TestFunctionalMatrix: FunctionalMatrix returns per (expense account, class,
// currency) activity (D21). Spot-check against the fixture's Functional oracle.
func TestFunctionalMatrix(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	m, err := tkFor(f, f.IDs.Root).FunctionalMatrix(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("FunctionalMatrix: %v", err)
	}
	for _, fc := range f.Expected.Functional {
		got, ok := find(m[reports.AccountID(fc.Account)][reports.Class(fc.Class)], fc.Currency)
		if !ok || got != fc.Amount {
			t.Errorf("matrix[%d][%s] %s = %d/%v, want %d", fc.Account, fc.Class, fc.Currency, got, ok, fc.Amount)
		}
	}
	// Occupancy sits under 'management', never 'program' — cross-class isolation.
	if _, ok := m[reports.AccountID(f.IDs.Occupancy)][reports.Class("program")]; ok {
		t.Errorf("Occupancy leaked into program class")
	}
}

// TestProgramActivity: ProgramActivity rolls (program, account) activity UP the
// program tree (D24). Educación and Food Pantry are LEAF programs (no program
// children), so their cells equal the fixture's raw Program oracle exactly. General
// is the program-tree ROOT, so its cells FOLD IN Educación + Food Pantry activity —
// hand-computed below (e.g. ProgramSupplies USD 60,000 General-raw + 150,000
// Educación = 210,000; Food Purchases MXN 210,000 + 150,000 = 360,000).
func TestProgramActivity(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	pa, err := tkFor(f, f.IDs.Root).ProgramActivity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		f.Expected.ActivityFrom, f.Expected.ActivityTo, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("ProgramActivity: %v", err)
	}
	// Leaf programs: rolled value == raw oracle (no descendants).
	for _, pc := range f.Expected.Program {
		if pc.Program != f.IDs.Educacion && pc.Program != f.IDs.FoodPantry {
			continue
		}
		got, ok := find(pa[reports.ProgramID(pc.Program)][reports.AccountID(pc.Account)], pc.Currency)
		if !ok || got != pc.Amount {
			t.Errorf("leaf program[%d][%d] %s = %d/%v, want %d", pc.Program, pc.Account, pc.Currency, got, ok, pc.Amount)
		}
	}
	// General (root) rollup: hand-computed folded totals for the discriminating cells.
	type gc struct {
		acct ids.AccountID
		ccy  string
		want int64
	}
	generalRolled := []gc{
		{f.IDs.ProgramSupplies, "USD", 210_000}, // 60,000 General + 150,000 Educación
		{f.IDs.ProgramSupplies, "MXN", 500_000}, // only Educación
		{f.IDs.FoodPurchases, "MXN", 360_000},   // 210,000 General + 150,000 Food Pantry
		{f.IDs.GovernmentGrants, "MXN", -10_000_000},
		{f.IDs.Salaries, "USD", 1_650_000},
	}
	for _, g := range generalRolled {
		got, ok := find(pa[reports.ProgramID(f.IDs.General)][reports.AccountID(g.acct)], g.ccy)
		if !ok || got != g.want {
			t.Errorf("General rolled[%d] %s = %d/%v, want %d", g.acct, g.ccy, got, ok, g.want)
		}
	}
}

// TestGroup990Effective: Group990 rolls a leaf (account->amount) map to effective
// 990 codes (D25): own code, else nearest ancestor's; a leaf that OVERRIDES its
// parent's code lands on its OWN line (Bank Fees -> IX.11g, not the Expenses
// parent's IX.24e); accounts with no effective code fall into an explicit Unmapped
// bucket (code ""), rendered LAST. Asserted against the fixture's Rollup990 oracle,
// for Part IX (expense) — built by handing Group990 the expense leaf map.
func TestGroup990Effective(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()

	// Build the Part IX (expense) leaf map in USD from the account balances oracle.
	leafUSD := map[reports.AccountID]int64{
		reports.AccountID(f.IDs.Salaries):        1_650_000,
		reports.AccountID(f.IDs.ProgramSupplies): 210_000,
		reports.AccountID(f.IDs.Occupancy):       305_000,
		reports.AccountID(f.IDs.Insurance):       60_000,
		reports.AccountID(f.IDs.BankFees):        2_500,
		reports.AccountID(f.IDs.EventCosts):      100_000,
	}
	rows, err := tkFor(f, f.IDs.Root).Group990(ctx, "IX", "USD", leafUSD)
	if err != nil {
		t.Fatalf("Group990: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Code] = r.Amount.Minor
	}
	// Effective-code expectations (USD, Part IX):
	//   IX.7   Salaries              1,650,000 (own)
	//   IX.16  Occupancy               305,000 (own)
	//   IX.11g Bank Fees                 2,500 (LEAF OVERRIDE of parent IX.24e)
	//   IX.24e Program Supplies+Insurance+Event Costs = 210,000+60,000+100,000 = 370,000 (inherited)
	want := map[string]int64{"IX.7": 1_650_000, "IX.16": 305_000, "IX.11g": 2_500, "IX.24e": 370_000}
	for code, w := range want {
		if got[code] != w {
			t.Errorf("Group990 %s = %d, want %d", code, got[code], w)
		}
	}
	// Bank Fees must NOT be folded into IX.24e (override lands on its own line).
	if got["IX.24e"] == 370_000+2_500 {
		t.Errorf("Bank Fees leaked into parent IX.24e")
	}
}

// TestGroup990Unmapped: an account with no effective code (Event Income, the
// fixture's deliberately-unmapped R/E leaf, Z19) lands in the explicit Unmapped
// bucket (code ""), rendered as the LAST row — never dropped (D25).
func TestGroup990Unmapped(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	leaf := map[reports.AccountID]int64{
		reports.AccountID(f.IDs.Contributions): -5_275_000, // VIII.1f
		reports.AccountID(f.IDs.EventIncome):   -300_000,   // unmapped
	}
	rows, err := tkFor(f, f.IDs.Root).Group990(ctx, "VIII", "USD", leaf)
	if err != nil {
		t.Fatalf("Group990 VIII: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("Group990 returned no rows")
	}
	last := rows[len(rows)-1]
	if last.Code != "" || last.Amount.Minor != -300_000 {
		t.Errorf("last row = {%q, %d}, want Unmapped bucket {\"\", -300000}", last.Code, last.Amount.Minor)
	}
	if last.Unmapped != true {
		t.Errorf("last row Unmapped flag = false, want true")
	}
}

// TestIntercompanyNetBalanced: on the balanced fixture the intercompany-flagged
// accounts net to zero per currency across a consolidated ROOT scope (D19):
// Due from RV México +1,000,000 USD nets Due to RV Internacional -1,000,000 USD.
// The oracle is Expected.IntercompanyNetCurrencies (["USD"]).
func TestIntercompanyNetBalanced(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	net, err := tkFor(f, f.IDs.Root).IntercompanyNet(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf)
	if err != nil {
		t.Fatalf("IntercompanyNet balanced: %v", err)
	}
	for _, ccy := range f.Expected.IntercompanyNetCurrencies {
		if m, _ := find(net, ccy); m != 0 {
			t.Errorf("balanced intercompany net %s = %d, want 0", ccy, m)
		}
	}
}

// TestIntercompanyNetCorrupted: post ONE extra balanced transaction that debits the
// intercompany Due-from account against cash WITHOUT a mirroring Due-to credit in
// the other subsidiary. Each transaction stays zero-sum (never write an unbalanced
// txn), but the CROSS-subsidiary intercompany property breaks: the USD net becomes
// nonzero (+250,000), which IntercompanyNet reports (a nonzero residual -> the
// caller renders a D19 warning row).
func TestIntercompanyNetCorrupted(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// A single balanced US txn: DR Due from RV México +2,500.00, CR Checking US
	// -2,500.00. Balanced within the txn and within the (nil) fund, but adds an
	// unmatched intercompany debit -> the consolidated intercompany net is no longer
	// zero.
	_, err := f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date:         "2026-06-15",
		SubsidiaryID: f.IDs.US,
		Currency:     "USD",
		Memo:         "unmatched intercompany advance (corruption)",
		Splits: []store.SplitInput{
			{AccountID: f.IDs.DueFromMX, Amount: 250_000},
			{AccountID: f.IDs.CheckingUS, Amount: -250_000},
		},
	})
	if err != nil {
		t.Fatalf("post corrupting txn: %v", err)
	}

	net, err := tkFor(f, f.IDs.Root).IntercompanyNet(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf)
	if err != nil {
		t.Fatalf("IntercompanyNet corrupted: %v", err)
	}
	m, ok := find(net, "USD")
	if !ok || m != 250_000 {
		t.Errorf("corrupted intercompany net USD = %d/%v, want 250000", m, ok)
	}
}

// TestFundPeriodStatementPreWindowCapitalExcluded (p26.94 regression) proves the
// capital classification scans the fund's CUMULATIVE position up to the window end,
// not just the in-window rows. The fixture's Building Fund capitalizes the Building
// asset on 2025-06-15 (Dr Building 40,000 / Cr CheckingUS). For a window that STARTS
// AFTER that date (2025-07-01..2025-12-31), the capitalization is a PRE-window row:
// the Building account carries a 40,000 opening debit and CheckingUS carries the
// remaining 10,000 spendable gift. If PASS 1 only looked at in-window rows, Building
// would be misclassified as "cash" and its 40,000 debit would inflate spendable
// Opening (and thus Closing) to 50,000; the fix classifies Building as capital from
// the cumulative history, so spendable Opening/Closing exclude it (== 10,000).
func TestFundPeriodStatementPreWindowCapitalExcluded(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()

	st, err := reports.NewToolkit(f.Store, reports.Params{}).
		FundPeriodStatement(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.IDs.BuildingFund, "2025-07-01", "2025-12-31")
	if err != nil {
		t.Fatalf("fund statement: %v", err)
	}
	// Building was capitalized BEFORE the window; it must be a capital account even
	// though no in-window row touches it.
	if !st.CapitalAccounts[reports.AccountID(f.IDs.Building)] {
		t.Errorf("Building not classified as capital (pre-window capitalization missed)")
	}
	// Spendable Opening excludes the 40,000 pre-window Building debit: only the
	// remaining CheckingUS gift (5,000,000 gift - 4,000,000 capitalized = 1,000,000).
	if st.Opening["USD"] != 1_000_000 {
		t.Errorf("USD opening = %d, want 1000000 (pre-window capital excluded)", st.Opening["USD"])
	}
	// No in-window activity, so Closing == Opening; still excludes the capital asset.
	if st.Closing["USD"] != 1_000_000 {
		t.Errorf("USD closing = %d, want 1000000 (pre-window capital excluded)", st.Closing["USD"])
	}
}

// TestIntercompanyResidualSplitSameCurrency: a USD (same-as-target) intercompany
// residual has NO translation component -- valued at the closing rate and at the
// transaction-date rate it is identical (USD->USD is 1:1 at every date), so the split's
// Closing == Historical and the CTA (Closing − Historical) is zero. This is the p26.70
// invariant that a same-currency imbalance stays entirely in the genuine-imbalance
// (Historical) component, with no spurious translation adjustment.
func TestIntercompanyResidualSplitSameCurrency(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// A USD unmirrored intercompany advance (+250,000 USD residual, single currency).
	_, err := f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date:         "2026-06-15",
		SubsidiaryID: f.IDs.US,
		Currency:     "USD",
		Memo:         "unmatched USD intercompany advance",
		Splits: []store.SplitInput{
			{AccountID: f.IDs.DueFromMX, Amount: 250_000},
			{AccountID: f.IDs.CheckingUS, Amount: -250_000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	split, err := tkFor(f, f.IDs.Root).IntercompanyResidualSplit(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf, "USD")
	if err != nil {
		t.Fatalf("IntercompanyResidualSplit: %v", err)
	}
	if split.Closing != 250_000 {
		t.Errorf("Closing = %d, want 250000 (USD residual @ closing 1:1)", split.Closing)
	}
	if split.Historical != 250_000 {
		t.Errorf("Historical = %d, want 250000 (USD 1:1 at every date -> no translation)", split.Historical)
	}
	if split.Closing-split.Historical != 0 {
		t.Errorf("CTA (Closing-Historical) = %d, want 0 for a same-currency residual", split.Closing-split.Historical)
	}
}

// TestIntercompanyResidualSplitTranslation: a FOREIGN-currency (MXN) intercompany
// residual DOES carry a translation component -- valued at the period-end CLOSING rate
// it differs from its value at the transaction-date (HISTORICAL) rate, so Closing !=
// Historical and the CTA is nonzero. The historical leg is the amount actually funded
// (Feb-2025 rate); the closing leg retranslates the same MXN position at the AsOf rate.
func TestIntercompanyResidualSplitTranslation(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// A foreign (MXN) unmirrored intercompany advance, dated early so the txn-date rate
	// differs from the closing rate: DueToIntl (IC-flagged liability) +1,000,000 MXN.
	_, err := f.Store.PostTransaction(ctx, store.PostTransactionInput{
		Date:         "2025-02-15",
		SubsidiaryID: f.IDs.MX,
		Currency:     "MXN",
		Memo:         "unmatched MXN intercompany advance",
		Splits: []store.SplitInput{
			{AccountID: f.IDs.DueToIntl, Amount: 1_000_000},
			{AccountID: f.IDs.CashMXN, Amount: -1_000_000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	tk := tkFor(f, f.IDs.Root)
	split, err := tk.IntercompanyResidualSplit(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, f.Expected.AsOf, "USD")
	if err != nil {
		t.Fatalf("IntercompanyResidualSplit: %v", err)
	}
	// Closing leg == ConvertMinorAt of the native residual at the AsOf rate (the oracle).
	wantClosing, err := tk.ConvertMinorAt(ctx, 1_000_000, "MXN", "USD", f.Expected.AsOf)
	if err != nil {
		t.Fatalf("convert closing: %v", err)
	}
	if split.Closing != wantClosing {
		t.Errorf("Closing = %d, want %d (native residual @ AsOf closing rate)", split.Closing, wantClosing)
	}
	// Historical leg == the same MXN converted at its transaction-DATE (Feb 2025) rate.
	wantHist, err := tk.ConvertMinorAt(ctx, 1_000_000, "MXN", "USD", "2025-02-28")
	if err != nil {
		t.Fatalf("convert historical: %v", err)
	}
	if split.Historical != wantHist {
		t.Errorf("Historical = %d, want %d (residual @ txn-date rate)", split.Historical, wantHist)
	}
	if split.Closing == split.Historical {
		t.Errorf("no translation component: Closing == Historical == %d (rates should differ across the period)", split.Closing)
	}
}

// TestActivityExcludesIntercompanyConsolidated: an intercompany-flagged R/E account
// (an intra-group transfer, D19) is DROPPED from Activity at a CONSOLIDATED (root)
// scope but KEPT at a leaf/single-sub scope (there it is the entity's real line).
func TestActivityExcludesIntercompanyConsolidated(t *testing.T) {
	f := fixture.New(t)
	ctx := store.WithActor(context.Background(), store.Actor{ID: 1})

	// Flag an expense account that carries activity as intercompany.
	yes := true
	if err := f.Store.UpdateAccount(ctx, f.IDs.Salaries, store.UpdateAccountInput{Intercompany: &yes}); err != nil {
		t.Fatalf("flag intercompany: %v", err)
	}
	from, to := f.Expected.ActivityFrom, f.Expected.ActivityTo

	// Consolidated root scope: the flagged account is excluded.
	rootAct, err := tkFor(f, f.IDs.Root).Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)}, from, to, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("root Activity: %v", err)
	}
	if _, ok := rootAct[reports.AccountID(f.IDs.Salaries)]; ok {
		t.Error("intercompany account NOT excluded at consolidated root scope")
	}

	// Leaf scopes: the account is kept where it has activity (not consolidated).
	usAct, err := tkFor(f, f.IDs.US).Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.US)}, from, to, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("US Activity: %v", err)
	}
	mxAct, err := tkFor(f, f.IDs.MX).Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.MX)}, from, to, reports.ConvertOpts{Mode: reports.RateNone})
	if err != nil {
		t.Fatalf("MX Activity: %v", err)
	}
	_, inUS := usAct[reports.AccountID(f.IDs.Salaries)]
	_, inMX := mxAct[reports.AccountID(f.IDs.Salaries)]
	if !inUS && !inMX {
		t.Error("intercompany account excluded at a LEAF scope (should only drop when consolidated)")
	}
}

// TestRollupTreeOrder: Rollup emits a placeholder subtotal row for each placeholder
// (parent) account in TREE ORDER, its subtotal = the sum of its subtree's leaf
// amounts, plus the leaf rows themselves. Feeding the expense leaf balances, the
// Expenses placeholder subtotal = the sum of all expense leaves. Salaries 1,650,000
// + Program Supplies 210,000 + Occupancy 305,000 + Insurance 60,000 + Bank Fees
// 2,500 + Event Costs 100,000 = 2,327,500 (USD).
func TestRollupTreeOrder(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	leaf := map[reports.AccountID]int64{
		reports.AccountID(f.IDs.Salaries):        1_650_000,
		reports.AccountID(f.IDs.ProgramSupplies): 210_000,
		reports.AccountID(f.IDs.Occupancy):       305_000,
		reports.AccountID(f.IDs.Insurance):       60_000,
		reports.AccountID(f.IDs.BankFees):        2_500,
		reports.AccountID(f.IDs.EventCosts):      100_000,
	}
	rows, err := tkFor(f, f.IDs.Root).Rollup(ctx, "USD", leaf)
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	// Find the Expenses placeholder subtotal row.
	var expensesSubtotal int64
	var sawExpenses bool
	for _, r := range rows {
		if r.AccountID == reports.AccountID(f.IDs.Expenses) {
			expensesSubtotal = r.Amount.Minor
			sawExpenses = r.Subtotal
		}
	}
	if !sawExpenses {
		t.Fatal("Rollup did not emit an Expenses placeholder subtotal row")
	}
	if expensesSubtotal != 2_327_500 {
		t.Errorf("Expenses subtotal = %d, want 2327500", expensesSubtotal)
	}

	// Tree order: the Expenses placeholder row precedes each of its leaf children's
	// rows (pre-order).
	pos := map[reports.AccountID]int{}
	for i, r := range rows {
		pos[r.AccountID] = i
	}
	if pos[reports.AccountID(f.IDs.Expenses)] > pos[reports.AccountID(f.IDs.Salaries)] {
		t.Errorf("Expenses placeholder not before its Salaries child in tree order")
	}
}

// TestRoundHalfEven: the D12 rounding primitive rounds ties to even and is
// symmetric about zero (guards the conversion path's grain).
func TestRoundHalfEven(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0.5, 0},
		{1.5, 2},
		{2.5, 2},
		{-0.5, 0},
		{-1.5, -2},
		{-2.5, -2},
		{2.4, 2},
		{2.6, 3},
		{-2.6, -3},
	}
	for _, c := range cases {
		if got := reports.RoundHalfEven(c.in); got != c.want {
			t.Errorf("RoundHalfEven(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
